package ratelimit

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/satint"
)

// defaultPollInterval is how often a blocked Reserve re-checks for capacity.
const defaultPollInterval = 100 * time.Millisecond

// Config configures an InMemoryLimiter. The zero value is usable: a zero
// TokensPerMinute or MaxConcurrency means that dimension is unlimited. The
// limits are validated like bucket.Limits (ADR-0034): a negative limit, a
// NaN, infinite or negative BufferFactor, or a capacity that overflows int is
// an error from NewInMemoryLimiter.
type Config struct {
	// Clock returns the current time; defaults to time.Now. Injectable so
	// refill behavior is deterministic in tests.
	Clock func() time.Time
	// TokensPerMinute is the nominal token rate; 0 means token-unlimited.
	TokensPerMinute int
	// MaxConcurrency caps in-flight reservations; 0 means concurrency-unlimited.
	MaxConcurrency int
	// MaxWait bounds how long Reserve blocks waiting for capacity. 0 means
	// wait until the context is done.
	MaxWait time.Duration
	// PollInterval is the re-check cadence while blocked; defaults to 100ms.
	PollInterval time.Duration
	// BufferFactor scales capacity below TokensPerMinute; 0 selects
	// bucket.DefaultBufferFactor (0.9).
	BufferFactor float64
}

// InMemoryLimiter is a process-local token-bucket + concurrency-semaphore
// Limiter suitable for desktop/local use and tests. It refills lazily from the
// clock (no background goroutine, so nothing to start or stop and no leak when
// embedded). It is safe for concurrent use.
type InMemoryLimiter struct {
	clock          func() time.Time
	lastRefill     time.Time
	maxWait        time.Duration
	pollInterval   time.Duration
	refillPerSec   float64
	tokenCarry     float64
	availableTok   int
	maxCapacity    int
	activeReqs     int
	maxConcurrency int
	tokenWaitHits  int64
	slotWaitHits   int64
	tokenLimiting  bool
	slotLimiting   bool
	mu             sync.Mutex
}

// compile-time assertions live in the test file (package-level vars would trip
// gochecknoglobals).

// NewInMemoryLimiter builds an InMemoryLimiter from cfg. It returns an error,
// and no limiter, when the limits fail validation (see Config).
func NewInMemoryLimiter(cfg Config) (*InMemoryLimiter, error) {
	// The capacity math and its validation are bucket.Limits', so both
	// in-memory limiters and bucket/postgres size a bucket identically.
	limits := bucket.Limits{
		TokensPerMinute: cfg.TokensPerMinute,
		MaxConcurrency:  cfg.MaxConcurrency,
		BufferFactor:    cfg.BufferFactor,
	}
	if err := limits.Validate(); err != nil {
		return nil, fmt.Errorf("ratelimit: %w", err)
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}
	// Capacity floors a positive rate at 1, so a sub-1 product (e.g. 1 TPM *
	// 0.9) never truncates to 0 and silently disables token limiting; the
	// fractional refill is kept via carry.
	capacity := limits.Capacity()
	return &InMemoryLimiter{
		clock:          clock,
		lastRefill:     clock(),
		maxWait:        cfg.MaxWait,
		pollInterval:   poll,
		refillPerSec:   limits.RefillPerSecond(),
		availableTok:   capacity,
		maxCapacity:    capacity,
		maxConcurrency: cfg.MaxConcurrency,
		tokenLimiting:  limits.TokenLimited(),
		slotLimiting:   limits.SlotLimited(),
	}, nil
}

func (l *InMemoryLimiter) tokenLimited() bool { return l.tokenLimiting }
func (l *InMemoryLimiter) slotLimited() bool  { return l.slotLimiting }

// refillLocked adds tokens accrued since the last refill, capped at capacity.
// Caller holds l.mu.
func (l *InMemoryLimiter) refillLocked() {
	if !l.tokenLimited() {
		return
	}
	now := l.clock()
	elapsed := now.Sub(l.lastRefill).Seconds()
	if elapsed <= 0 {
		return
	}
	l.lastRefill = now
	l.tokenCarry += elapsed * l.refillPerSec
	// Consume the carry in chunks of at most MaxInt (converting a larger
	// float is undefined) until it runs out or the bucket is full, so debt
	// deeper than MaxInt is still repaid in one refill (ADR-0034). The room
	// is compared in ints, so it is exact at any capacity. When the room
	// itself would overflow (debt below maxCapacity-MaxInt) it exceeds any
	// chunk, so the chunk is added. That lifts the balance to at least -1,
	// so at most two chunks precede the one that fills the bucket.
	for l.tokenCarry >= 1 {
		whole := math.MaxInt
		if l.tokenCarry < maxCarry {
			whole = int(l.tokenCarry)
		}
		if l.availableTok >= l.maxCapacity-math.MaxInt && whole >= l.maxCapacity-l.availableTok {
			l.availableTok = l.maxCapacity
			l.tokenCarry = 0
			return
		}
		l.tokenCarry -= float64(whole)
		l.availableTok += whole // below the room, so it can't overflow
	}
}

