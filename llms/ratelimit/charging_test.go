package ratelimit

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// TestNewInMemoryLimiterValidates: limits that bucket.Limits.Validate rejects
// are an error, not a silently disabled or undefined limiter (ADR-0034).
func TestNewInMemoryLimiterValidates(t *testing.T) {
	bad := []Config{
		{TokensPerMinute: -1},
		{MaxConcurrency: -1},
		{BufferFactor: -0.5},
		{BufferFactor: math.NaN()},
		{BufferFactor: math.Inf(1)},
		{TokensPerMinute: math.MaxInt, BufferFactor: 1},
	}
	for _, cfg := range bad {
		if l, err := NewInMemoryLimiter(cfg); err == nil || l != nil {
			t.Errorf("NewInMemoryLimiter(%+v) = (%v, %v), want (nil, error)", cfg, l, err)
		}
	}
	l := mustNewLimiter(t, Config{TokensPerMinute: math.MaxInt})
	if s, _ := l.Stats(context.Background()); s.MaxCapacity <= 0 {
		t.Fatalf("MaxCapacity at MaxInt TPM = %d, want positive", s.MaxCapacity)
	}
}

// chargingCases pin effectiveTokens (ADR-0034). Each is run through both
// limiter paths: InMemoryLimiter.Commit and FromBucket's Adjust.
var chargingCases = []struct { //nolint:gochecknoglobals // shared test table
	name  string
	usage llms.Usage
	want  int
}{
	{"normalized chat", llms.Usage{InputTokens: 100, OutputTokens: 20, ReasoningTokens: 30, BillableOutputTokens: 50, TotalTokens: 150}, 150},
	{"billable unset counts reasoning", llms.Usage{InputTokens: 100, OutputTokens: 20, ReasoningTokens: 30}, 150},
	{"billable only", llms.Usage{BillableOutputTokens: 40}, 40},
	{"reasoning only", llms.Usage{ReasoningTokens: 40}, 40},
	{"total only is charged, not refunded", llms.Usage{TotalTokens: 70}, 70},
	{"larger total is a floor (Gemini tool-use prompt)", llms.Usage{InputTokens: 100, OutputTokens: 20, BillableOutputTokens: 20, TotalTokens: 130}, 130},
	{"smaller total does not undercount", llms.Usage{InputTokens: 100, OutputTokens: 20, ReasoningTokens: 30, BillableOutputTokens: 50, TotalTokens: 120}, 150},
	{"embedding", llms.Usage{EmbeddingTokens: 7, TotalTokens: 7}, 7},
	{"input and embedding overlap", llms.Usage{InputTokens: 7, EmbeddingTokens: 7}, 7},
	{"cache is a subset of input", llms.Usage{InputTokens: 90, OutputTokens: 10, CacheReadTokens: 60, CacheWriteTokens: 20}, 100},
	{"negative fields count as zero", llms.Usage{InputTokens: -5, OutputTokens: 12, ReasoningTokens: -3}, 12},
	// The sums saturate instead of wrapping into a refund (Copilot, #126).
	{"output sum saturates", llms.Usage{OutputTokens: math.MaxInt, ReasoningTokens: 1}, math.MaxInt},
	{"input plus output saturates", llms.Usage{InputTokens: math.MaxInt, BillableOutputTokens: 1}, math.MaxInt},
}

func TestEffectiveTokensInMemory(t *testing.T) {
	for _, c := range chargingCases {
		t.Run(c.name, func(t *testing.T) {
			clk := &fakeClock{now: time.Unix(0, 0)}
			l := mustNewLimiter(t, Config{TokensPerMinute: 10_000, BufferFactor: 1, Clock: clk.Now})
			r := mustReserve(t, l, chatReq(500))
			if err := r.Commit(context.Background(), c.usage); err != nil {
				t.Fatal(err)
			}
			if s, _ := l.Stats(context.Background()); s.AvailableTokens != 10_000-c.want {
				t.Fatalf("charged %d, want %d", 10_000-s.AvailableTokens, c.want)
			}
		})
	}
}

