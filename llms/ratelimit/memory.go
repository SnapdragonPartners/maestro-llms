package ratelimit

import (
	"context"
	"sync"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// defaultBufferFactor sizes bucket capacity below the nominal per-minute rate
// to absorb token-estimate inaccuracy (overestimation is the safe direction).
const defaultBufferFactor = 0.9

// defaultPollInterval is how often a blocked Reserve re-checks for capacity.
const defaultPollInterval = 100 * time.Millisecond

// Config configures an InMemoryLimiter. The zero value is usable: a zero
// TokensPerMinute or MaxConcurrency means that dimension is unlimited.
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
	// BufferFactor scales capacity below TokensPerMinute; defaults to 0.9.
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
	mu             sync.Mutex
}

// compile-time assertions live in the test file (package-level vars would trip
// gochecknoglobals).

// NewInMemoryLimiter builds an InMemoryLimiter from cfg.
func NewInMemoryLimiter(cfg Config) *InMemoryLimiter {
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	buffer := cfg.BufferFactor
	if buffer <= 0 {
		buffer = defaultBufferFactor
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}
	capacity := int(float64(cfg.TokensPerMinute) * buffer)
	return &InMemoryLimiter{
		clock:          clock,
		lastRefill:     clock(),
		maxWait:        cfg.MaxWait,
		pollInterval:   poll,
		refillPerSec:   float64(cfg.TokensPerMinute) / 60.0,
		availableTok:   capacity,
		maxCapacity:    capacity,
		maxConcurrency: cfg.MaxConcurrency,
	}
}

func (l *InMemoryLimiter) tokenLimited() bool { return l.maxCapacity > 0 }
func (l *InMemoryLimiter) slotLimited() bool  { return l.maxConcurrency > 0 }

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
	whole := int(l.tokenCarry)
	if whole <= 0 {
		return
	}
	l.tokenCarry -= float64(whole)
	l.availableTok += whole
	if l.availableTok > l.maxCapacity {
		l.availableTok = l.maxCapacity
		l.tokenCarry = 0
	}
}

func normalizeUnits(u UsageUnits) (tokens, slots int) {
	tokens = max(u.tokens(), 0)
	slots = max(u.Requests, 1)
	return tokens, slots
}

func (l *InMemoryLimiter) limitErr(req ReservationRequest, reason string, retryAfter time.Duration) *llms.LimitError {
	return &llms.LimitError{
		Provider:   req.Provider,
		Model:      req.Model,
		Reason:     reason,
		RetryAfter: retryAfter,
	}
}

// impossible returns a LimitError when the request can never be admitted at
// any time (estimate larger than maximum capacity).
func (l *InMemoryLimiter) impossible(req ReservationRequest, tokens, slots int) *llms.LimitError {
	switch {
	case l.tokenLimited() && tokens > l.maxCapacity:
		return l.limitErr(req, "estimated tokens exceed maximum bucket capacity; request can never be admitted", 0)
	case l.slotLimited() && slots > l.maxConcurrency:
		return l.limitErr(req, "requested concurrency exceeds maximum; request can never be admitted", 0)
	default:
		return nil
	}
}

// tryAcquireLocked deducts tokens and a slot when both are available,
// returning true. On failure it records the wait and returns a retry-after
// estimate. Caller holds l.mu.
func (l *InMemoryLimiter) tryAcquireLocked(tokens, slots int) (ok bool, retryAfter time.Duration) {
	hasTokens := !l.tokenLimited() || l.availableTok >= tokens
	hasSlot := !l.slotLimited() || l.activeReqs+slots <= l.maxConcurrency
	if hasTokens && hasSlot {
		if l.tokenLimited() {
			l.availableTok -= tokens
		}
		l.activeReqs += slots
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
// cancellation all return a *llms.LimitError.
func (l *InMemoryLimiter) Reserve(ctx context.Context, req ReservationRequest) (Reservation, error) {
	tokens, slots := normalizeUnits(req.EstimatedUnits)
	if le := l.impossible(req, tokens, slots); le != nil {
		return nil, le
	}

	start := l.clock()
	for {
		l.mu.Lock()
		l.refillLocked()
		ok, retryAfter := l.tryAcquireLocked(tokens, slots)
		l.mu.Unlock()
		if ok {
			return &reservation{limiter: l, tokens: tokens, slots: slots}, nil
		}

		if l.maxWait > 0 && l.clock().Sub(start) >= l.maxWait {
			return nil, l.limitErr(req, "rate limit wait exceeded MaxWait", retryAfter)
		}
		select {
		case <-ctx.Done():
			return nil, l.limitErr(req, "context done while waiting for rate-limit capacity", retryAfter)
		case <-time.After(l.pollInterval):
		}
	}
}

// retryAfterLocked estimates how long until enough tokens refill. Caller holds
// l.mu. Zero when the wait is on a concurrency slot (no time-based estimate).
func (l *InMemoryLimiter) retryAfterLocked(tokens int, hasTokens bool) time.Duration {
	if hasTokens || l.refillPerSec <= 0 {
		return 0
	}
	deficit := float64(tokens - l.availableTok)
	if deficit <= 0 {
		return 0
	}
	return time.Duration(deficit / l.refillPerSec * float64(time.Second))
}

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
	actual := usage.TotalTokens
	if actual == 0 {
		actual = usage.InputTokens + usage.OutputTokens
	}
	delta := actual - r.tokens // >0: used more than reserved; <0: refund
	l.availableTok -= delta
	l.availableTok = min(l.availableTok, l.maxCapacity)
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
