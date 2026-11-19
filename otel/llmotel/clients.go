package llmotel

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
)

const (
	opChat       = "chat"
	opEmbeddings = "embeddings"
	callPrefix   = "maestro_llms.call "
)

// Chat returns the attempt-layer chat middleware: one CLIENT span per
// Complete, named "chat {model}", plus the gen_ai.client.operation.duration
// and gen_ai.client.token.usage metrics. Put it innermost, on the provider
// client: middleware.RecommendedChat(t.Chat()(base), cfg). Do not also wrap
// the chain with Chat from outside; use ChatCall there.
func (t *Telemetry) Chat() middleware.ChatMiddleware {
	return func(next llms.ChatClient) llms.ChatClient {
		return &chatClient{next: next, t: t, logical: false}
	}
}

// ChatCall returns the logical-call chat middleware: one INTERNAL span per
// Complete, named "maestro_llms.call chat {model}", with no usage or response
// attributes and no metrics. Wrap the whole chain with it so the attempt
// spans from Chat nest under it.
func (t *Telemetry) ChatCall() middleware.ChatMiddleware {
	return func(next llms.ChatClient) llms.ChatClient {
		return &chatClient{next: next, t: t, logical: true}
	}
}

// Embeddings is Chat for embedding clients: one CLIENT span per Embed, named
// "embeddings {model}", plus the metrics.
func (t *Telemetry) Embeddings() middleware.EmbeddingMiddleware {
	return func(next llms.EmbeddingClient) llms.EmbeddingClient {
		return &embeddingClient{next: next, t: t, logical: false}
	}
}

// EmbeddingsCall is ChatCall for embedding clients.
func (t *Telemetry) EmbeddingsCall() middleware.EmbeddingMiddleware {
	return func(next llms.EmbeddingClient) llms.EmbeddingClient {
		return &embeddingClient{next: next, t: t, logical: true}
	}
}

type chatClient struct {
	next    llms.ChatClient
	t       *Telemetry
	logical bool
}

func (c *chatClient) Model() llms.ModelRef { return c.next.Model() }

func (c *chatClient) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
	ref := c.next.Model()
	common := c.t.commonAttrs(semconv.GenAIOperationNameChat, ref)
	attrs := append([]attribute.KeyValue(nil), common...)
	if req.Purpose != "" {
		attrs = append(attrs, AttrPurpose.String(string(req.Purpose)))
	}
	if c.logical {
		return traced(ctx, c.t, callPrefix+spanName(opChat, ref.Name), trace.SpanKindInternal, attrs,
			func(ctx context.Context) (llms.ChatResponse, error) { return c.next.Complete(ctx, req) },
			nil)
	}
	if n, ok := middleware.AttemptFromContext(ctx); ok {
		attrs = append(attrs, AttrAttempt.Int(n))
	}
	if req.MaxTokens > 0 {
		attrs = append(attrs, semconv.GenAIRequestMaxTokens(req.MaxTokens))
	}
	if req.Temperature != nil {
		attrs = append(attrs, float32Attr(semconv.GenAIRequestTemperatureKey, *req.Temperature))
	}
	start := time.Now()
	return traced(ctx, c.t, spanName(opChat, ref.Name), trace.SpanKindClient, attrs,
		func(ctx context.Context) (llms.ChatResponse, error) { return c.next.Complete(ctx, req) },
		func(ctx context.Context, span trace.Span, resp *llms.ChatResponse, err error) {
			var out []attribute.KeyValue
			if err == nil {
				if resp.Usage.ProviderRequestID != "" {
					out = append(out, semconv.GenAIResponseID(resp.Usage.ProviderRequestID))
				}
				if resp.StopReason != "" {
					out = append(out, semconv.GenAIResponseFinishReasons(string(resp.StopReason)))
				}
				out = appendUsage(out, resp.Usage)
			}
			span.SetAttributes(out...)
			c.t.record(ctx, start, common, err, resp.Usage.InputTokens, resp.Usage.BillableOutputTokens)
		})
}

type embeddingClient struct {
	next    llms.EmbeddingClient
	t       *Telemetry
	logical bool
}

func (c *embeddingClient) Model() llms.ModelRef   { return c.next.Model() }
func (c *embeddingClient) DefaultDimensions() int { return c.next.DefaultDimensions() }