func TestEffectiveTokensFromBucket(t *testing.T) {
	for _, c := range chargingCases {
		t.Run(c.name, func(t *testing.T) {
			b := &recordingBucket{}
			res, err := FromBucket(b, func(ReservationRequest) string { return "k" }).
				Reserve(context.Background(), bucketReq(UsageUnits{TotalTokensMax: 500}))
			if err != nil {
				t.Fatal(err)
			}
			if err := res.Commit(context.Background(), c.usage); err != nil {
				t.Fatal(err)
			}
			if b.lease.adjusted != c.want {
				t.Fatalf("Adjust(%d), want %d", b.lease.adjusted, c.want)
			}
		})
	}
}

// TestRefillAtValidateBoundStaysDefined: the largest TokensPerMinute
// Validate accepts must refill to capacity after a long gap, not through an
// out-of-range float-to-int conversion (ADR-0034).
func TestRefillAtValidateBoundStaysDefined(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := mustNewLimiter(t, Config{TokensPerMinute: math.MaxInt, Clock: clk.Now})
	r := mustReserve(t, l, chatReq(1_000_000))
	_ = r.Commit(context.Background(), llms.Usage{InputTokens: 1_000_000})
	clk.Advance(10 * time.Minute)
	s, _ := l.Stats(context.Background())
	if s.AvailableTokens != s.MaxCapacity {
		t.Fatalf("after a long gap available = %d, want capacity %d", s.AvailableTokens, s.MaxCapacity)
	}
}

// TestRefillKeepsSmallRoomAtLargeCapacity: at capacity 1e18 float64 can't
// resolve a few tokens (its spacing there is 128), so the refill room must
// be compared in ints. A whole token accrues against a room of two, and the
// bucket must end one short of capacity, not full (Codex r2, buggrit
// f-a01134). A sub-token gap must not move the balance either.
func TestRefillKeepsSmallRoomAtLargeCapacity(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := mustNewLimiter(t, Config{TokensPerMinute: 1, BufferFactor: 1e18, Clock: clk.Now})
	_ = mustReserve(t, l, chatReq(2))

	clk.Advance(time.Nanosecond) // no whole token accrued
	if s, _ := l.Stats(context.Background()); s.AvailableTokens != s.MaxCapacity-2 {
		t.Fatalf("after 1ns available = %d, want capacity-2 = %d", s.AvailableTokens, s.MaxCapacity-2)
	}
	clk.Advance(time.Minute) // one whole token at 1 TPM; the room is 2
	if s, _ := l.Stats(context.Background()); s.AvailableTokens != s.MaxCapacity-1 {
		t.Fatalf("after 1 token of refill available = %d, want capacity-1 = %d", s.AvailableTokens, s.MaxCapacity-1)
	}
	clk.Advance(time.Minute) // the second token fills the room exactly
	if s, _ := l.Stats(context.Background()); s.AvailableTokens != s.MaxCapacity {
		t.Fatalf("after 2 tokens of refill available = %d, want capacity %d", s.AvailableTokens, s.MaxCapacity)
	}
}

// The int-overflow class in the limiter accounting (ADR-0034): each boundary
// saturates instead of wrapping.

// TestTokensSaturates: an estimate whose parts overflow reserves MaxInt
// (rejected as impossible), not a wrapped negative clamped to zero.
func TestTokensSaturates(t *testing.T) {
	if got := (UsageUnits{InputTokens: math.MaxInt, OutputTokensMax: 1}).Tokens(); got != math.MaxInt {
		t.Fatalf("Tokens() = %d, want MaxInt", got)
	}
	l := mustNewLimiter(t, Config{TokensPerMinute: 1000})
	_, err := l.Reserve(context.Background(), ReservationRequest{EstimatedUnits: UsageUnits{InputTokens: math.MaxInt, OutputTokensMax: 1}})
	var le *llms.LimitError
	if !errors.As(err, &le) || !le.Permanent {
		t.Fatalf("overflowing estimate = %v, want a Permanent LimitError", err)
	}
}