// maxCarry bounds the refill carry converted to int in one step:
// float64(math.MaxInt) rounds up to 2^63, and a float below it converts
// exactly.
const maxCarry = float64(math.MaxInt)

func normalizeUnits(u UsageUnits) (tokens, slots int) {
	return u.Tokens(), u.Slots()
}

func (l *InMemoryLimiter) limitErr(req ReservationRequest, reason string, retryAfter time.Duration) *llms.LimitError {
	return &llms.LimitError{
		Provider:   req.Provider,
		Model:      req.Model,
		Reason:     reason,
		RetryAfter: retryAfter,
	}
}

// ctxDone is the LimitError for a wait ended by ctx: Cause is ctx.Err(), so
// errors.Is matches the context error. Canceled is not retryable; a
// deadline (possibly a per-attempt one) is (ADR-0033).
func (l *InMemoryLimiter) ctxDone(ctx context.Context, req ReservationRequest, reason string, retryAfter time.Duration) *llms.LimitError {
	le := l.limitErr(req, reason, retryAfter)
	le.Cause = ctx.Err()
	return le
}

// impossible returns a Permanent LimitError when the request can never be
// admitted under these limits (estimate larger than maximum capacity).
func (l *InMemoryLimiter) impossible(req ReservationRequest, tokens, slots int) *llms.LimitError {
	var le *llms.LimitError
	switch {
	case l.tokenLimited() && tokens > l.maxCapacity:
		le = l.limitErr(req, "estimated tokens exceed maximum bucket capacity; request can never be admitted", 0)
	case l.slotLimited() && slots > l.maxConcurrency:
		le = l.limitErr(req, "requested concurrency exceeds maximum; request can never be admitted", 0)
	default:
		return nil
	}
	le.Permanent = true
	return le
}

// tryAcquireLocked deducts tokens and a slot when both are available,
// returning true. On failure it records the wait and returns a retry-after
// estimate. Caller holds l.mu.
func (l *InMemoryLimiter) tryAcquireLocked(tokens, slots int) (ok bool, retryAfter time.Duration) {
	hasTokens := !l.tokenLimited() || l.availableTok >= tokens
	// Compare against the free slots, not activeReqs+slots: the difference
	// can't overflow (0 <= activeReqs <= maxConcurrency), and a sum could.
	hasSlot := !l.slotLimited() || slots <= l.maxConcurrency-l.activeReqs
	if hasTokens && hasSlot {
		if l.tokenLimited() {
			l.availableTok -= tokens
		}
		l.activeReqs = satint.Add(l.activeReqs, slots) // unbounded when slots aren't limited
		return true, 0
	}
	if !hasTokens {
		l.tokenWaitHits++
	}
	if !hasSlot {
		l.slotWaitHits++
	}
	return false, l.retryAfterLocked(tokens, hasTokens)
}

// Reserve admits the request, blocking until capacity is available, MaxWait is
// exceeded, or ctx is done. Over-capacity-impossible, timeout, and ctx
// cancellation all return a *llms.LimitError: impossible is Permanent, a
// done ctx sets Cause to ctx.Err(), and MaxWait is temporary (retryable).
func (l *InMemoryLimiter) Reserve(ctx context.Context, req ReservationRequest) (Reservation, error) {
	tokens, slots := normalizeUnits(req.EstimatedUnits)
	if le := l.impossible(req, tokens, slots); le != nil {
		return nil, le
	}

	start := l.clock()
	for {
		// Honor cancellation before acquiring so a done context never
		// consumes capacity (and so a post-sleep cancellation is caught
		// before the next acquire attempt).
		if err := ctx.Err(); err != nil {
			return nil, l.ctxDone(ctx, req, "context done before rate-limit capacity available", 0)
		}

		l.mu.Lock()
		l.refillLocked()
		ok, retryAfter := l.tryAcquireLocked(tokens, slots)
		l.mu.Unlock()
		if ok {
			return &reservation{limiter: l, tokens: tokens, slots: slots}, nil
		}

		sleep := l.pollInterval
		if l.maxWait > 0 {
			remaining := l.maxWait - l.clock().Sub(start)
			if remaining <= 0 {
				return nil, l.limitErr(req, "rate limit wait exceeded MaxWait", retryAfter)
			}
			// Don't oversleep the budget: a PollInterval larger than the
			// remaining MaxWait would block well past it.
			sleep = min(sleep, remaining)
		}
		select {
		case <-ctx.Done():
			return nil, l.ctxDone(ctx, req, "context done while waiting for rate-limit capacity", retryAfter)
		case <-time.After(sleep):
		}
	}
}

