// Package bucket is a generic per-key token-rate and concurrency limiter for
// any API with token or concurrency limits (ADR-0025).
//
// A caller acquires a lease on a key for an estimated number of tokens and
// one concurrency slot, adjusts the token charge once the actual count is
// known, and always releases the lease. Backends live in subpackages:
// bucket/memory (process-local) and bucket/postgres (shared across
// processes). bucket/remote is a Limiter for a process that holds leases
// through another process's API (ADR-0052); DetachedLease and Validity are
// the by-ID form of a lease it and bucket/postgres exchange.
//
// This package must not import the llms packages; the dependency runs the
// other way (llms/ratelimit.FromBucket adapts a Limiter to the LLM
// reservation protocol).
package bucket

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// DefaultBufferFactor scales token capacity below the nominal per-minute rate
// to absorb estimate inaccuracy (overestimation is the safe direction).
const DefaultBufferFactor = 0.9

// Limiter admits work against per-key limits.
type Limiter interface {
	// Acquire blocks until key has capacity for tokens and one concurrency
	// slot, or until ctx is done or the backend's MaxWait elapses. A request
	// that is not admitted returns a *LimitError; any other error is a
	// backend failure, and no lease is held (fail closed). A key with no
	// configured limits returns an error wrapping ErrUnknownKey.
	Acquire(ctx context.Context, key string, tokens int) (Lease, error)
}

// Lease is one admitted unit of work. Callers must always call Release.
type Lease interface {
	// Adjust reconciles the actual token count against the acquired one:
	// it refunds a surplus and charges a shortfall (the balance may go
	// negative and is repaid by refill). Only the first call has an effect.
	// It does not free the slot.
	Adjust(ctx context.Context, actualTokens int) error
	// Release frees the slot. It is idempotent, safe after Adjust, and safe
	// on a context with no deadline or one already canceled: backends bound
	// it internally. It returns ErrLeaseLost when the backend found the
	// lease already gone.
	Release(ctx context.Context) error
	// Lost is closed when the backend detects that the lease expired or was
	// removed while held, so another holder may now have the slot. Callers
	// that want to abort work can select on it. It never closes for
	// backends that cannot lose a lease.
	Lost() <-chan struct{}
}

// DetachedLease is a lease held by its ID rather than by a Lease value, so a
// process other than the one that acquired it can renew, adjust and release
// it (ADR-0052). Nothing renews it on its own: the holder renews it every
// TTL/3 or it expires. The ID is a bearer secret: together with the key it
// is all an operation on the lease needs.
type DetachedLease struct {
	ID string
	Validity
}

// Validity is how long a detached lease stays live, as durations measured
// on the backend's clock when the call that returned them ran. A holder that
// measures them from when it sent the request stays on the safe side of the
// backend, whatever the clocks and the network delay.
type Validity struct {
	// TTL is the renewal window fixed when the lease was acquired: renew
	// every TTL/3.
	TTL time.Duration
	// ExpiresIn is how long until the lease expires unless it is renewed.
	// It never exceeds CapIn.
	ExpiresIn time.Duration
	// CapIn is how long until the lease's hard cap (the backend's maximum
	// lease duration, fixed at acquire). No renewal extends it past that.
	CapIn time.Duration
}

// Limits are what every user of a key must agree on. Zero values mean
// unlimited for that dimension. Caller policy such as how long to wait is a
// backend option, not part of Limits.
type Limits struct {
	// TokensPerMinute is the refill rate. Quotas expressed over other
	// windows must be converted by the app (see ADR-0025 on bursts).
	TokensPerMinute int
	// MaxConcurrency caps leases held at once.
	MaxConcurrency int
	// BufferFactor scales capacity: Capacity = TokensPerMinute *
	// BufferFactor. Zero means DefaultBufferFactor.
	BufferFactor float64
}

