package ratelimit

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// Rollbacker is an optional capability a Reservation may also implement.
// All discovers it with a type assertion; the core Reservation interface is
// frozen (README "Stability contract (v1)").
//
// Rollback undoes a reservation whose request was never sent: All calls it,
// instead of Release, on each reservation it already holds when a later
// limiter refuses. That lets a limiter tell "never sent" apart from "sent and
// failed", which Release without Commit also signals. A budget limiter can
// refund a rolled-back reservation in full, where it would charge a released
// one.
//
// Rollback must free everything Release would. All calls it at most once per
// reservation, on a context that survives cancellation of the request, and
// never calls Commit or Release on that reservation afterwards. A reservation
// without Rollback is released instead, which is the conservative fallback
// (the limiter keeps its charge).
type Rollbacker interface {
	Rollback(ctx context.Context) error
}

// errNoLimiters and errNilLimiter are All's configuration errors.
var (
	errNoLimiters = errors.New("ratelimit: All needs at least one limiter")
	errNilLimiter = errors.New("ratelimit: All got a nil limiter")
)

// All composes limiters into one Limiter that admits a request only when every
// limiter admits it. It returns an error, and no limiter, when given no
// limiters or a nil one.
//
// Reserve reserves on each limiter in the order given and stops at the first
// refusal. It then rolls back the reservations already held, in reverse
// order: by Rollback when the reservation implements Rollbacker, otherwise by
// Release. Rollback errors are discarded, like Release errors in the rate
// limit middleware, and the refusing limiter's error is returned unwrapped,
// so errors.As keeps resolving *llms.LimitError and the retry and circuit
// middleware classify it exactly as they would the limiter alone.
//
// Order matters: put a limiter that waits (a rate limiter) before one that
// holds a scarce resource (a budget), so the wait happens before the budget is
// held and a rate-limit refusal never touches the budget.
//
// Each limiter receives the same ReservationRequest. The returned
// Reservation commits the same usage to every reservation and releases them
// in reverse order, joining their errors. It also implements Rollbacker, so
// an All nests inside another All.
func All(limiters ...Limiter) (Limiter, error) {
	if len(limiters) == 0 {
		return nil, errNoLimiters
	}
	for _, l := range limiters {
		if l == nil {
			return nil, errNilLimiter
		}
	}
	return &allLimiter{limiters: append([]Limiter(nil), limiters...)}, nil
}

type allLimiter struct {
	limiters []Limiter
}

// Reserve implements Limiter.
func (a *allLimiter) Reserve(ctx context.Context, req ReservationRequest) (Reservation, error) {
	held := make([]Reservation, 0, len(a.limiters))
	for _, l := range a.limiters {
		r, err := l.Reserve(ctx, req)
		if err != nil {
			_ = rollbackAll(context.WithoutCancel(ctx), held)
			return nil, err //nolint:wrapcheck // pass limiter errors (incl. *llms.LimitError) through unwrapped
		}
		held = append(held, r)
	}
	return &allReservation{held: held}, nil
}

// rollbackAll rolls back held in reverse order, by Rollback where supported
// and Release otherwise, joining the errors.
func rollbackAll(ctx context.Context, held []Reservation) error {
	var errs []error
	for i := len(held) - 1; i >= 0; i-- {
		if err := rollbackOne(ctx, held[i]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...) //nolint:wrapcheck // members name their limiters
}

func rollbackOne(ctx context.Context, r Reservation) error {
	if rb, ok := r.(Rollbacker); ok {
		return rb.Rollback(ctx) //nolint:wrapcheck // the limiter names itself
	}
	return r.Release(ctx) //nolint:wrapcheck // the limiter names itself
}

// allReservation holds one reservation per composed limiter, in limiter order.
type allReservation struct {
	held []Reservation
	// ended is set by the first Release or Rollback; later calls to either
	// are no-ops, so Release stays idempotent and a rolled-back reservation
	// is never released as well.
	ended atomic.Bool
}

// Commit implements Reservation: it commits usage to every reservation and
// joins their errors.
func (r *allReservation) Commit(ctx context.Context, usage llms.Usage) error {
	var errs []error
	for _, h := range r.held {
		if err := h.Commit(ctx, usage); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...) //nolint:wrapcheck // members name their limiters
}

// Release implements Reservation: it releases every reservation in reverse
// order and joins their errors. Repeated Release, and Release after Rollback,
// are no-ops.
func (r *allReservation) Release(ctx context.Context) error {
	if !r.ended.CompareAndSwap(false, true) {
		return nil
	}
	var errs []error
	for i := len(r.held) - 1; i >= 0; i-- {
		if err := r.held[i].Release(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...) //nolint:wrapcheck // members name their limiters
}

// Rollback implements Rollbacker: it rolls back every reservation in reverse
// order, so a nested All passes "never sent" through to its own limiters.
// Rollback after Release, and repeated Rollback, are no-ops.
func (r *allReservation) Rollback(ctx context.Context) error {
	if !r.ended.CompareAndSwap(false, true) {
		return nil
	}
	return rollbackAll(ctx, r.held)
}
