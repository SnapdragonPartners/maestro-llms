// Package core holds the token arithmetic and the wait loop shared by the
// bucket backends, so every backend accounts and waits identically.
package core

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
)

// Settle returns the token balance after refilling from lastRefill to now
// under l, capped at l.Capacity(). A negative balance (debt) is repaid by
// refill like any other. With token limiting off the balance is unchanged.
func Settle(l bucket.Limits, balance float64, lastRefill, now time.Time) float64 {
	if !l.TokenLimited() {
		return balance
	}
	if elapsed := now.Sub(lastRefill).Seconds(); elapsed > 0 {
		balance += elapsed * l.RefillPerSecond()
	}
	return min(balance, float64(l.Capacity()))
}

// Adjusted returns the balance after reconciling actual tokens against the
// charged ones: a surplus is refunded (capped at capacity) and a shortfall
// charged (the balance may go negative). The caller settles refill first.
func Adjusted(l bucket.Limits, balance float64, charged, actual int) float64 {
	if !l.TokenLimited() {
		return balance
	}
	balance -= float64(actual - charged)
	return min(balance, float64(l.Capacity()))
}

// RetryAfter estimates how long until balance refills to tokens. Zero when
// the tokens already fit or token limiting is off.
func RetryAfter(l bucket.Limits, balance float64, tokens int) time.Duration {
	if !l.TokenLimited() {
		return 0
	}
	deficit := float64(tokens) - balance
	if deficit <= 0 {
		return 0
	}
	ns := deficit / l.RefillPerSecond() * float64(time.Second)
	if ns >= maxDurationNS { // a larger float-to-int conversion is undefined
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(ns)
}

// maxDurationNS is float64(math.MaxInt64), which rounds up to 2^63: a
// RetryAfter at or past it saturates (ADR-0034).
const maxDurationNS = float64(math.MaxInt64)

// Impossible returns a non-empty reason when a request for tokens can never
// be admitted under l.
func Impossible(l bucket.Limits, tokens int) string {
	if l.TokenLimited() && tokens > l.Capacity() {
		return fmt.Sprintf("%d tokens exceed the bucket capacity of %d; the request can never be admitted", tokens, l.Capacity())
	}
	return ""
}

// Fits reports whether tokens and one more slot fit, given the settled
// balance and the number of live leases.
func Fits(l bucket.Limits, balance float64, tokens, live int) bool {
	hasTokens := !l.TokenLimited() || balance >= float64(tokens)
	hasSlot := !l.SlotLimited() || live < l.MaxConcurrency
	return hasTokens && hasSlot
}

// Result is one admission attempt's outcome. Exactly one of Lease or
// Impossible is set when admitted or never admissible; neither is set when
// the caller should wait and retry.
type Result struct {
	Lease      bucket.Lease
	Impossible string
	RetryAfter time.Duration
}

// Waiter is the polling policy: back off with jitter from Initial to Max,
// never sleeping past the RetryAfter hint. MaxWait bounds the whole wait in
// real time, including an attempt that is itself blocked (for example on a
// database lock).
type Waiter struct {
	MaxWait time.Duration // 0 = until ctx is done
	Initial time.Duration
	Max     time.Duration
}

// Wait runs try until it admits, reports the request impossible, fails, or
// the wait ends. MaxWait is a hard deadline on the whole call. Each try
// receives a context that ends when the caller's ctx does or MaxWait
// elapses, and a backend must check it after taking any lock and before
// charging capacity, so no attempt can admit work after the budget is
// spent. A try error is a backend failure and is returned as is,
// unless that context is done, in which case the ended wait is reported as
// a *bucket.LimitError.
func Wait(ctx context.Context, key string, w Waiter, try func(context.Context) (Result, error)) (bucket.Lease, error) {
	waitCtx := ctx
	if w.MaxWait > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, w.MaxWait)
		defer cancel()
	}
	ended := func(when string, retryAfter time.Duration) *bucket.LimitError {
		// The ctx passed in, not waitCtx: only that context ending the
		// wait sets Cause. MaxWait alone is a temporary rejection.
		if cause := ctx.Err(); cause != nil {
			return &bucket.LimitError{Key: key, Reason: "context done " + when, RetryAfter: retryAfter, Cause: cause}
		}
		return &bucket.LimitError{Key: key, Reason: "wait exceeded MaxWait " + when, RetryAfter: retryAfter}
	}
	backoff := w.Initial
	var retryAfter time.Duration
	for {
		if waitCtx.Err() != nil {
			return nil, ended("before capacity was available", retryAfter)
		}
		res, err := try(waitCtx)
		if err != nil {
			if waitCtx.Err() != nil {
				return nil, ended("while acquiring", retryAfter)
			}
			return nil, err
		}
		if res.Lease != nil {
			return res.Lease, nil
		}
		if res.Impossible != "" {
			return nil, &bucket.LimitError{Key: key, Reason: res.Impossible, Permanent: true}
		}
		retryAfter = res.RetryAfter
		sleep := jitter(backoff)
		if retryAfter > 0 {
			sleep = min(sleep, retryAfter)
		}
		t := time.NewTimer(sleep)
		select {
		case <-waitCtx.Done():
			t.Stop()
			return nil, ended("while waiting for capacity", retryAfter)
		case <-t.C:
		}
		backoff = min(backoff*2, w.Max)
	}
}

// jitter returns a duration in [d/2, d], spreading pollers so they don't
// retry in lockstep.
func jitter(d time.Duration) time.Duration {
	if d <= 1 {
		return max(d, 1)
	}
	half := d / 2
	return half + rand.N(d-half+1) //nolint:gosec // jitter, not security
}
