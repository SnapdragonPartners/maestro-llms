package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
	"github.com/SnapdragonPartners/maestro-llms/bucket/internal/core"
)

// Leases held by ID (ADR-0052). A detached lease is a bucket.lease row with
// detached = true that records its own terms: the tokens charged at acquire
// (tokens, token_charged), whether it has been adjusted, its renewal TTL and
// its hard deadline (max_expires_at). Any Limiter on the database can renew,
// adjust or release it from those columns alone, whatever its own Config.
// Nothing renews it in the background: the holder calls RenewLease.
//
// Every by-ID operation takes the key the caller has authorized and matches
// the lease against that bucket's current name as well as its ID. An ID that
// is unknown, malformed, an in-process lease's, or a different bucket's
// gives the same result as a lease that is gone, bucket.ErrLeaseLost, and
// changes nothing. A lease stays bound to its bucket's stable id, so after a
// rename callers pass the new name, and a new bucket that takes the old name
// never gains an earlier lease.

// validityCols returns a detached row's terms in microseconds: its TTL, the
// time until expiry on the database clock now, and the gap from expiry to
// the hard cap. The clock is read once, after the statement wrote the row,
// so the times err short, and CapIn (expiry plus gap) never falls below
// ExpiresIn as two readings could make it.
const validityCols = `
floor(extract(epoch FROM ttl) * 1000000)::bigint,
floor(extract(epoch FROM expires_at - clock_timestamp()) * 1000000)::bigint,
floor(extract(epoch FROM max_expires_at - expires_at) * 1000000)::bigint`

// scanValidity scans validityCols into a bucket.Validity, clamping the
// remaining times at zero.
func scanValidity(scan func(...any) error, extra ...any) (bucket.Validity, error) {
	var ttl, exp, gap int64
	if err := scan(append(extra, &ttl, &exp, &gap)...); err != nil {
		return bucket.Validity{}, err
	}
	exp = max(exp, 0)
	return bucket.Validity{
		TTL:       time.Duration(ttl) * time.Microsecond,
		ExpiresIn: time.Duration(exp) * time.Microsecond,
		CapIn:     time.Duration(exp+max(gap, 0)) * time.Microsecond,
	}, nil
}

// TryAcquireDetached makes one admission attempt on key for tokens and one
// concurrency slot, with Acquire's admission rules, and on success inserts a
// detached lease that only RenewLease extends. It does not wait: when the
// request doesn't fit now it returns a temporary *bucket.LimitError with a
// RetryAfter hint (zero when waiting on a slot), and a Permanent one when it
// can never fit. A holder that wants Acquire's waiting runs it on its own
// side (bucket/remote does). A key with no configured limits returns an
// error wrapping bucket.ErrUnknownKey; any other error is a database failure
// and no lease is held. The attempt is bounded by OpTimeout.
//
// The lease's TTL and hard cap come from this Limiter's LeaseTTL and
// MaxLeaseDuration and are stored on the row, so a renewal on any instance
// enforces these, not its own.
func (l *Limiter) TryAcquireDetached(ctx context.Context, key string, tokens int) (bucket.DetachedLease, error) {
	tokens = max(tokens, 0)
	var refusal *core.Result
	var dl bucket.DetachedLease
	err := l.inTx(ctx, func(ctx context.Context, tx Tx) error {
		r, refused, err := admit(ctx, tx, key, tokens)
		if err != nil || refused != nil {
			refusal = refused
			return err
		}
		// One clock reading stamps the row, so the TTL and the cap are
		// measured from the same instant.
		row := tx.QueryRow(ctx, `
WITH t AS (SELECT clock_timestamp() AS now)
INSERT INTO bucket.lease (bucket_id, acquired_at, expires_at, detached, tokens, token_charged, ttl, max_expires_at)
SELECT $1, t.now, t.now + $2::float8 * interval '1 microsecond', true, $3, $4,
       $2::float8 * interval '1 microsecond', t.now + $5::float8 * interval '1 microsecond'
FROM t
RETURNING id, `+validityCols,
			r.id, micros(l.cfg.LeaseTTL), tokens, r.limits.TokenLimited(), micros(l.cfg.MaxLeaseDuration))
		v, err := scanValidity(row.Scan, &dl.ID)
		if err != nil {
			return fmt.Errorf("bucket %q: insert lease: %w", key, schemaErr(err))
		}
		dl.Validity = v
		return nil
	})
	switch {
	case err != nil:
		if cause := ctx.Err(); cause != nil {
			return bucket.DetachedLease{}, &bucket.LimitError{Key: key, Reason: "context done while acquiring", Cause: cause}
		}
		return bucket.DetachedLease{}, err
	case refusal != nil && refusal.Impossible != "":
		return bucket.DetachedLease{}, &bucket.LimitError{Key: key, Reason: refusal.Impossible, Permanent: true}
	case refusal != nil:
		return bucket.DetachedLease{}, &bucket.LimitError{Key: key, Reason: "no capacity now", RetryAfter: refusal.RetryAfter}
	}
	return dl, nil
}

