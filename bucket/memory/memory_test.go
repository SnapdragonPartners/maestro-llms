package memory

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
)

var _ bucket.Limiter = (*Limiter)(nil)

type fakeClock struct {
	t  time.Time
	mu sync.Mutex
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func mustNew(t *testing.T, cfg Config) *Limiter {
	t.Helper()
	l, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestConcurrencyCap(t *testing.T) {
	l := mustNew(t, Config{Limits: map[string]bucket.Limits{"api": {MaxConcurrency: 2}}, PollInterval: time.Millisecond})
	var inFlight, peak atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			lease, err := l.Acquire(context.Background(), "api", 0)
			if err != nil {
				t.Error(err)
				return
			}
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			inFlight.Add(-1)
			_ = lease.Release(context.Background())
		})
	}
	wg.Wait()
	if p := peak.Load(); p > 2 || p == 0 {
		t.Fatalf("peak in flight = %d, want 1..2", p)
	}
}

func TestTokenRefillAndRetryAfter(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	l := mustNew(t, Config{
		Limits:  map[string]bucket.Limits{"api": {TokensPerMinute: 60, BufferFactor: 1}},
		Clock:   clk.now,
		MaxWait: 20 * time.Millisecond,
	})
	ctx := context.Background()
	lease, err := l.Acquire(ctx, "api", 60)
	if err != nil {
		t.Fatal(err)
	}
	_ = lease.Release(ctx)
	_, err = l.Acquire(ctx, "api", 10)
	var le *bucket.LimitError
	if !errors.As(err, &le) || le.RetryAfter != 10*time.Second || le.Permanent || le.Cause != nil {
		t.Fatalf("empty bucket: want a temporary LimitError, got %#v", err)
	}
	clk.advance(10 * time.Second)
	if _, err := l.Acquire(ctx, "api", 10); err != nil {
		t.Fatalf("after refill: %v", err)
	}
}

func TestAdjust(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	l := mustNew(t, Config{
		Limits:  map[string]bucket.Limits{"api": {TokensPerMinute: 60, BufferFactor: 1}},
		Clock:   clk.now,
		MaxWait: 20 * time.Millisecond,
	})
	ctx := context.Background()
	lease, _ := l.Acquire(ctx, "api", 50)
	_ = lease.Adjust(ctx, 20) // refund 30 -> balance 40
	_ = lease.Adjust(ctx, 0)  // second call is a no-op
	_ = lease.Release(ctx)
	if got := l.state["api"].balance; got != 40 {
		t.Fatalf("balance after refund = %v, want 40", got)
	}
	lease, _ = l.Acquire(ctx, "api", 40)
	_ = lease.Adjust(ctx, 100) // charge 60 more -> -60
	_ = lease.Release(ctx)
	if got := l.state["api"].balance; got != -60 {
		t.Fatalf("balance after charge = %v, want -60", got)
	}
}

func TestImpossibleUnknownAndDefault(t *testing.T) {
	def := bucket.Limits{MaxConcurrency: 1}
	l := mustNew(t, Config{Limits: map[string]bucket.Limits{"api": {TokensPerMinute: 10}}})
	ctx := context.Background()
	var le *bucket.LimitError
	if _, err := l.Acquire(ctx, "api", 100); !errors.As(err, &le) || !le.Permanent {
		t.Fatalf("impossible: want a Permanent LimitError, got %#v", err)
	}
	if _, err := l.Acquire(ctx, "nope", 1); !errors.Is(err, bucket.ErrUnknownKey) {
		t.Fatalf("unknown: %v", err)
	}
	l = mustNew(t, Config{Default: &def})
	if _, err := l.Acquire(ctx, "anything", 1); err != nil {
		t.Fatalf("default: %v", err)
	}
	if _, err := New(Config{Limits: map[string]bucket.Limits{"x": {MaxConcurrency: -1}}}); err == nil {
		t.Fatal("invalid limits accepted")
	}
}

func TestReleaseIdempotentAndLostNeverCloses(t *testing.T) {
	l := mustNew(t, Config{Limits: map[string]bucket.Limits{"api": {MaxConcurrency: 1}}, MaxWait: 20 * time.Millisecond})
	ctx := context.Background()
	lease, _ := l.Acquire(ctx, "api", 0)
	_ = lease.Release(ctx)
	_ = lease.Release(ctx)
	if l.state["api"].live != 0 {
		t.Fatalf("live = %d after double release", l.state["api"].live)
	}
	if lease.Lost() == nil {
		t.Fatal("Lost() must be non-nil")
	}
	select {
	case <-lease.Lost():
		t.Fatal("memory lease reported lost")
	default:
	}
	held, _ := l.Acquire(ctx, "api", 0)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	var le *bucket.LimitError
	if _, err := l.Acquire(cctx, "api", 0); !errors.As(err, &le) || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ctx: want a LimitError caused by cancellation, got %#v", err)
	}
	_ = held.Release(ctx)
}

// TestMaxWaitHoldsUnderMutexContention: an attempt that waits on the
// limiter's mutex past MaxWait is not admitted when it finally gets it.
func TestMaxWaitHoldsUnderMutexContention(t *testing.T) {
	l := mustNew(t, Config{Limits: map[string]bucket.Limits{"api": {MaxConcurrency: 1}}, MaxWait: 20 * time.Millisecond})
	l.mu.Lock()
	go func() {
		time.Sleep(150 * time.Millisecond)
		l.mu.Unlock()
	}()
	_, err := l.Acquire(context.Background(), "api", 0)
	var le *bucket.LimitError
	if !errors.As(err, &le) || !strings.Contains(le.Reason, "MaxWait") {
		t.Fatalf("Acquire after mutex contention = %v, want a MaxWait LimitError", err)
	}
	if s := l.state["api"]; s != nil && s.live != 0 {
		t.Fatalf("live = %d: capacity was charged after MaxWait", s.live)
	}
}