// Validate reports whether the limits are usable: no negative limit, a
// finite non-negative BufferFactor, and a Capacity that fits in an int
// (ADR-0034). Without it, a negative limit silently disables its dimension
// and a NaN, infinite or overflowing capacity is undefined.
func (l Limits) Validate() error {
	switch {
	case l.TokensPerMinute < 0:
		return fmt.Errorf("bucket: TokensPerMinute %d is negative", l.TokensPerMinute)
	case l.MaxConcurrency < 0:
		return fmt.Errorf("bucket: MaxConcurrency %d is negative", l.MaxConcurrency)
	case math.IsNaN(l.BufferFactor) || math.IsInf(l.BufferFactor, 0):
		return fmt.Errorf("bucket: BufferFactor %v is not finite", l.BufferFactor)
	case l.BufferFactor < 0:
		return fmt.Errorf("bucket: BufferFactor %v is negative", l.BufferFactor)
	case float64(l.TokensPerMinute)*l.EffectiveBufferFactor() >= maxCapacity:
		return fmt.Errorf("bucket: capacity TokensPerMinute %d * BufferFactor %v overflows int",
			l.TokensPerMinute, l.EffectiveBufferFactor())
	}
	return nil
}

// maxCapacity bounds TokensPerMinute * BufferFactor: float64(math.MaxInt)
// rounds up to 2^63, and converting a float at or above it to int is
// undefined.
const maxCapacity = float64(math.MaxInt)

// EffectiveBufferFactor is BufferFactor, or DefaultBufferFactor when unset.
func (l Limits) EffectiveBufferFactor() float64 {
	if l.BufferFactor <= 0 {
		return DefaultBufferFactor
	}
	return l.BufferFactor
}

// Capacity is the maximum token balance: TokensPerMinute * BufferFactor,
// floored at 1 when token limiting is on (so a tiny rate never truncates to a
// disabled bucket), and 0 when it is off. It is defined only for limits that
// pass Validate.
func (l Limits) Capacity() int {
	if l.TokensPerMinute <= 0 {
		return 0
	}
	return max(1, int(float64(l.TokensPerMinute)*l.EffectiveBufferFactor()))
}

// TokenLimited reports whether the token dimension is limited.
func (l Limits) TokenLimited() bool { return l.TokensPerMinute > 0 }

// SlotLimited reports whether the concurrency dimension is limited.
func (l Limits) SlotLimited() bool { return l.MaxConcurrency > 0 }

// RefillPerSecond is the continuous refill rate in tokens per second.
func (l Limits) RefillPerSecond() float64 { return float64(l.TokensPerMinute) / 60.0 }

// LimitError means a request was not admitted: the wait timed out or was
// canceled, or the request can never fit. It is distinct from a backend
// failure.
type LimitError struct {
	// Cause is the context error (context.Canceled or
	// context.DeadlineExceeded) when the context passed to Acquire ended
	// the wait; nil when MaxWait ended it or the request is Permanent.
	// Unwrap exposes it.
	Cause  error
	Key    string
	Reason string
	// RetryAfter is a hint for when capacity may be available; zero when
	// the backend has no estimate (for example, waiting on a slot).
	RetryAfter time.Duration
	// Permanent reports that the request can never fit under the key's
	// current limits, so retrying it is pointless without changing the
	// request or the limits.
	Permanent bool
}

// Error implements error.
func (e *LimitError) Error() string {
	return fmt.Sprintf("bucket %q: not admitted: %s", e.Key, e.Reason)
}

// Unwrap exposes Cause for errors.Is/errors.As.
func (e *LimitError) Unwrap() error { return e.Cause }

// ErrUnknownKey is wrapped by errors for a key with no configured limits.
var ErrUnknownKey = errors.New("bucket: unknown key")

// ErrLeaseLost is returned by Release when the lease had already expired or
// been removed, so the slot may have been given to someone else.
var ErrLeaseLost = errors.New("bucket: lease lost")

// UnknownKey returns an error wrapping ErrUnknownKey that names key.
func UnknownKey(key string) error {
	return fmt.Errorf("%w %q", ErrUnknownKey, key)
}