// RenewLease extends the live detached lease id on key by its own TTL,
// capped at its own hard deadline, both fixed at acquire. It returns the
// lease's validity after the renewal, measured on the database clock. Once
// the cap is reached a renewal succeeds without extending (ExpiresIn equals
// CapIn); after it, or for a lease that expired, was released or is unknown,
// it returns bucket.ErrLeaseLost. Any other error is a database failure; the
// lease keeps its previous expiry. Bounded by OpTimeout and ctx.
//
// It locks the lease row first and checks liveness on a clock read after
// the lock: a renewal that waited on the lock (behind AdjustLease, say)
// past the expiry must find the lease lost, and PostgreSQL does not
// re-evaluate a predicate checked before the wait when the holder only
// locked the row.
func (l *Limiter) RenewLease(ctx context.Context, key, id string) (bucket.Validity, error) {
	if !validLeaseID(id) {
		return bucket.Validity{}, bucket.ErrLeaseLost
	}
	var v bucket.Validity
	err := l.inTx(ctx, func(ctx context.Context, tx Tx) error {
		var expiresAt time.Time
		err := tx.QueryRow(ctx, `
SELECT l.expires_at FROM bucket.lease AS l JOIN bucket.bucket AS b ON b.id = l.bucket_id
WHERE l.id = $1 AND l.detached AND b.name = $2
FOR UPDATE OF l`, id, key).Scan(&expiresAt)
		if errors.Is(err, sql.ErrNoRows) {
			return bucket.ErrLeaseLost
		}
		if err != nil {
			return fmt.Errorf("bucket %q: renew: %w", key, schemaErr(err))
		}
		var now time.Time
		if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return fmt.Errorf("bucket %q: clock: %w", key, err)
		}
		if !expiresAt.After(now) {
			return bucket.ErrLeaseLost
		}
		row := tx.QueryRow(ctx, `
UPDATE bucket.lease SET expires_at = LEAST($2::timestamptz + ttl, max_expires_at)
WHERE id = $1
RETURNING `+validityCols, id, now)
		if v, err = scanValidity(row.Scan); err != nil {
			return fmt.Errorf("bucket %q: renew: %w", key, schemaErr(err))
		}
		return nil
	})
	if err != nil {
		return bucket.Validity{}, err
	}
	return v, nil
}

