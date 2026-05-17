package middleware

import (
	"context"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/ratelimit"
)

// Event is the app-neutral record of one provider call handed to an Observer.
// It deliberately carries only provider-neutral facts — no story/request IDs,
// no cost: pricing tables and attribution are application concerns, not the
// toolkit's (see docs/MAESTRO_DIVERGENCES.md M2).
type Event struct {
	// fieldalignment: pointer-bearing fields first (the strings, the error
	// interface, and Usage — which itself ends in a string), then the only
	// non-pointer field, Latency, last.
	Provider  string
	Model     string
	Operation ratelimit.Operation
	Purpose   llms.Purpose
	// Err is the call's error (nil on success), passed through unwrapped so
	// an observer can errors.As it. WHICH errors are observable depends on
	// where MetricsChat sits in the chain: it only sees what the client it
	// wraps returns. Placed outermost it can observe *ValidationError /
	// *llms.LimitError / *CircuitOpenError; placed innermost (as
	// RecommendedChat does) it sees only provider-attempt outcomes
	// (*llms.ProviderError or nil) — outer rejections never reach it.
	Err error
	// Usage is populated only when Err is nil. On failure it is the zero
	// value: a partial response returned with an error is not trusted.
	Usage   llms.Usage
	Latency time.Duration
}

// Observer receives exactly one Event per provider call, on success or
// failure. Implementations must be safe for concurrent use and should not
// block (the call returns only after Observe returns). Intentionally narrow:
// a single method, no logging-package or app dependency (spec: metrics/
// logging hooks are narrow callback interfaces).
type Observer interface {
	Observe(Event)
}

// MetricsChat returns middleware that reports each Complete to obs. A nil obs
// returns next unchanged (zero overhead, no wrapper). Per the spec's
// recommended order this is the innermost middleware (closest to the provider
// client), so Latency/Usage reflect the real provider call after retry,
// timeout, circuit, and the rate-limit reservation.
func MetricsChat(obs Observer) ChatMiddleware {
	return func(next llms.ChatClient) llms.ChatClient {
		if obs == nil {
			return next
		}
		return &metricsChat{next: next, obs: obs}
	}
}

// MetricsEmbeddings is the embedding-side counterpart of MetricsChat.
func MetricsEmbeddings(obs Observer) EmbeddingMiddleware {
	return func(next llms.EmbeddingClient) llms.EmbeddingClient {
		if obs == nil {
			return next
		}
		return &metricsEmbedding{next: next, obs: obs}
	}
}

// observed times call and emits exactly one Event regardless of outcome.
// The call's error passes through unwrapped so errors.As keeps resolving.
func observed[Resp any](
	obs Observer,
	m llms.ModelRef,
	op ratelimit.Operation,
	purpose llms.Purpose,
	usageOf func(Resp) llms.Usage,
	call func() (Resp, error),
) (Resp, error) {
	start := time.Now()
	resp, err := call()
	ev := Event{
		Provider:  m.Provider,
		Model:     m.Name,
		Operation: op,
		Purpose:   purpose,
		Latency:   time.Since(start),
		Err:       err,
	}
	// Usage is reported only on success. We do not trust a partially-filled
	// response returned alongside an error; Err already signals failure.
	if err == nil {
		ev.Usage = usageOf(resp)
	}
	obs.Observe(ev)
	return resp, err //nolint:wrapcheck // pass provider/limiter errors through unwrapped
}

type metricsChat struct {
	next llms.ChatClient
	obs  Observer
}

func (c *metricsChat) Model() llms.ModelRef { return c.next.Model() }

// Complete reports the inner Complete to the observer. Structurally parallel
// to metricsEmbedding.Embed; shared logic lives in observed().
//
//nolint:dupl // parallel typed adapter; shared logic already extracted to observed()
func (c *metricsChat) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
	return observed(c.obs, c.next.Model(), ratelimit.OperationChat, req.Purpose,
		func(r llms.ChatResponse) llms.Usage { return r.Usage },
		func() (llms.ChatResponse, error) { return c.next.Complete(ctx, req) },
	)
}

type metricsEmbedding struct {
	next llms.EmbeddingClient
	obs  Observer
}

func (c *metricsEmbedding) Model() llms.ModelRef   { return c.next.Model() }
func (c *metricsEmbedding) DefaultDimensions() int { return c.next.DefaultDimensions() }

// Embed reports the inner Embed to the observer. The embedding-typed parallel
// of metricsChat.Complete; shared logic lives in observed().
//
//nolint:dupl // parallel typed adapter; shared logic already extracted to observed()
func (c *metricsEmbedding) Embed(ctx context.Context, req llms.EmbeddingRequest) (llms.EmbeddingResponse, error) {
	return observed(c.obs, c.next.Model(), ratelimit.OperationEmbedding, req.Purpose,
		func(r llms.EmbeddingResponse) llms.Usage { return r.Usage },
		func() (llms.EmbeddingResponse, error) { return c.next.Embed(ctx, req) },
	)
}
