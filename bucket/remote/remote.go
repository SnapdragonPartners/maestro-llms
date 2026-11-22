// Package remote is a bucket.Limiter for a process that holds leases through
// another process rather than a backend of its own (ADR-0052). For example,
// a worker with no database credentials takes capacity from a shared
// bucket/postgres limiter through an internal API on a service that has
// them.
//
// The app supplies the transport as a Backend: four stateless calls by key
// and lease ID, which the serving side passes straight through to
// postgres.Limiter's TryAcquireDetached, RenewLease, AdjustLease and
// ReleaseLease (*postgres.Limiter is itself a Backend). This package runs the
// holder's side: Acquire's waiting and backoff with a hard MaxWait, the
// renewal loop, Lost and release-once, so a host doesn't rewrite them.
//
// Every failure fails closed: a refusal, an unreachable backend or a
// response that arrives too late returns an error from Acquire and no lease,
// so no work starts.
package remote

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
	"github.com/SnapdragonPartners/maestro-llms/bucket/internal/core"
)

// Backend reaches a limiter that holds detached leases, usually over the
// app's own API. Each call is stateless: any serving instance may answer it.
// The serving side authorizes the key; the lease ID is a bearer secret.
type Backend interface {
	// TryAcquireDetached makes one admission attempt. A request that
	// doesn't fit returns a *bucket.LimitError: temporary (Permanent false,
	// Cause nil, RetryAfter a hint) to be retried, or Permanent when it can
	// never fit. Any other error, including a transport failure whose
	// outcome is unknown, ends the acquire: it is never replayed, because
	// the backend may have admitted the request and lost the response.
	TryAcquireDetached(ctx context.Context, key string, tokens int) (bucket.DetachedLease, error)
	// RenewLease extends the lease. bucket.ErrLeaseLost means it is gone;
	// any other error is transient and the next renewal retries.
	RenewLease(ctx context.Context, key, id string) (bucket.Validity, error)
	// AdjustLease reconciles the actual token count. The backend must apply
	// it at most once, so a retry after a lost response is harmless.
	AdjustLease(ctx context.Context, key, id string, actualTokens int) error
	// ReleaseLease frees the slot. bucket.ErrLeaseLost means the lease was
	// already gone.
	ReleaseLease(ctx context.Context, key, id string) error
}

// Defaults for Config.
const (
	DefaultOpTimeout       = 5 * time.Second
	DefaultPollInterval    = 100 * time.Millisecond
	DefaultMaxPollInterval = 2 * time.Second
)

// Config configures a Limiter. The zero value uses every default.
type Config struct {
	// MaxWait bounds how long Acquire blocks, across every backend call;
	// 0 waits until ctx is done. A lease granted after it has passed is
	// released and not returned.
	MaxWait time.Duration
	// PollInterval is the first re-check delay after a temporary refusal;
	// it doubles up to MaxPollInterval, never sleeping past the refusal's
	// RetryAfter.
	PollInterval    time.Duration
	MaxPollInterval time.Duration
	// OpTimeout bounds each renew, adjust and release call. A renewal is
	// also bounded by the renewal interval and the lease's remaining
	// validity.
	OpTimeout time.Duration
}

func (c Config) withDefaults() Config {
	if c.OpTimeout <= 0 {
		c.OpTimeout = DefaultOpTimeout
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.MaxPollInterval <= 0 {
		c.MaxPollInterval = DefaultMaxPollInterval
	}
	c.MaxPollInterval = max(c.MaxPollInterval, c.PollInterval)
	return c
}

// Limiter is a bucket.Limiter over a Backend. It is safe for concurrent use.
type Limiter struct {
	b   Backend
	cfg Config
}

var _ bucket.Limiter = (*Limiter)(nil)

// New builds a Limiter over b.
func New(b Backend, cfg Config) (*Limiter, error) {
	if b == nil {
		return nil, errors.New("remote: nil Backend")
	}
	return &Limiter{b: b, cfg: cfg.withDefaults()}, nil
}

// Acquire implements bucket.Limiter with the semantics of the backend's own
// Acquire: it retries temporary refusals with jittered backoff until
// admitted, the request is Permanent, ctx is done or MaxWait passes, all as
// a *bucket.LimitError. Any other backend error is returned after one
// attempt, and no lease is held.
//
// A lease's validity is measured from when its request was sent, so a slow
// response only shortens it. A grant that arrives after that validity ran
// out or after MaxWait is released (best effort) and not returned, so the
// caller never starts work on a lease that may already be gone.
func (l *Limiter) Acquire(ctx context.Context, key string, tokens int) (bucket.Lease, error) {
	tokens = max(tokens, 0)
	w := core.Waiter{MaxWait: l.cfg.MaxWait, Initial: l.cfg.PollInterval, Max: l.cfg.MaxPollInterval}
	return core.Wait(ctx, key, w, func(waitCtx context.Context) (core.Result, error) {
		return l.try(ctx, waitCtx, key, tokens)
	})
}

// try is one acquire attempt. waitCtx ends with ctx or at MaxWait; ctx's
// values (not its cancellation) carry into the renewal loop.
func (l *Limiter) try(ctx, waitCtx context.Context, key string, tokens int) (core.Result, error) {
	sent := time.Now()
	dl, err := l.b.TryAcquireDetached(waitCtx, key, tokens)
	if err != nil {
		var le *bucket.LimitError
		if errors.As(err, &le) && le.Cause == nil {
			if le.Permanent {
				return core.Result{Impossible: le.Reason}, nil
			}
			return core.Result{RetryAfter: le.RetryAfter}, nil
		}
		return core.Result{}, err //nolint:wrapcheck // the backend's error, as is
	}
	if dl.ID == "" || dl.TTL/3 <= 0 {
		if dl.ID != "" {
			l.releaseQuietly(ctx, key, dl.ID)
		}
		return core.Result{}, fmt.Errorf("bucket %q: remote: invalid grant (ID %q, TTL %v)", key, dl.ID, dl.TTL)
	}
	deadline := sent.Add(dl.ExpiresIn)
	if waitCtx.Err() != nil || !time.Now().Before(deadline) {
		l.releaseQuietly(ctx, key, dl.ID)
		if err := waitCtx.Err(); err != nil {
			return core.Result{}, err //nolint:wrapcheck // Wait reports the ended wait as a *LimitError
		}
		return core.Result{}, fmt.Errorf("bucket %q: remote: the grant arrived after its validity (%v) ran out", key, dl.ExpiresIn)
	}
	return core.Result{Lease: l.newLease(ctx, key, dl.ID, dl.TTL/3, deadline, sent.Add(dl.CapIn))}, nil
}

// releaseQuietly releases a grant the caller will never see. Failing is
// harmless: nothing renews it, so it expires within one TTL.
func (l *Limiter) releaseQuietly(ctx context.Context, key, id string) {
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.cfg.OpTimeout)
	defer cancel()
	_ = l.b.ReleaseLease(opCtx, key, id)
}