// AdjustLease reconciles actualTokens against the tokens the detached lease
// id on key was charged at acquire, like Lease.Adjust: it settles refill
// under the current limits, then refunds or charges the difference. It is a
// no-op on the balance when nothing was charged at acquire or the bucket has
// no token limit now.
//
// Only a live lease is adjusted. Liveness is checked on the database clock
// after the bucket and lease rows are locked, and marking the lease adjusted
// commits with the balance change, so the balance changes at most once
// however often the call is repeated. A repeat returns nil while the lease
// is live. After the lease expired or was released it returns
// bucket.ErrLeaseLost, even when an earlier call committed and its response
// was lost, and the estimate charged at acquire stands.
//
// Like Lease.Adjust it runs on a cancellation-surviving context bounded by
// OpTimeout. It does not free the slot.
func (l *Limiter) AdjustLease(ctx context.Context, key, id string, actualTokens int) error {
	if !validLeaseID(id) {
		return bucket.ErrLeaseLost
	}
	actualTokens = max(actualTokens, 0)
	return l.inTx(context.WithoutCancel(ctx), func(ctx context.Context, tx Tx) error {
		// Bucket before lease: the lock order acquire's reap uses.
		b, _, err := lockBucket(ctx, tx, key)
		if errors.Is(err, bucket.ErrUnknownKey) {
			return bucket.ErrLeaseLost
		}
		if err != nil {
			return err
		}
		var tokens int64
		var charged, adjusted bool
		var expiresAt time.Time
		err = tx.QueryRow(ctx, `
SELECT tokens, token_charged, adjusted, expires_at FROM bucket.lease
WHERE id = $1 AND detached AND bucket_id = $2 FOR UPDATE`, id, b.id).Scan(&tokens, &charged, &adjusted, &expiresAt)
		if errors.Is(err, sql.ErrNoRows) {
			return bucket.ErrLeaseLost
		}
		if err != nil {
			return fmt.Errorf("bucket %q: adjust: %w", key, schemaErr(err))
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return fmt.Errorf("bucket %q: clock: %w", key, err)
		}
		if !expiresAt.After(now) {
			return bucket.ErrLeaseLost
		}
		if adjusted {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE bucket.lease SET adjusted = true WHERE id = $1`, id); err != nil {
			return fmt.Errorf("bucket %q: adjust: %w", key, schemaErr(err))
		}
		if !charged || !b.limits.TokenLimited() {
			return nil
		}
		balance := core.Settle(b.limits, b.balance, b.lastRefill, now)
		balance = core.Adjusted(b.limits, balance, int(tokens), actualTokens)
		if _, err := tx.Exec(ctx,
			`UPDATE bucket.bucket SET token_balance = $1, last_refill = $2 WHERE id = $3`, balance, now, b.id); err != nil {
			return fmt.Errorf("bucket %q: adjust: %w", key, schemaErr(err))
		}
		return nil
	})
}

// ReleaseLease deletes the detached lease id on key, freeing its slot, on a
// cancellation-surviving context bounded by OpTimeout. It returns
// bucket.ErrLeaseLost when the lease had already expired or was gone, so a
// repeated release reports ErrLeaseLost even when the first one succeeded.
// On a database error the slot frees when the lease expires, within one TTL.
func (l *Limiter) ReleaseLease(ctx context.Context, key, id string) error {
	if !validLeaseID(id) {
		return bucket.ErrLeaseLost
	}
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.cfg.OpTimeout)
	defer cancel()
	var live bool
	err := l.db.QueryRowContext(opCtx, `
DELETE FROM bucket.lease AS l USING bucket.bucket AS b
WHERE l.id = $1 AND l.detached AND b.id = l.bucket_id AND b.name = $2
RETURNING l.expires_at > clock_timestamp()`, id, key).Scan(&live)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return bucket.ErrLeaseLost
	case err != nil:
		return fmt.Errorf("bucket %q: release: %w", key, schemaErr(err))
	case !live:
		return bucket.ErrLeaseLost
	}
	return nil
}

// validLeaseID reports whether id is a UUID in the canonical 8-4-4-4-12 form
// the database hands out. Anything else can't name a lease; rejecting it
// here keeps a malformed ID from reaching the database as a type error.
func validLeaseID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := range len(id) {
		c := id[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return false
			}
		}
	}
	return true
}