func (c *embeddingClient) Embed(ctx context.Context, req llms.EmbeddingRequest) (llms.EmbeddingResponse, error) {
	ref := c.next.Model()
	common := c.t.commonAttrs(semconv.GenAIOperationNameEmbeddings, ref)
	attrs := append([]attribute.KeyValue(nil), common...)
	if req.Purpose != "" {
		attrs = append(attrs, AttrPurpose.String(string(req.Purpose)))
	}
	if c.logical {
		return traced(ctx, c.t, callPrefix+spanName(opEmbeddings, ref.Name), trace.SpanKindInternal, attrs,
			func(ctx context.Context) (llms.EmbeddingResponse, error) { return c.next.Embed(ctx, req) },
			nil)
	}
	if n, ok := middleware.AttemptFromContext(ctx); ok {
		attrs = append(attrs, AttrAttempt.Int(n))
	}
	if req.Dimensions > 0 {
		attrs = append(attrs, semconv.GenAIEmbeddingsDimensionCount(req.Dimensions))
	}
	start := time.Now()
	return traced(ctx, c.t, spanName(opEmbeddings, ref.Name), trace.SpanKindClient, attrs,
		func(ctx context.Context) (llms.EmbeddingResponse, error) { return c.next.Embed(ctx, req) },
		func(ctx context.Context, span trace.Span, resp *llms.EmbeddingResponse, err error) {
			if err == nil {
				span.SetAttributes(positive(nil, semconv.GenAIUsageInputTokensKey, resp.Usage.EmbeddingTokens)...)
			}
			c.t.record(ctx, start, common, err, resp.Usage.EmbeddingTokens, 0)
		})
}

// traced runs call inside a span and sets the failure status. after, when
// non-nil, adds attempt-layer attributes and records metrics before the span
// ends. The response passed to after is the zero value on failure, so usage
// from a response returned with an error is never trusted (as in
// middleware.Event).
func traced[Resp any](
	ctx context.Context,
	t *Telemetry,
	name string,
	kind trace.SpanKind,
	attrs []attribute.KeyValue,
	call func(context.Context) (Resp, error),
	after func(context.Context, trace.Span, *Resp, error),
) (Resp, error) {
	ctx, span := t.tracer.Start(ctx, name, trace.WithSpanKind(kind), trace.WithAttributes(attrs...))
	defer endSpan(span)
	resp, err := call(ctx)
	if after != nil {
		var seen Resp
		if err == nil {
			seen = resp
		}
		after(ctx, span, &seen, err)
	}
	if err != nil {
		setFailed(span, errorType(err))
	}
	return resp, err
}

// endSpan ends span. It is deferred in place of span.End() so that End's
// recover() is not called directly by a deferred function: it returns nil,
// and an active panic propagates without the SDK exporting its value as an
// exception event (a panic value can carry request or tool content).
func endSpan(span trace.Span) { span.End() }

// setFailed marks a span failed with error.type as both attribute and status
// description. It never records the error itself (ADR-0051 §5).
func setFailed(span trace.Span, typ string) {
	span.SetAttributes(semconv.ErrorTypeKey.String(typ))
	span.SetStatus(codes.Error, typ)
}

// appendUsage adds the gen_ai.usage.* attributes for the non-zero fields.
// gen_ai.usage.output_tokens includes reasoning under the conventions, so it
// is BillableOutputTokens; input includes cached tokens on both sides
// (ADR-0023).
func appendUsage(attrs []attribute.KeyValue, u llms.Usage) []attribute.KeyValue {
	attrs = positive(attrs, semconv.GenAIUsageInputTokensKey, u.InputTokens)
	attrs = positive(attrs, semconv.GenAIUsageOutputTokensKey, u.BillableOutputTokens)
	attrs = positive(attrs, semconv.GenAIUsageCacheReadInputTokensKey, u.CacheReadTokens)
	attrs = positive(attrs, semconv.GenAIUsageCacheCreationInputTokensKey, u.CacheWriteTokens)
	return positive(attrs, semconv.GenAIUsageReasoningOutputTokensKey, u.ReasoningTokens)
}

// record writes the duration (always) and the token usage (on success, each
// direction only when > 0) with the common attributes plus error.type.
func (t *Telemetry) record(ctx context.Context, start time.Time, common []attribute.KeyValue, err error, in, out int) {
	elapsed := time.Since(start).Seconds()
	attrs := common
	if err != nil {
		attrs = append(append([]attribute.KeyValue(nil), common...), semconv.ErrorTypeKey.String(errorType(err)))
	}
	t.duration.Record(ctx, elapsed, metric.WithAttributeSet(attribute.NewSet(attrs...)))
	if err != nil {
		return
	}
	if in > 0 {
		t.tokens.Record(ctx, int64(in), metric.WithAttributeSet(attribute.NewSet(
			append(append([]attribute.KeyValue(nil), common...), semconv.GenAITokenTypeInput)...)))
	}
	if out > 0 {
		t.tokens.Record(ctx, int64(out), metric.WithAttributeSet(attribute.NewSet(
			append(append([]attribute.KeyValue(nil), common...), semconv.GenAITokenTypeOutput)...)))
	}
}
