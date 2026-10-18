package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
	"github.com/SnapdragonPartners/maestro-llms/bucket/internal/core"
)

// lease is one held slot: a bucket.lease row renewed in the background
// until Release, loss, or MaxLeaseDuration.
type lease struct {
	acquired   time.Time // local monotonic clock, for the MaxLeaseDuration cap
	releaseErr error
	l          *Limiter
	cancel     context.CancelFunc
	done       chan struct{} // closed when the renewal goroutine exits
	lost       chan struct{} // closed when the lease is detected lost
	key        string
	id         string
	bucketID   int64
	tokens     int // the estimate debited at acquire, when tokenCharged
	lostOnce   sync.Once
	release    sync.Once
	mu         sync.Mutex
	adjusted   bool
	// tokenCharged records whether acquire actually debited tokens (the
	// bucket was token-limited then). Adjust reconciles only a charge that
	// happened, so a limit change in between can't refund tokens that were
	// never taken.
	tokenCharged bool
}

// newLease starts renewal for a freshly inserted lease row. The renewal
// context keeps acquireCtx's values but not its cancellation: the request
// ending must not stop renewal while the slot is still held. Release stops
// it.
func (l *Limiter) newLease(acquireCtx context.Context, key, id string, bucketID int64, tokens int, tokenCharged bool) *lease {
	ctx, cancel := context.WithCancel(context.WithoutCancel(acquireCtx))
	r := &lease{
		acquired:     time.Now(),
		l:            l,
		cancel:       cancel,
		done:         make(chan struct{}),
		lost:         make(chan struct{}),
		key:          key,
		id:           id,
		bucketID:     bucketID,
		tokens:       tokens,
		tokenCharged: tokenCharged,
	}
	go r.renew(ctx)
	return r
}

func (r *lease) markLost() { r.lostOnce.Do(func() { close(r.lost) }) }

// Lost implements bucket.Lease.
func (r *lease) Lost() <-chan struct{} { return r.lost }

// renew extends the lease every LeaseTTL/3 until ctx is canceled, the lease
// is found lost, or MaxLeaseDuration has passed since acquire. A failed
// renewal (database unreachable, timeout) is transient: the next tick
// retries, and TTL/3 leaves two retries before expiry. The database caps
// expires_at at acquired_at + MaxLeaseDuration as well, so the cap holds
// even if this goroutine's clock misbehaves.
//
// Reaching MaxLeaseDuration marks the lease lost: the database stops
// extending it at that point, so the slot is (or is about to be) expired
// and may be reassigned. acquired is stamped after the insert committed,
// so this fires no earlier than the database-side cap.
func (r *lease) renew(ctx context.Context) {
	defer close(r.done)
	cfg := r.l.cfg
	interval := cfg.LeaseTTL / 3
	t := time.NewTicker(interval)
	defer t.Stop()
	hardCap := time.NewTimer(cfg.MaxLeaseDuration - time.Since(r.acquired))
	defer hardCap.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hardCap.C:
			r.markLost()
			return
		case <-t.C:
		}
		lost, err := r.renewOnce(ctx, min(cfg.OpTimeout, interval))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		if lost {
			r.markLost()
			return
		}
	}
}

// renewOnce runs one renewal under timeout. It reports lost when no live
// row matched: the lease expired or was removed (reaped, or cascaded from a
// deleted bucket).
func (r *lease) renewOnce(ctx context.Context, timeout time.Duration) (lost bool, err error) {
	opCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cfg := r.l.cfg
	res, err := r.l.db.ExecContext(opCtx, `
UPDATE bucket.lease
SET expires_at = LEAST(clock_timestamp() + $2::float8 * interval '1 microsecond',
                       acquired_at + $3::float8 * interval '1 microsecond')
WHERE id = $1 AND expires_at > clock_timestamp()`,
		r.id, micros(cfg.LeaseTTL), micros(cfg.MaxLeaseDuration))
	if err != nil {
		return false, fmt.Errorf("bucket %q: renew: %w", r.key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("bucket %q: renew: %w", r.key, err)
	}
	return n == 0, nil
}

// Adjust implements bucket.Lease. It settles refill under the current
// limits, then charges or refunds the difference. It is a no-op for a lease
// acquired while the bucket had no token limit (nothing was debited, so
// there is nothing to reconcile), and when the bucket has no token limit
// now. It runs on a
// cancellation-surviving context bounded by OpTimeout: the call it
// reconciles already happened.
func (r *lease) Adjust(ctx context.Context, actualTokens int) error {
	r.mu.Lock()
	if r.adjusted {
		r.mu.Unlock()
		return nil
	}
	r.adjusted = true
	r.mu.Unlock()
	if !r.tokenCharged {
		return nil
	}
	actualTokens = max(actualTokens, 0)
	return r.l.inTx(context.WithoutCancel(ctx), func(ctx context.Context, tx *sql.Tx) error {
		b, now, err := lockBucketByID(ctx, tx, r.key, r.bucketID)
		if err != nil {
			return err
		}
		if !b.limits.TokenLimited() {
			return nil
		}
		balance := core.Settle(b.limits, b.balance, b.lastRefill, now)
		balance = core.Adjusted(b.limits, balance, r.tokens, actualTokens)
		if _, err := tx.ExecContext(ctx,
			`UPDATE bucket.bucket SET token_balance = $1, last_refill = $2 WHERE id = $3`, balance, now, b.id); err != nil {
			return fmt.Errorf("bucket %q: adjust: %w", r.key, schemaErr(err))
		}
		return nil
	})
}

// Release implements bucket.Lease. It stops renewal, then deletes the row
// on a cancellation-surviving context bounded by OpTimeout. It returns
// bucket.ErrLeaseLost when the row was already gone or had expired. On a
// database error the slot is reclaimed one LeaseTTL later.
func (r *lease) Release(ctx context.Context) error {
	r.release.Do(func() {
		r.cancel()
		<-r.done
		opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.l.cfg.OpTimeout)
		defer cancel()
		var live bool
		err := r.l.db.QueryRowContext(opCtx,
			`DELETE FROM bucket.lease WHERE id = $1 RETURNING expires_at > clock_timestamp()`, r.id).Scan(&live)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			live = false
		case err != nil:
			r.releaseErr = fmt.Errorf("bucket %q: release: %w", r.key, schemaErr(err))
			return
		}
		if !live {
			r.markLost()
			r.releaseErr = bucket.ErrLeaseLost
		}
	})
	return r.releaseErr
}
