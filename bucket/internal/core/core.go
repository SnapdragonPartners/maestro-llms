// Package core holds the token arithmetic and the wait loop shared by the
// bucket backends, so every backend accounts and waits identically.
package core

import (
	"context"
	"fmt"
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
	return time.Duration(deficit / l.RefillPerSecond() * float64(time.Second))
}

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
// never sleeping past the RetryAfter hint or the remaining MaxWait.
type Waiter struct {
	// Now measures MaxWait; defaults to time.Now.
	Now     func() time.Time
	MaxWait time.Duration // 0 = until ctx is done
	Initial time.Duration
	Max     time.Duration
}

// Wait runs try until it admits, reports the request impossible, fails, or
// the wait ends. A try error is a backend failure and is returned as is,
// unless ctx is done, in which case the caller's cancellation is reported
// as a *bucket.LimitError like any other ended wait.
func Wait(ctx context.Context, key string, w Waiter, try func(context.Context) (Result, error)) (bucket.Lease, error) {
	now := w.Now
	if now == nil {
		now = time.Now
	}
	start := now()
	backoff := w.Initial
	for {
		if ctx.Err() != nil {
			return nil, &bucket.LimitError{Key: key, Reason: "context done before capacity was available"}
		}
		res, err := try(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, &bucket.LimitError{Key: key, Reason: "context done while acquiring"}
			}
			return nil, err
		}
		if res.Lease != nil {
			return res.Lease, nil
		}
		if res.Impossible != "" {
			return nil, &bucket.LimitError{Key: key, Reason: res.Impossible}
		}

		sleep := jitter(backoff)
		if res.RetryAfter > 0 {
			sleep = min(sleep, res.RetryAfter)
		}
		if w.MaxWait > 0 {
			remaining := w.MaxWait - now().Sub(start)
			if remaining <= 0 {
				return nil, &bucket.LimitError{Key: key, Reason: "wait exceeded MaxWait", RetryAfter: res.RetryAfter}
			}
			sleep = min(sleep, remaining)
		}
		t := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, &bucket.LimitError{Key: key, Reason: "context done while waiting for capacity", RetryAfter: res.RetryAfter}
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
