package middleware

import (
	"context"
	"math"
	"math/rand"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// RetryConfig controls retry behavior. Zero/invalid numeric fields (including
// NaN/Inf) are replaced with defaults by normalized(), so callers override
// only what they care about. Jitter is the exception: 0 means "jitter
// disabled" — a caller must be able to opt out for deterministic backoff — so
// RetryConfig{} yields the default backoff schedule with jitter OFF. Use
// DefaultRetryConfig() (or set Jitter explicitly) for the recommended
// jittered policy.
type RetryConfig struct {
	// MaxAttempts is the total number of attempts including the first.
	MaxAttempts int
	// InitialDelay is the backoff before the second attempt.
	InitialDelay time.Duration
	// MaxDelay caps any single backoff (including a provider Retry-After).
	MaxDelay time.Duration
	// BackoffFactor multiplies the delay after each failed attempt.
	BackoffFactor float64
	// Jitter is the +/- fraction randomly applied to each delay to avoid
	// thundering herds (0.1 => +/-10%). 0 disables jitter.
	Jitter float64
}

// DefaultRetryConfig is the recommended policy (adapted from Maestro): 5
// attempts, 1s initial backoff doubling to a 30s cap, +/-10% jitter.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts:   5,
		InitialDelay:  time.Second,
		MaxDelay:      30 * time.Second,
		BackoffFactor: 2.0,
		Jitter:        0.1,
	}
}

func (c RetryConfig) normalized() RetryConfig {
	d := DefaultRetryConfig()
	if c.MaxAttempts < 1 {
		c.MaxAttempts = d.MaxAttempts
	}
	if c.InitialDelay <= 0 {
		c.InitialDelay = d.InitialDelay
	}
	if c.MaxDelay <= 0 {
		c.MaxDelay = d.MaxDelay
	}
	// Reject NaN/Inf explicitly: NaN compares false to everything, so a NaN
	// factor/jitter would slip past ordered checks, propagate into the
	// duration math, collapse waits to ~0, and spin a tight retry loop.
	if math.IsNaN(c.BackoffFactor) || math.IsInf(c.BackoffFactor, 0) || c.BackoffFactor < 1 {
		c.BackoffFactor = d.BackoffFactor
	}
	switch {
	case math.IsNaN(c.Jitter) || math.IsInf(c.Jitter, 0) || c.Jitter < 0:
		c.Jitter = 0
	case c.Jitter > 1:
		c.Jitter = 1 // jitter is a +/- fraction; >1 could make 1+delta negative
	}
	return c
}

// RetryChat returns middleware that retries Complete while the error is
// retryable per llms.Retryable (see ADR-0004: classification is the core
// error model's job, not a separate classifier). It honors the larger of the
// computed backoff and any llms.RetryAfter hint, capped at MaxDelay, and
// aborts immediately if ctx is canceled during a backoff.
//
// Per the spec's recommended order this is the outermost resilience
// middleware: each attempt flows independently through timeout, circuit
// breaker, and the rate-limit reservation, so retries are gated like first
// attempts.
func RetryChat(cfg RetryConfig) ChatMiddleware {
	cfg = cfg.normalized()
	return func(next llms.ChatClient) llms.ChatClient {
		return &retryChat{next: next, cfg: cfg}
	}
}

// RetryEmbeddings is the embedding-side counterpart of RetryChat.
func RetryEmbeddings(cfg RetryConfig) EmbeddingMiddleware {
	cfg = cfg.normalized()
	return func(next llms.EmbeddingClient) llms.EmbeddingClient {
		return &retryEmbedding{next: next, cfg: cfg}
	}
}

// retry runs call up to cfg.MaxAttempts times, backing off between attempts
// while llms.Retryable(err) holds. Provider/limiter errors pass through
// unwrapped so errors.As keeps resolving *llms.ProviderError / *llms.LimitError;
// a backoff interrupted by ctx cancellation returns ctx.Err().
func retry[Resp any](
	ctx context.Context,
	cfg RetryConfig,
	zero Resp,
	call func(context.Context) (Resp, error),
) (Resp, error) {
	var (
		resp  Resp
		err   error
		delay = cfg.InitialDelay
	)
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		resp, err = call(ctx)
		if err == nil || !llms.Retryable(err) {
			return resp, err //nolint:wrapcheck // pass provider/limiter errors through unwrapped
		}
		if attempt == cfg.MaxAttempts {
			break // exhausted; return the last (retryable) error
		}

		wait := delay
		if ra := llms.RetryAfter(err); ra > wait {
			wait = ra
		}
		wait = jittered(min(wait, cfg.MaxDelay), cfg.Jitter)

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, ctx.Err() //nolint:wrapcheck // cancellation supersedes the provider error
		case <-timer.C:
		}

		delay = min(time.Duration(float64(delay)*cfg.BackoffFactor), cfg.MaxDelay)
	}
	return resp, err //nolint:wrapcheck // pass the final provider/limiter error through unwrapped
}

// jittered applies +/-frac random jitter to d. frac<=0 returns d unchanged.
// rand's default source is safe for concurrent use.
func jittered(d time.Duration, frac float64) time.Duration {
	if frac <= 0 {
		return d
	}
	delta := (rand.Float64()*2 - 1) * frac //nolint:gosec // jitter, not security-sensitive
	return max(time.Duration(float64(d)*(1+delta)), 0)
}

type retryChat struct {
	next llms.ChatClient
	cfg  RetryConfig
}

func (c *retryChat) Model() llms.ModelRef { return c.next.Model() }

// Complete retries the inner Complete per c.cfg. Structurally parallel to
// retryEmbedding.Embed; shared logic lives in retry().
//
//nolint:dupl // parallel typed adapter; shared logic already extracted to retry()
func (c *retryChat) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
	return retry(ctx, c.cfg, llms.ChatResponse{},
		func(ctx context.Context) (llms.ChatResponse, error) { return c.next.Complete(ctx, req) },
	)
}

type retryEmbedding struct {
	next llms.EmbeddingClient
	cfg  RetryConfig
}

func (c *retryEmbedding) Model() llms.ModelRef   { return c.next.Model() }
func (c *retryEmbedding) DefaultDimensions() int { return c.next.DefaultDimensions() }

// Embed retries the inner Embed per c.cfg. The embedding-typed parallel of
// retryChat.Complete; shared logic lives in retry().
//
//nolint:dupl // parallel typed adapter; shared logic already extracted to retry()
func (c *retryEmbedding) Embed(ctx context.Context, req llms.EmbeddingRequest) (llms.EmbeddingResponse, error) {
	return retry(ctx, c.cfg, llms.EmbeddingResponse{},
		func(ctx context.Context) (llms.EmbeddingResponse, error) { return c.next.Embed(ctx, req) },
	)
}
