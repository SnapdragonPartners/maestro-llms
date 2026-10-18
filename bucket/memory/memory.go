// Package memory is a process-local bucket.Limiter: a per-key token bucket
// and concurrency counter for desktop use, single-instance services and
// tests. State lives in the process, so a crash loses nothing that matters
// and leases can never be lost.
package memory

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
	"github.com/SnapdragonPartners/maestro-llms/bucket/internal/core"
)

// defaultPollInterval is the first re-check delay while blocked.
const defaultPollInterval = 100 * time.Millisecond

// defaultMaxPollInterval caps the backoff between re-checks.
const defaultMaxPollInterval = time.Second

// Config configures a Limiter.
type Config struct {
	// Limits maps each key to its limits.
	Limits map[string]bucket.Limits
	// Default, when set, applies to keys not in Limits. When nil, an
	// unknown key is an error (fail closed).
	Default *bucket.Limits
	// Clock returns the current time for refill; defaults to time.Now.
	// Injectable so refill is deterministic in tests. Waiting (MaxWait,
	// polling) always uses real time.
	Clock func() time.Time
	// MaxWait bounds how long Acquire blocks; 0 waits until ctx is done.
	MaxWait time.Duration
	// PollInterval is the first re-check delay while blocked; defaults to
	// 100ms. It doubles up to MaxPollInterval (default 1s).
	PollInterval    time.Duration
	MaxPollInterval time.Duration
}

// Limiter is a process-local bucket.Limiter. It refills lazily from the
// clock (no background goroutine) and is safe for concurrent use.
type Limiter struct {
	clock  func() time.Time
	limits map[string]bucket.Limits
	def    *bucket.Limits
	state  map[string]*keyState
	waiter core.Waiter
	mu     sync.Mutex
}

type keyState struct {
	lastRefill time.Time
	limits     bucket.Limits
	balance    float64
	live       int
}

// New builds a Limiter, validating every configured limit.
func New(cfg Config) (*Limiter, error) {
	for key, l := range cfg.Limits {
		if err := l.Validate(); err != nil {
			return nil, fmt.Errorf("memory: key %q: %w", key, err)
		}
	}
	if cfg.Default != nil {
		if err := cfg.Default.Validate(); err != nil {
			return nil, fmt.Errorf("memory: default limits: %w", err)
		}
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}
	maxPoll := cfg.MaxPollInterval
	if maxPoll <= 0 {
		maxPoll = defaultMaxPollInterval
	}
	limits := maps.Clone(cfg.Limits)
	var def *bucket.Limits
	if cfg.Default != nil {
		d := *cfg.Default
		def = &d
	}
	return &Limiter{
		clock:  clock,
		limits: limits,
		def:    def,
		state:  make(map[string]*keyState),
		waiter: core.Waiter{MaxWait: cfg.MaxWait, Initial: poll, Max: max(poll, maxPoll)},
	}, nil
}

// stateLocked returns key's state, creating it full on first use. Caller
// holds l.mu.
func (l *Limiter) stateLocked(key string) (*keyState, error) {
	if s, ok := l.state[key]; ok {
		return s, nil
	}
	lim, ok := l.limits[key]
	if !ok {
		if l.def == nil {
			return nil, bucket.UnknownKey(key)
		}
		lim = *l.def
	}
	s := &keyState{limits: lim, balance: float64(lim.Capacity()), lastRefill: l.clock()}
	l.state[key] = s
	return s, nil
}

// settleLocked refills s up to now. Caller holds l.mu.
func (l *Limiter) settleLocked(s *keyState) {
	now := l.clock()
	s.balance = core.Settle(s.limits, s.balance, s.lastRefill, now)
	s.lastRefill = now
}

// Acquire implements bucket.Limiter.
func (l *Limiter) Acquire(ctx context.Context, key string, tokens int) (bucket.Lease, error) {
	tokens = max(tokens, 0)
	return core.Wait(ctx, key, l.waiter, func(attemptCtx context.Context) (core.Result, error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		// The attempt's context carries the MaxWait budget. Check it after
		// taking the lock and before charging anything, so contention on
		// the mutex can never admit work after the budget is spent (the
		// Postgres backend gets the same guarantee from its transaction's
		// context).
		if err := attemptCtx.Err(); err != nil {
			return core.Result{}, fmt.Errorf("memory: %w", err)
		}
		s, err := l.stateLocked(key)
		if err != nil {
			return core.Result{}, err
		}
		if reason := core.Impossible(s.limits, tokens); reason != "" {
			return core.Result{Impossible: reason}, nil
		}
		l.settleLocked(s)
		if !core.Fits(s.limits, s.balance, tokens, s.live) {
			return core.Result{RetryAfter: core.RetryAfter(s.limits, s.balance, tokens)}, nil
		}
		if s.limits.TokenLimited() {
			s.balance -= float64(tokens)
		}
		s.live++
		return core.Result{Lease: &lease{limiter: l, state: s, tokens: tokens, lost: make(chan struct{})}}, nil
	})
}

// lease is one admitted unit against a Limiter.
type lease struct {
	limiter  *Limiter
	state    *keyState
	lost     chan struct{}
	tokens   int
	adjusted bool
	released bool
}

// Adjust implements bucket.Lease.
func (r *lease) Adjust(_ context.Context, actualTokens int) error {
	l := r.limiter
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.adjusted {
		return nil
	}
	r.adjusted = true
	// Settle under the old timestamp first, so the charged delta is not
	// erased by a later refill computed from before it.
	l.settleLocked(r.state)
	r.state.balance = core.Adjusted(r.state.limits, r.state.balance, r.tokens, max(actualTokens, 0))
	return nil
}

// Release implements bucket.Lease. It never fails and ignores ctx.
func (r *lease) Release(_ context.Context) error {
	l := r.limiter
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.released {
		return nil
	}
	r.released = true
	r.state.live = max(r.state.live-1, 0)
	return nil
}

// Lost implements bucket.Lease. A process-local lease cannot be lost, so the
// channel never closes.
func (r *lease) Lost() <-chan struct{} { return r.lost }
