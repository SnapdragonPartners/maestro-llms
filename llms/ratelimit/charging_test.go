package ratelimit

import (
	"context"
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
