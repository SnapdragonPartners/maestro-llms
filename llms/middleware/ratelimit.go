package middleware

import (
	"context"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/ratelimit"
)

// RateLimitChat returns middleware that reserves limiter capacity before each
// Complete and reconciles actual usage after. A limiter rejection surfaces as
// the limiter's error (a *llms.LimitError from the in-memory limiter) and the
// inner client is not called.
//
// Per the spec, Release is always called and runs on a context that survives
// request cancellation, and Commit records actual usage only when the call
// returned one.
func RateLimitChat(limiter ratelimit.Limiter, est TokenEstimator) ChatMiddleware {
	return func(next llms.ChatClient) llms.ChatClient {
		return &rlChat{next: next, limiter: limiter, est: est}
	}
}

// RateLimitEmbeddings is the embedding-side counterpart of RateLimitChat.
func RateLimitEmbeddings(limiter ratelimit.Limiter, est TokenEstimator) EmbeddingMiddleware {
	return func(next llms.EmbeddingClient) llms.EmbeddingClient {
		return &rlEmbedding{next: next, limiter: limiter, est: est}
	}
}

// reserved runs call under a limiter reservation: estimate -> Reserve ->
// (defer Release on a cancellation-surviving context) -> call -> Commit actual
// usage on success. Limiter and provider errors pass through unwrapped so
// errors.As keeps resolving *llms.LimitError / *llms.ProviderError.
func reserved[Resp any](
	ctx context.Context,
	limiter ratelimit.Limiter,
	rr ratelimit.ReservationRequest,
	zero Resp,
	call func(context.Context) (Resp, error),
	usageOf func(Resp) llms.Usage,
) (Resp, error) {
	res, err := limiter.Reserve(ctx, rr)
	if err != nil {
		return zero, err //nolint:wrapcheck // pass limiter errors (incl. *llms.LimitError) through unwrapped
	}
	// Release frees the lease even if ctx was canceled mid-call.
	defer func() { _ = res.Release(context.WithoutCancel(ctx)) }()

	resp, err := call(ctx)
	if err == nil {
		// Only reconcile when the provider actually reported usage. The
		// core Usage contract leaves fields zero when usage is unknown;
		// committing a zero Usage would refund the whole estimate and
		// undercount the limiter. Keeping the estimate is the safe
		// (overcount) direction when usage is unreported.
		if u := usageOf(resp); usageReported(u) {
			_ = res.Commit(context.WithoutCancel(ctx), u)
		}
	}
	return resp, err //nolint:wrapcheck // pass provider errors through unwrapped
}

// usageReported reports whether any token field is set, i.e. the provider
// returned usage we can reconcile against.
func usageReported(u llms.Usage) bool {
	return u.InputTokens != 0 || u.OutputTokens != 0 || u.TotalTokens != 0 ||
		u.EmbeddingTokens != 0 || u.CacheReadTokens != 0 || u.CacheWriteTokens != 0
}

type rlChat struct {
	next    llms.ChatClient
	limiter ratelimit.Limiter
	est     TokenEstimator
}

func (c *rlChat) Model() llms.ModelRef { return c.next.Model() }

// Complete reserves limiter capacity, delegates, then reconciles usage. It is
// structurally parallel to rlEmbedding.Embed but differs by type
// (ChatClient/ChatRequest) and Operation; the shared logic lives in reserved().
//
//nolint:dupl // parallel typed adapter; shared logic already extracted to reserved()
func (c *rlChat) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
	m := c.next.Model()
	return reserved(ctx, c.limiter, ratelimit.ReservationRequest{
		Provider:       m.Provider,
		Model:          m.Name,
		Operation:      ratelimit.OperationChat,
		Purpose:        req.Purpose,
		EstimatedUnits: c.est.EstimateChat(req),
		Metadata:       req.Metadata,
	}, llms.ChatResponse{},
		func(ctx context.Context) (llms.ChatResponse, error) { return c.next.Complete(ctx, req) },
		func(r llms.ChatResponse) llms.Usage { return r.Usage },
	)
}

type rlEmbedding struct {
	next    llms.EmbeddingClient
	limiter ratelimit.Limiter
	est     TokenEstimator
}

func (c *rlEmbedding) Model() llms.ModelRef   { return c.next.Model() }
func (c *rlEmbedding) DefaultDimensions() int { return c.next.DefaultDimensions() }

// Embed reserves limiter capacity, delegates, then reconciles usage. It is the
// embedding-typed parallel of rlChat.Complete; shared logic lives in reserved().
//
//nolint:dupl // parallel typed adapter; shared logic already extracted to reserved()
func (c *rlEmbedding) Embed(ctx context.Context, req llms.EmbeddingRequest) (llms.EmbeddingResponse, error) {
	m := c.next.Model()
	return reserved(ctx, c.limiter, ratelimit.ReservationRequest{
		Provider:       m.Provider,
		Model:          m.Name,
		Operation:      ratelimit.OperationEmbedding,
		Purpose:        req.Purpose,
		EstimatedUnits: c.est.EstimateEmbeddings(req),
		Metadata:       req.Metadata,
	}, llms.EmbeddingResponse{},
		func(ctx context.Context) (llms.EmbeddingResponse, error) { return c.next.Embed(ctx, req) },
		func(r llms.EmbeddingResponse) llms.Usage { return r.Usage },
	)
}
