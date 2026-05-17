package middleware

import (
	"context"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// TimeoutChat returns middleware that bounds each Complete call with
// context.WithTimeout(d). A non-positive d disables the middleware (the call
// is delegated unchanged) so TimeoutChat(0) is a safe no-op rather than an
// instantly-expiring deadline.
//
// Per the spec's recommended order this sits inside retry: each retry attempt
// gets its own fresh timeout budget rather than one total deadline across all
// attempts. When the deadline trips, the inner provider call observes a
// canceled context and returns its error (real providers classify it via the
// shared apierr as a retryable timeout); the error passes through unwrapped.
func TimeoutChat(d time.Duration) ChatMiddleware {
	return func(next llms.ChatClient) llms.ChatClient {
		return &timeoutChat{next: next, d: d}
	}
}

// TimeoutEmbeddings is the embedding-side counterpart of TimeoutChat.
func TimeoutEmbeddings(d time.Duration) EmbeddingMiddleware {
	return func(next llms.EmbeddingClient) llms.EmbeddingClient {
		return &timeoutEmbedding{next: next, d: d}
	}
}

// withTimeout runs call under a fresh per-call deadline. A non-positive d
// delegates unchanged. The call's error passes through unwrapped so errors.As
// keeps resolving *llms.ProviderError / *llms.LimitError.
func withTimeout[Resp any](
	ctx context.Context,
	d time.Duration,
	call func(context.Context) (Resp, error),
) (Resp, error) {
	if d <= 0 {
		return call(ctx) //nolint:wrapcheck // pass provider/limiter errors through unwrapped
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return call(ctx) //nolint:wrapcheck // pass provider/limiter errors through unwrapped
}

type timeoutChat struct {
	next llms.ChatClient
	d    time.Duration
}

func (c *timeoutChat) Model() llms.ModelRef { return c.next.Model() }

// Complete bounds the inner Complete with a per-call deadline. Structurally
// parallel to timeoutEmbedding.Embed; shared logic lives in withTimeout().
//
//nolint:dupl // parallel typed adapter; shared logic already extracted to withTimeout()
func (c *timeoutChat) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
	return withTimeout(ctx, c.d,
		func(ctx context.Context) (llms.ChatResponse, error) { return c.next.Complete(ctx, req) },
	)
}

type timeoutEmbedding struct {
	next llms.EmbeddingClient
	d    time.Duration
}

func (c *timeoutEmbedding) Model() llms.ModelRef   { return c.next.Model() }
func (c *timeoutEmbedding) DefaultDimensions() int { return c.next.DefaultDimensions() }

// Embed bounds the inner Embed with a per-call deadline. The embedding-typed
// parallel of timeoutChat.Complete; shared logic lives in withTimeout().
//
//nolint:dupl // parallel typed adapter; shared logic already extracted to withTimeout()
func (c *timeoutEmbedding) Embed(ctx context.Context, req llms.EmbeddingRequest) (llms.EmbeddingResponse, error) {
	return withTimeout(ctx, c.d,
		func(ctx context.Context) (llms.EmbeddingResponse, error) { return c.next.Embed(ctx, req) },
	)
}