// lease is one held detached lease, renewed until Release, loss, the end of
// its confirmed validity, or its hard cap.
type lease struct {
	releaseErr  error
	l           *Limiter
	cancel      context.CancelFunc
	done        chan struct{} // closed when the renewal goroutine exits
	lost        chan struct{}
	key         string
	id          string
	interval    time.Duration
	lostOnce    sync.Once
	releaseOnce sync.Once
	mu          sync.Mutex
	adjusted    bool
}

func (l *Limiter) newLease(acquireCtx context.Context, key, id string, interval time.Duration, deadline, hardCap time.Time) *lease {
	ctx, cancel := context.WithCancel(context.WithoutCancel(acquireCtx))
	r := &lease{
		l:        l,
		cancel:   cancel,
		done:     make(chan struct{}),
		lost:     make(chan struct{}),
		key:      key,
		id:       id,
		interval: interval,
	}
	go r.renew(ctx, deadline, hardCap)
	return r
}

func (r *lease) markLost() { r.lostOnce.Do(func() { close(r.lost) }) }

// Lost implements bucket.Lease. It closes when a renewal reports the lease
// gone, when the last confirmed validity runs out without a successful
// renewal (for example while the backend is unreachable), or at the hard
// cap.
func (r *lease) Lost() <-chan struct{} { return r.lost }

// renew renews every interval. deadline is the last confirmed validity and
// hardCap the lease's cap, both on the local clock and measured from when
// the confirming request was sent, so they never run past the backend's.
func (r *lease) renew(ctx context.Context, deadline, hardCap time.Time) {
	defer close(r.done)
	deadline = minTime(deadline, hardCap)
	tick := time.NewTicker(r.interval)
	defer tick.Stop()
	expiry := time.NewTimer(time.Until(deadline))
	defer expiry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-expiry.C:
			r.markLost()
			return
		case <-tick.C:
		}
		sent := time.Now()
		opCtx, cancel := context.WithTimeout(ctx, min(r.l.cfg.OpTimeout, r.interval, time.Until(deadline)))
		v, err := r.l.b.RenewLease(opCtx, r.key, r.id)
		cancel()
		switch {
		case errors.Is(err, bucket.ErrLeaseLost):
			r.markLost()
			return
		case err != nil:
			// Transient: the next tick retries, and expiry fires if the
			// validity runs out first.
			continue
		}
		hardCap = minTime(hardCap, sent.Add(v.CapIn))
		deadline = minTime(sent.Add(v.ExpiresIn), hardCap)
		expiry.Reset(time.Until(deadline))
	}
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// Adjust implements bucket.Lease. It sends the adjustment on a
// cancellation-surviving context bounded by OpTimeout. Once the backend has
// answered (nil, or bucket.ErrLeaseLost) later calls are no-ops; after a
// transport error a later call sends it again, which the backend applies at
// most once. bucket.ErrLeaseLost also closes Lost.
func (r *lease) Adjust(ctx context.Context, actualTokens int) error {
	r.mu.Lock()
	done := r.adjusted
	r.mu.Unlock()
	if done {
		return nil
	}
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.l.cfg.OpTimeout)
	defer cancel()
	err := r.l.b.AdjustLease(opCtx, r.key, r.id, max(actualTokens, 0))
	if err == nil || errors.Is(err, bucket.ErrLeaseLost) {
		r.mu.Lock()
		r.adjusted = true
		r.mu.Unlock()
	}
	if errors.Is(err, bucket.ErrLeaseLost) {
		r.markLost()
	}
	return err //nolint:wrapcheck // keep errors.Is(err, bucket.ErrLeaseLost) direct
}

// Release implements bucket.Lease. It stops renewal, then releases the lease
// once on a cancellation-surviving context bounded by OpTimeout, and returns
// that result on every call. bucket.ErrLeaseLost means the backend found the
// lease already gone. On a transport error the slot frees when the lease
// expires, within one TTL, since nothing renews it any more.
func (r *lease) Release(ctx context.Context) error {
	r.releaseOnce.Do(func() {
		r.cancel()
		<-r.done
		opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.l.cfg.OpTimeout)
		defer cancel()
		err := r.l.b.ReleaseLease(opCtx, r.key, r.id)
		if errors.Is(err, bucket.ErrLeaseLost) {
			r.markLost()
			err = bucket.ErrLeaseLost
		}
		r.releaseErr = err
	})
	return r.releaseErr
}
