package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
)

func TestSettle(t *testing.T) {
	l := bucket.Limits{TokensPerMinute: 60, BufferFactor: 1} // 1 token/s, capacity 60
	t0 := time.Unix(0, 0)
	if got := Settle(l, 10, t0, t0.Add(5*time.Second)); got != 15 {
		t.Errorf("refill = %v, want 15", got)
	}
	if got := Settle(l, 50, t0, t0.Add(time.Minute)); got != 60 {
		t.Errorf("capped = %v, want 60", got)
	}
	if got := Settle(l, -30, t0, t0.Add(10*time.Second)); got != -20 {
		t.Errorf("debt repaid = %v, want -20", got)
	}
	if got := Settle(l, 10, t0, t0.Add(-time.Second)); got != 10 {
		t.Errorf("clock went back = %v, want 10", got)
	}
	if got := Settle(bucket.Limits{}, 7, t0, t0.Add(time.Hour)); got != 7 {
		t.Errorf("unlimited = %v, want unchanged", got)
	}
}

func TestAdjusted(t *testing.T) {
	l := bucket.Limits{TokensPerMinute: 60, BufferFactor: 1}
	if got := Adjusted(l, 10, 30, 20); got != 20 {
		t.Errorf("refund = %v, want 20", got)
	}
	if got := Adjusted(l, 10, 30, 50); got != -10 {
		t.Errorf("charge = %v, want -10", got)
	}
	if got := Adjusted(l, 55, 30, 0); got != 60 {
		t.Errorf("refund capped = %v, want 60", got)
	}
}

func TestRetryAfterAndFits(t *testing.T) {
	l := bucket.Limits{TokensPerMinute: 60, MaxConcurrency: 2, BufferFactor: 1}
	if got := RetryAfter(l, 0, 10); got != 10*time.Second {
		t.Errorf("RetryAfter = %v, want 10s", got)
	}
	if got := RetryAfter(l, 20, 10); got != 0 {
		t.Errorf("RetryAfter fits = %v, want 0", got)
	}
	if !Fits(l, 10, 10, 1) || Fits(l, 9, 10, 1) || Fits(l, 10, 10, 2) {
		t.Error("Fits wrong")
	}
	if Impossible(l, 61) == "" || Impossible(l, 60) != "" {
		t.Error("Impossible wrong")
	}
}

func TestWaitAdmits(t *testing.T) {
	calls := 0
	want := fakeLease{}
	got, err := Wait(context.Background(), "k", Waiter{Initial: time.Millisecond, Max: time.Millisecond},
		func(context.Context) (Result, error) {
			calls++
			if calls < 3 {
				return Result{}, nil
			}
			return Result{Lease: want}, nil
		})
	if err != nil || got != want || calls != 3 {
		t.Fatalf("Wait = %v, %v after %d calls", got, err, calls)
	}
}

func TestWaitImpossibleAndBackendError(t *testing.T) {
	_, err := Wait(context.Background(), "k", Waiter{Initial: time.Millisecond, Max: time.Millisecond},
		func(context.Context) (Result, error) { return Result{Impossible: "too big"}, nil })
	var le *bucket.LimitError
	if !errors.As(err, &le) || le.Reason != "too big" {
		t.Fatalf("impossible: %v", err)
	}
	boom := errors.New("db down")
	_, err = Wait(context.Background(), "k", Waiter{Initial: time.Millisecond, Max: time.Millisecond},
		func(context.Context) (Result, error) { return Result{}, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("backend error: %v", err)
	}
}

func TestWaitMaxWait(t *testing.T) {
	start := time.Now()
	_, err := Wait(context.Background(), "k", Waiter{MaxWait: 30 * time.Millisecond, Initial: 5 * time.Millisecond, Max: 10 * time.Millisecond},
		func(context.Context) (Result, error) { return Result{RetryAfter: time.Hour}, nil })
	var le *bucket.LimitError
	if !errors.As(err, &le) || le.RetryAfter != time.Hour || !strings.Contains(le.Reason, "MaxWait") {
		t.Fatalf("MaxWait: %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("MaxWait overslept: %v", el)
	}
}

// TestWaitMaxWaitBoundsABlockedAttempt: an attempt that blocks (a database
// lock wait) sees MaxWait through its context and cannot admit late.
func TestWaitMaxWaitBoundsABlockedAttempt(t *testing.T) {
	start := time.Now()
	_, err := Wait(context.Background(), "k", Waiter{MaxWait: 50 * time.Millisecond, Initial: time.Millisecond, Max: time.Millisecond},
		func(ctx context.Context) (Result, error) {
			select {
			case <-ctx.Done():
				return Result{}, ctx.Err()
			case <-time.After(2 * time.Second):
				return Result{Lease: fakeLease{}}, nil // would admit late
			}
		})
	var le *bucket.LimitError
	if !errors.As(err, &le) || !strings.Contains(le.Reason, "MaxWait") {
		t.Fatalf("blocked attempt: %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("blocked attempt ran %v past MaxWait", el)
	}
}

func TestWaitContextCanceled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := Wait(ctx, "k", Waiter{Initial: 5 * time.Millisecond, Max: 5 * time.Millisecond},
		func(context.Context) (Result, error) { return Result{}, nil })
	var le *bucket.LimitError
	if !errors.As(err, &le) {
		t.Fatalf("ctx done: %v", err)
	}
	// A backend error caused by the caller's cancellation is reported as an
	// ended wait, not a backend failure.
	ctx2, cancel2 := context.WithCancel(context.Background())
	_, err = Wait(ctx2, "k", Waiter{Initial: time.Millisecond, Max: time.Millisecond},
		func(ctx context.Context) (Result, error) { cancel2(); return Result{}, ctx.Err() })
	if !errors.As(err, &le) {
		t.Fatalf("ctx canceled mid-try: %v", err)
	}
}

func TestJitterBounds(t *testing.T) {
	for range 1000 {
		if j := jitter(100); j < 50 || j > 100 {
			t.Fatalf("jitter(100) = %d", j)
		}
	}
	if jitter(0) != 1 {
		t.Fatal("jitter(0) must be positive")
	}
}

type fakeLease struct{}

func (fakeLease) Adjust(context.Context, int) error { return nil }
func (fakeLease) Release(context.Context) error     { return nil }
func (fakeLease) Lost() <-chan struct{}             { return nil }