// retryAfterLocked estimates how long until enough tokens refill. Caller holds
// l.mu. Zero when the wait is on a concurrency slot (no time-based estimate).
func (l *InMemoryLimiter) retryAfterLocked(tokens int, hasTokens bool) time.Duration {
	if hasTokens || l.refillPerSec <= 0 {
		return 0
	}
	// Exact int difference when it can't overflow (tokens >= 0, so only
	// debt below tokens-MaxInt can), so a small deficit at a large capacity
	// isn't rounded to zero; beyond that it is too large for rounding to
	// matter.
	deficit := float64(tokens) - float64(l.availableTok)
	if l.availableTok >= tokens-math.MaxInt {
		deficit = float64(tokens - l.availableTok)
	}
	if deficit <= 0 {
		return 0
	}
	ns := deficit / l.refillPerSec * float64(time.Second)
	if ns >= maxDurationNS { // a larger float-to-int conversion is undefined
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(ns)
}

// maxDurationNS is float64(math.MaxInt64), which rounds up to 2^63: a
// RetryAfter at or past it saturates (ADR-0034).
const maxDurationNS = float64(math.MaxInt64)

// Stats implements LimiterStats.
func (l *InMemoryLimiter) Stats(_ context.Context) (LimiterSnapshot, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refillLocked()
	return LimiterSnapshot{
		AvailableTokens: l.availableTok,
		MaxCapacity:     l.maxCapacity,
		ActiveRequests:  l.activeReqs,
		MaxConcurrency:  l.maxConcurrency,
		TokenWaitHits:   l.tokenWaitHits,
		SlotWaitHits:    l.slotWaitHits,
	}, nil
}

// reservation is one admitted request against an InMemoryLimiter.
type reservation struct {
	limiter   *InMemoryLimiter
	tokens    int
	slots     int
	committed bool
	released  bool
}

// effectiveTokens is the token count charged for actual usage (ADR-0034),
// shared by InMemoryLimiter and FromBucket:
//
//   - input is InputTokens, or EmbeddingTokens for an embedding call. Both
//     count the request's input tokens, so they overlap and the larger is
//     taken rather than the sum. Cache reads and writes are subsets of
//     InputTokens (ADR-0023) and are not added again.
//   - output is BillableOutputTokens, or OutputTokens + ReasoningTokens when
//     it is unset, so reasoning tokens are charged either way.
//   - TotalTokens is a provider-specific aggregate, so it is a floor, not the
//     charge: usage that reports only a total charges that total instead of
//     refunding the whole estimate, and a total that counts tokens the
//     normalized fields don't (Gemini's tool-use prompt) is not undercounted.
//
// Negative fields count as zero, and the sums saturate at math.MaxInt rather
// than wrapping into a refund.
func effectiveTokens(u llms.Usage) int {
	input := max(u.InputTokens, u.EmbeddingTokens, 0)
	output := u.BillableOutputTokens
	if output <= 0 {
		output = satint.Add(max(u.OutputTokens, 0), max(u.ReasoningTokens, 0))
	}
	return max(satint.Add(input, output), u.TotalTokens)
}

// Commit reconciles actual token usage against the estimate. Lower actual
// refunds the difference (capped at capacity); higher actual charges the
// delta (the bucket may go negative, repaid by future refills, so traffic is
// never undercounted). It does not release the concurrency lease. Repeated
// Commit is a no-op.
func (r *reservation) Commit(_ context.Context, usage llms.Usage) error {
	l := r.limiter
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.committed {
		return nil
	}
	r.committed = true
	if !l.tokenLimited() {
		return nil
	}
	// Apply accrued refill against the old timestamp first; otherwise a
	// later Reserve/Stats would refill from before the delta was charged
	// and erase over-use debt (undercounting traffic).
	l.refillLocked()
	delta := effectiveTokens(usage) - r.tokens // >0: used more; <0: refund (both >= 0, so no overflow)
	l.availableTok = min(satint.Sub(l.availableTok, delta), l.maxCapacity)
	return nil
}

// Release frees the concurrency lease. Idempotent and safe after Commit; does
// not depend on ctx, so it works after the request context is canceled.
func (r *reservation) Release(_ context.Context) error {
	l := r.limiter
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.released {
		return nil
	}
	r.released = true
	l.activeReqs -= r.slots
	l.activeReqs = max(l.activeReqs, 0)
	return nil
}
