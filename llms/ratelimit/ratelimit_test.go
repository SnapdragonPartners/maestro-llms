package ratelimit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

var (
	_ Limiter      = (*InMemoryLimiter)(nil)
	_ LimiterStats = (*InMemoryLimiter)(nil)
	_ Reservation  = (*reservation)(nil)
)

// fakeClock is a manually advanced clock for deterministic refill tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func chatReq(tokens int) ReservationRequest {
	return ReservationRequest{
		Provider: "anthropic", Model: "claude", Operation: OperationChat,
		EstimatedUnits: UsageUnits{TotalTokensMax: tokens},
	}
}

func TestReserveSucceedsAndStatsReflectConsumption(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := NewInMemoryLimiter(Config{TokensPerMinute: 1000, MaxConcurrency: 4, Clock: clk.Now})
	// capacity = 1000 * 0.9 = 900.
	r, err := l.Reserve(context.Background(), chatReq(100))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	s, _ := l.Stats(context.Background())
	if s.AvailableTokens != 800 || s.ActiveRequests != 1 || s.MaxCapacity != 900 {
		t.Fatalf("unexpected stats: %+v", s)
	}
	if err := r.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, _ = l.Stats(context.Background())
	if s.ActiveRequests != 0 {
		t.Fatalf("active not released: %+v", s)
	}
}

func TestImpossibleRequestsReturnLimitError(t *testing.T) {
	l := NewInMemoryLimiter(Config{TokensPerMinute: 100, MaxConcurrency: 1})
	_, err := l.Reserve(context.Background(), chatReq(10_000))
	var le *llms.LimitError
	if !errors.As(err, &le) {
		t.Fatalf("want *llms.LimitError, got %v", err)
	}
	_, err = l.Reserve(context.Background(), ReservationRequest{
		EstimatedUnits: UsageUnits{Requests: 5},
	})
	if !errors.As(err, &le) {
		t.Fatalf("want *llms.LimitError for over-concurrency, got %v", err)
	}
}

func TestConcurrencyGateBlocksUntilRelease(t *testing.T) {
	l := NewInMemoryLimiter(Config{MaxConcurrency: 1}) // token-unlimited
	r1, err := l.Reserve(context.Background(), chatReq(0))
	if err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = l.Reserve(canceled, chatReq(0))
	var le *llms.LimitError
	if !errors.As(err, &le) {
		t.Fatalf("expected LimitError while slot taken, got %v", err)
	}

	if relErr := r1.Release(context.Background()); relErr != nil {
		t.Fatal(relErr)
	}
	r3, err := l.Reserve(context.Background(), chatReq(0))
	if err != nil {
		t.Fatalf("reserve after release should succeed: %v", err)
	}
	_ = r3.Release(context.Background())
}

func TestCommitReconciliation(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)} // frozen: no refill interference
	l := NewInMemoryLimiter(Config{TokensPerMinute: 1000, MaxConcurrency: 2, Clock: clk.Now})

	// Under-use: reserve 200, actually use 50 -> refund 150.
	r, _ := l.Reserve(context.Background(), chatReq(200))
	s, _ := l.Stats(context.Background())
	if s.AvailableTokens != 700 {
		t.Fatalf("after reserve want 700, got %d", s.AvailableTokens)
	}
	if err := r.Commit(context.Background(), llms.Usage{TotalTokens: 50}); err != nil {
		t.Fatal(err)
	}
	s, _ = l.Stats(context.Background())
	if s.AvailableTokens != 850 {
		t.Fatalf("after under-use commit want 850, got %d", s.AvailableTokens)
	}
	// Repeated Commit is a no-op.
	_ = r.Commit(context.Background(), llms.Usage{TotalTokens: 9999})
	s, _ = l.Stats(context.Background())
	if s.AvailableTokens != 850 {
		t.Fatalf("repeated commit changed state: %d", s.AvailableTokens)
	}

	// Over-use: reserve 100, actually use 1000 -> bucket goes negative (debt),
	// so traffic is never undercounted.
	r2, _ := l.Reserve(context.Background(), chatReq(100))
	if err := r2.Commit(context.Background(), llms.Usage{InputTokens: 400, OutputTokens: 600}); err != nil {
		t.Fatal(err)
	}
	s, _ = l.Stats(context.Background())
	if s.AvailableTokens != 850-100-(1000-100) {
		t.Fatalf("over-use accounting wrong: got %d", s.AvailableTokens)
	}
}

func TestReleaseIdempotentAndSafeAfterCommit(t *testing.T) {
	l := NewInMemoryLimiter(Config{TokensPerMinute: 1000, MaxConcurrency: 2})
	r, _ := l.Reserve(context.Background(), chatReq(10))
	if err := r.Commit(context.Background(), llms.Usage{TotalTokens: 10}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := r.Release(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := l.Stats(context.Background())
	if s.ActiveRequests != 0 {
		t.Fatalf("repeated Release double-decremented: %+v", s)
	}
}

func TestLazyRefillFromClock(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := NewInMemoryLimiter(Config{TokensPerMinute: 600, MaxConcurrency: 1, Clock: clk.Now})
	// capacity = 540, refill = 10 tok/sec.
	r, _ := l.Reserve(context.Background(), chatReq(540))
	_ = r.Release(context.Background())
	s, _ := l.Stats(context.Background())
	if s.AvailableTokens != 0 {
		t.Fatalf("want drained bucket, got %d", s.AvailableTokens)
	}
	clk.Advance(20 * time.Second) // +200 tokens
	s, _ = l.Stats(context.Background())
	if s.AvailableTokens != 200 {
		t.Fatalf("want 200 after refill, got %d", s.AvailableTokens)
	}
	clk.Advance(time.Hour) // refill caps at capacity
	s, _ = l.Stats(context.Background())
	if s.AvailableTokens != 540 {
		t.Fatalf("refill should cap at capacity, got %d", s.AvailableTokens)
	}
}

func TestUnlimitedDimensions(t *testing.T) {
	l := NewInMemoryLimiter(Config{}) // both unlimited
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			r, err := l.Reserve(context.Background(), chatReq(1_000_000))
			if err != nil {
				t.Errorf("unlimited reserve failed: %v", err)
				return
			}
			_ = r.Commit(context.Background(), llms.Usage{TotalTokens: 5})
			_ = r.Release(context.Background())
		})
	}
	wg.Wait()
}
