package middleware

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// CircuitOpenError is returned when the circuit breaker is open and rejects a
// call before it reaches the inner client. It is deliberately neither a
// *llms.ProviderError nor a *llms.LimitError, so llms.Retryable reports it as
// NOT retryable: the retry middleware sits outside the circuit, and retrying
// an open breaker in-call would just spin until OpenTimeout. Recovery is by
// the Open->HalfOpen transition on a later call, not by in-call retry. See
// docs/adr/0005-circuit-open-error.md.
type CircuitOpenError struct {
	Provider string
	Model    string
	// RetryAfter is the remaining time until the breaker will admit a probe
	// (a hint for the caller; the error stays non-retryable for middleware).
	RetryAfter time.Duration
}

func (e *CircuitOpenError) Error() string {
	return fmt.Sprintf("%s/%s: circuit open (retry after %s)", e.Provider, e.Model, e.RetryAfter)
}

// CircuitConfig controls the breaker. Zero/invalid fields fall back to
// defaults via normalized(), so CircuitConfig{} is a valid "use defaults".
type CircuitConfig struct {
	// FailureThreshold is the consecutive retryable failures in Closed state
	// that trip the breaker Open.
	FailureThreshold int
	// SuccessThreshold is the consecutive successes in HalfOpen that close it.
	SuccessThreshold int
	// OpenTimeout is how long the breaker stays Open before admitting a probe.
	OpenTimeout time.Duration
}

// DefaultCircuitConfig is the recommended policy (adapted from Maestro): trip
// after 5 consecutive failures, require 3 successes to recover, 30s open.
func DefaultCircuitConfig() CircuitConfig {
	return CircuitConfig{FailureThreshold: 5, SuccessThreshold: 3, OpenTimeout: 30 * time.Second}
}

func (c CircuitConfig) normalized() CircuitConfig {
	d := DefaultCircuitConfig()
	if c.FailureThreshold < 1 {
		c.FailureThreshold = d.FailureThreshold
	}
	if c.SuccessThreshold < 1 {
		c.SuccessThreshold = d.SuccessThreshold
	}
	if c.OpenTimeout <= 0 {
		c.OpenTimeout = d.OpenTimeout
	}
	return c
}

type circuitState int

const (
	stateClosed circuitState = iota
	stateOpen
	stateHalfOpen
)

// breaker is the shared three-state machine. One breaker is created per
// wrapped client (the constructor closure runs once per next), so state is
// per-client. Guarded by mu; all transitions take the lock.
type breaker struct {
	// Pointer-bearing fields first, ordered to minimize the GC pointer-scan
	// prefix (fieldalignment): time.Time's internal pointer is at offset 16.
	openedAt  time.Time
	provider  string
	model     string
	cfg       CircuitConfig
	mu        sync.Mutex
	state     circuitState
	failures  int
	successes int
	// probing is true while a single HalfOpen probe call is in flight; it
	// gates concurrent callers so a recovering provider sees one request at
	// a time, not a thundering herd.
	probing bool
}

// allow reports whether a call may proceed. When Open it rejects with a
// *CircuitOpenError until OpenTimeout elapses, then moves to HalfOpen and
// admits a SINGLE probe. While that probe (or any subsequent HalfOpen probe
// before SuccessThreshold is reached) is in flight, other callers are
// rejected, so a recovering provider is not hit by a concurrent burst. The
// probe is bounded by the caller's context/timeout middleware; record()
// clears the gate.
func (b *breaker) allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateClosed:
		return nil
	case stateOpen:
		if remaining := b.cfg.OpenTimeout - time.Since(b.openedAt); remaining > 0 {
			return &CircuitOpenError{Provider: b.provider, Model: b.model, RetryAfter: remaining}
		}
		// Timeout elapsed: this caller becomes the single HalfOpen probe.
		b.state = stateHalfOpen
		b.successes = 0
		b.probing = true
		return nil
	case stateHalfOpen:
		if b.probing {
			return &CircuitOpenError{Provider: b.provider, Model: b.model} // probe already in flight
		}
		b.probing = true // admit the next sequential probe
		return nil
	default:
		return nil
	}
}