// TestCommitDebtSaturates: repeated huge charges drive the balance to MinInt,
// not around to a large positive balance.
func TestCommitDebtSaturates(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := mustNewLimiter(t, Config{TokensPerMinute: 1000, Clock: clk.Now})
	r1, r2 := mustReserve(t, l, chatReq(1)), mustReserve(t, l, chatReq(1))
	_ = r1.Commit(context.Background(), llms.Usage{TotalTokens: math.MaxInt})
	_ = r2.Commit(context.Background(), llms.Usage{TotalTokens: math.MaxInt})
	if s, _ := l.Stats(context.Background()); s.AvailableTokens != math.MinInt {
		t.Fatalf("available = %d, want MinInt (saturated debt)", s.AvailableTokens)
	}
}

// TestRefillRepaysDebtBeyondMaxInt: carry larger than MaxInt is consumed in
// chunks, so one refill repays debt deeper than MaxInt (Copilot, #126).
func TestRefillRepaysDebtBeyondMaxInt(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := mustNewLimiter(t, Config{TokensPerMinute: math.MaxInt, BufferFactor: 0.125, Clock: clk.Now})
	s0, _ := l.Stats(context.Background())
	r1, r2 := mustReserve(t, l, chatReq(1)), mustReserve(t, l, chatReq(1))
	_ = r1.Commit(context.Background(), llms.Usage{TotalTokens: math.MaxInt})
	_ = r2.Commit(context.Background(), llms.Usage{TotalTokens: s0.MaxCapacity / 2})
	clk.Advance(10 * time.Minute)
	if s, _ := l.Stats(context.Background()); s.AvailableTokens != s.MaxCapacity {
		t.Fatalf("after ten idle minutes available = %d, want capacity %d", s.AvailableTokens, s.MaxCapacity)
	}
}

// TestRetryAfterKeepsSmallDeficitAtLargeCapacity: a one-token deficit at
// capacity 1e18 must give a one-minute RetryAfter at 1 TPM, not zero from
// float rounding (Codex r4).
func TestRetryAfterKeepsSmallDeficitAtLargeCapacity(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := mustNewLimiter(t, Config{TokensPerMinute: 1, BufferFactor: 1e18, Clock: clk.Now, PollInterval: time.Millisecond})
	s, _ := l.Stats(context.Background())
	_ = mustReserve(t, l, chatReq(1))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err := l.Reserve(ctx, chatReq(s.MaxCapacity))
	var le *llms.LimitError
	if !errors.As(err, &le) || le.RetryAfter != time.Minute {
		t.Fatalf("Reserve = %v (RetryAfter %v), want a LimitError with RetryAfter 1m", err, le)
	}
}

// TestRetryAfterSaturates: deep debt at a low rate gives a wait beyond int64
// nanoseconds, which saturates (ADR-0034).
func TestRetryAfterSaturates(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := mustNewLimiter(t, Config{TokensPerMinute: 1, BufferFactor: 1e18, Clock: clk.Now, PollInterval: time.Millisecond})
	r := mustReserve(t, l, chatReq(1))
	_ = r.Commit(context.Background(), llms.Usage{TotalTokens: math.MaxInt})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err := l.Reserve(ctx, chatReq(1))
	var le *llms.LimitError
	if !errors.As(err, &le) || le.RetryAfter != time.Duration(math.MaxInt64) {
		t.Fatalf("Reserve = %v, want a LimitError with RetryAfter MaxInt64", err)
	}
}

// TestSlotCheckSaturates: a huge Requests weight must not wrap the slot sum
// and slip past MaxConcurrency.
func TestSlotCheckSaturates(t *testing.T) {
	l := mustNewLimiter(t, Config{MaxConcurrency: math.MaxInt, MaxWait: time.Millisecond, PollInterval: time.Millisecond})
	_ = mustReserve(t, l, ReservationRequest{EstimatedUnits: UsageUnits{Requests: 1}})
	r, err := l.Reserve(context.Background(), ReservationRequest{EstimatedUnits: UsageUnits{Requests: math.MaxInt}})
	if err == nil {
		_ = r.Release(context.Background())
		t.Fatal("MaxInt slots admitted next to a held slot; the slot sum wrapped")
	}
}
