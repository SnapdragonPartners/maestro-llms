package ratelimit

import (
	"context"
	"errors"
	"fmt"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// FromBucket adapts a generic bucket.Limiter (ADR-0025) to the reservation
// protocol, so the rate-limit middleware runs unchanged over any bucket
// backend, including the shared Postgres one. key is the app's policy: it
// maps each request to a bucket key (for example one key per upstream
// endpoint, or provider plus model). key must not be nil.
//
// Reserve acquires EstimatedUnits.Tokens() on key(req); Commit adjusts to the
// same effective-token count InMemoryLimiter uses; Release releases. A
// *bucket.LimitError becomes a *llms.LimitError with the request's Provider
// and Model, keeping Permanent and Cause, so retry and circuit middleware
// treat it like any local rejection (ADR-0033). A bucket lease is exactly one slot: a reservation with
// EstimatedUnits.Slots() > 1 is rejected before acquiring, with a
// non-retryable error.
func FromBucket(l bucket.Limiter, key func(ReservationRequest) string) Limiter {
	if l == nil || key == nil {
		panic("ratelimit.FromBucket: nil limiter or key func")
	}
	return &bucketLimiter{l: l, key: key}
}

type bucketLimiter struct {
	l   bucket.Limiter
	key func(ReservationRequest) string
}

// Reserve implements Limiter.
func (b *bucketLimiter) Reserve(ctx context.Context, req ReservationRequest) (Reservation, error) {
	if slots := req.EstimatedUnits.Slots(); slots > 1 {
		return nil, fmt.Errorf("ratelimit: bucket leases hold one slot; reservation asks for %d", slots)
	}
	lease, err := b.l.Acquire(ctx, b.key(req), req.EstimatedUnits.Tokens())
	if err != nil {
		var le *bucket.LimitError
		if errors.As(err, &le) {
			return nil, &llms.LimitError{
				Provider:   req.Provider,
				Model:      req.Model,
				Reason:     fmt.Sprintf("bucket %q: %s", le.Key, le.Reason),
				RetryAfter: le.RetryAfter,
				Permanent:  le.Permanent,
				Cause:      le.Cause,
			}
		}
		return nil, fmt.Errorf("ratelimit: bucket: %w", err)
	}
	return &bucketReservation{lease: lease}, nil
}

type bucketReservation struct {
	lease bucket.Lease
}

// Commit implements Reservation.
func (r *bucketReservation) Commit(ctx context.Context, usage llms.Usage) error {
	return r.lease.Adjust(ctx, effectiveTokens(usage)) //nolint:wrapcheck // the backend names the bucket
}

// Release implements Reservation.
func (r *bucketReservation) Release(ctx context.Context) error {
	return r.lease.Release(ctx) //nolint:wrapcheck // keep errors.Is(err, bucket.ErrLeaseLost) direct
}