// record folds a call result into the state. Only llms.Retryable failures
// count against the breaker (a provider-health signal); a success resets a
// Closed failure streak. Non-retryable/caller errors are neutral — they pass
// through without opening the breaker (consistent with ADR-0004).
func (b *breaker) record(err error) {
	success := err == nil
	failure := err != nil && llms.Retryable(err)
	if !success && !failure {
		return // neutral: caller/non-retryable error, no health signal
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateClosed:
		if failure {
			b.failures++
			if b.failures >= b.cfg.FailureThreshold {
				b.trip()
			}
			return
		}
		b.failures = 0 // success breaks the streak
	case stateHalfOpen:
		if failure {
			b.trip() // probe failed: reopen (trip clears probing)
			return
		}
		b.successes++
		if b.successes >= b.cfg.SuccessThreshold {
			b.state = stateClosed
			b.failures = 0
			b.successes = 0
		}
		b.probing = false // probe done: admit the next sequential probe (or none, if now Closed)
	case stateOpen:
		// Defensive: allow() always moves Open->HalfOpen before a call runs,
		// so this is normally unreachable. Re-trip on failure to be safe.
		if failure {
			b.trip()
		}
	}
}

// trip moves the breaker Open and starts the timeout. Caller holds b.mu.
func (b *breaker) trip() {
	b.state = stateOpen
	b.openedAt = time.Now()
	b.failures = 0
	b.successes = 0
	b.probing = false
}

// CircuitChat returns middleware that fails fast while the breaker is open.
// Per the spec's recommended order it sits inside retry/timeout and outside
// the rate-limit reservation.
func CircuitChat(cfg CircuitConfig) ChatMiddleware {
	cfg = cfg.normalized()
	return func(next llms.ChatClient) llms.ChatClient {
		m := next.Model()
		return &circuitChat{next: next, b: &breaker{cfg: cfg, provider: m.Provider, model: m.Name}}
	}
}

// CircuitEmbeddings is the embedding-side counterpart of CircuitChat.
func CircuitEmbeddings(cfg CircuitConfig) EmbeddingMiddleware {
	cfg = cfg.normalized()
	return func(next llms.EmbeddingClient) llms.EmbeddingClient {
		m := next.Model()
		return &circuitEmbedding{next: next, b: &breaker{cfg: cfg, provider: m.Provider, model: m.Name}}
	}
}

// guarded checks the breaker, runs call, then records the outcome. Provider/
// limiter errors pass through unwrapped so errors.As keeps resolving.
func guarded[Resp any](
	b *breaker,
	zero Resp,
	call func() (Resp, error),
) (Resp, error) {
	if err := b.allow(); err != nil {
		return zero, err //nolint:wrapcheck // *CircuitOpenError is the typed sentinel callers match
	}
	resp, err := call()
	b.record(err)
	return resp, err //nolint:wrapcheck // pass provider/limiter errors through unwrapped
}

type circuitChat struct {
	next llms.ChatClient
	b    *breaker
}

func (c *circuitChat) Model() llms.ModelRef { return c.next.Model() }

// Complete guards the inner Complete with the breaker. Structurally parallel
// to circuitEmbedding.Embed; shared logic lives in guarded().
//
//nolint:dupl // parallel typed adapter; shared logic already extracted to guarded()
func (c *circuitChat) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
	return guarded(c.b, llms.ChatResponse{},
		func() (llms.ChatResponse, error) { return c.next.Complete(ctx, req) },
	)
}

type circuitEmbedding struct {
	next llms.EmbeddingClient
	b    *breaker
}

func (c *circuitEmbedding) Model() llms.ModelRef   { return c.next.Model() }
func (c *circuitEmbedding) DefaultDimensions() int { return c.next.DefaultDimensions() }

// Embed guards the inner Embed with the breaker. The embedding-typed parallel
// of circuitChat.Complete; shared logic lives in guarded().
//
//nolint:dupl // parallel typed adapter; shared logic already extracted to guarded()
func (c *circuitEmbedding) Embed(ctx context.Context, req llms.EmbeddingRequest) (llms.EmbeddingResponse, error) {
	return guarded(c.b, llms.EmbeddingResponse{},
		func() (llms.EmbeddingResponse, error) { return c.next.Embed(ctx, req) },
	)
}
