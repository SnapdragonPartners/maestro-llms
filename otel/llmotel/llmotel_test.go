package llmotel_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
	"github.com/SnapdragonPartners/maestro-llms/otel/llmotel"
)

const canary = "CANARY-secret-prompt-text"

type harness struct {
	t      *testing.T
	spans  *tracetest.SpanRecorder
	reader *sdkmetric.ManualReader
	tel    *llmotel.Telemetry
}

func newHarness(t *testing.T, opts llmotel.Options) *harness {
	t.Helper()
	h := &harness{t: t, spans: tracetest.NewSpanRecorder(), reader: sdkmetric.NewManualReader()}
	opts.TracerProvider = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(h.spans))
	opts.MeterProvider = sdkmetric.NewMeterProvider(sdkmetric.WithReader(h.reader))
	tel, err := llmotel.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.tel = tel
	return h
}

func (h *harness) ended() []sdktrace.ReadOnlySpan { return h.spans.Ended() }

func (h *harness) span(name string) sdktrace.ReadOnlySpan {
	h.t.Helper()
	var found []sdktrace.ReadOnlySpan
	for _, s := range h.ended() {
		if s.Name() == name {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		h.t.Fatalf("want exactly one span %q, got %d (all: %v)", name, len(found), h.names())
	}
	return found[0]
}

func (h *harness) names() []string {
	var out []string
	for _, s := range h.ended() {
		out = append(out, s.Name())
	}
	return out
}

func (h *harness) metrics() metricdata.ResourceMetrics {
	h.t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		h.t.Fatalf("Collect: %v", err)
	}
	return rm
}

// histogram returns the data points of the named metric, keyed by their
// attribute set's encoded form.
func (h *harness) histogram(rm *metricdata.ResourceMetrics, name string) []histPoint {
	h.t.Helper()
	var out []histPoint
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					out = append(out, histPoint{attrs: p.Attributes, count: p.Count, sum: p.Sum})
				}
			case metricdata.Histogram[int64]:
				for _, p := range d.DataPoints {
					out = append(out, histPoint{attrs: p.Attributes, count: p.Count, sum: float64(p.Sum)})
				}
			default:
				h.t.Fatalf("metric %s: unexpected data %T", name, m.Data)
			}
		}
	}
	return out
}

type histPoint struct {
	attrs attribute.Set
	count uint64
	sum   float64
}

func attrMap(kvs []attribute.KeyValue) map[attribute.Key]attribute.Value {
	m := make(map[attribute.Key]attribute.Value, len(kvs))
	for _, kv := range kvs {
		m[kv.Key] = kv.Value
	}
	return m
}

func wantAttrs(t *testing.T, got []attribute.KeyValue, want map[string]any, absent ...string) {
	t.Helper()
	m := attrMap(got)
	for k, v := range want {
		gv, ok := m[attribute.Key(k)]
		if !ok {
			t.Errorf("attribute %s missing (have %v)", k, got)
			continue
		}
		if gs, ws := fmt.Sprint(gv.AsInterface()), fmt.Sprint(v); gs != ws {
			t.Errorf("attribute %s = %s, want %s", k, gs, ws)
		}
	}
	for _, k := range absent {
		if v, ok := m[attribute.Key(k)]; ok {
			t.Errorf("attribute %s should be absent, got %v", k, v.AsInterface())
		}
	}
}

func chatReq() llms.ChatRequest {
	return llms.ChatRequest{Messages: []llms.Message{llms.UserText(canary)}}
}

var anthropicModel = llms.ModelRef{Provider: llms.ProviderAnthropic, Name: "claude-test"}

func TestChatAttemptSpanAndMetrics(t *testing.T) {
	h := newHarness(t, llmotel.Options{})
	temp := float32(0.7)
	fake := &testllm.FakeChatClient{ModelRef: anthropicModel, Responses: []llms.ChatResponse{{
		Message:    llms.AssistantText(canary),
		Text:       canary,
		StopReason: "end_turn",
		Usage: llms.Usage{
			InputTokens: 100, OutputTokens: 7, ReasoningTokens: 13, BillableOutputTokens: 20,
			CacheReadTokens: 40, ProviderRequestID: "req-1",
		},
	}}}
	req := chatReq()
	req.MaxTokens = 512
	req.Temperature = &temp
	req.Purpose = llms.PurposeSummarization
	if _, err := h.tel.Chat()(fake).Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	s := h.span("chat claude-test")
	if s.SpanKind() != trace.SpanKindClient {
		t.Errorf("kind = %v, want client", s.SpanKind())
	}
	if s.Status().Code != codes.Unset {
		t.Errorf("status = %v, want unset", s.Status())
	}
	wantAttrs(t, s.Attributes(), map[string]any{
		"gen_ai.operation.name":                "chat",
		"gen_ai.provider.name":                 "anthropic",
		"gen_ai.request.model":                 "claude-test",
		"gen_ai.request.max_tokens":            512,
		"gen_ai.request.temperature":           0.7,
		"gen_ai.response.id":                   "req-1",
		"gen_ai.response.finish_reasons":       []string{"end_turn"},
		"gen_ai.usage.input_tokens":            100,
		"gen_ai.usage.output_tokens":           20, // billable: includes reasoning
		"gen_ai.usage.cache_read.input_tokens": 40,
		"gen_ai.usage.reasoning.output_tokens": 13,
		"maestro_llms.purpose":                 "summarization",
	}, "gen_ai.usage.cache_creation.input_tokens", "maestro_llms.attempt", "error.type", "gen_ai.response.model")

	rm := h.metrics()
	dur := h.histogram(&rm, "gen_ai.client.operation.duration")
	if len(dur) != 1 || dur[0].count != 1 {
		t.Fatalf("duration points = %+v, want one record", dur)
	}
	wantAttrs(t, dur[0].attrs.ToSlice(), map[string]any{
		"gen_ai.operation.name": "chat", "gen_ai.provider.name": "anthropic", "gen_ai.request.model": "claude-test",
	}, "error.type", "maestro_llms.purpose", "maestro_llms.attempt")
	if n := dur[0].attrs.Len(); n != 3 {
		t.Errorf("duration has %d attributes, want only the 3 low-cardinality ones: %v", n, dur[0].attrs.ToSlice())
	}
	tok := h.histogram(&rm, "gen_ai.client.token.usage")
	sums := map[string]float64{}
	for _, p := range tok {
		v, _ := p.attrs.Value("gen_ai.token.type")
		sums[v.AsString()] += p.sum
		if p.count != 1 {
			t.Errorf("token point %v count = %d, want 1", p.attrs.ToSlice(), p.count)
		}
	}
	if sums["input"] != 100 || sums["output"] != 20 || len(sums) != 2 {
		t.Errorf("token sums = %v, want input 100, output 20", sums)
	}
}

func TestChatZeroUsageIsOmitted(t *testing.T) {
	h := newHarness(t, llmotel.Options{})
	fake := &testllm.FakeChatClient{ModelRef: anthropicModel, Text: "ok"}
	if _, err := h.tel.Chat()(fake).Complete(context.Background(), chatReq()); err != nil {
		t.Fatal(err)
	}
	wantAttrs(t, h.span("chat claude-test").Attributes(), nil,
		"gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", "gen_ai.usage.cache_read.input_tokens",
		"gen_ai.usage.cache_creation.input_tokens", "gen_ai.usage.reasoning.output_tokens",
		"gen_ai.response.id", "gen_ai.response.finish_reasons", "gen_ai.request.max_tokens", "gen_ai.request.temperature")
	rm := h.metrics()
	if tok := h.histogram(&rm, "gen_ai.client.token.usage"); len(tok) != 0 {
		t.Errorf("token usage recorded for unreported usage: %+v", tok)
	}
	if dur := h.histogram(&rm, "gen_ai.client.operation.duration"); len(dur) != 1 {
		t.Errorf("duration points = %d, want 1", len(dur))
	}
}

func TestErrorTypeAndNoErrorText(t *testing.T) {
	cases := []struct {
		err  error
		name string
		want string
	}{
		{name: "provider", err: &llms.ProviderError{Kind: llms.ErrorKindContentPolicy, Message: canary}, want: "content_policy"},
		{name: "wrapped provider", err: fmt.Errorf("ctx %s: %w", canary, &llms.ProviderError{Kind: llms.ErrorKindAuth, Message: canary}), want: "auth"},
		{name: "provider without kind", err: &llms.ProviderError{Message: canary}, want: "_OTHER"},
		{name: "limit", err: &llms.LimitError{Reason: canary}, want: "rate_limited"},
		{name: "plain", err: errors.New(canary), want: "_OTHER"},
		{name: "context", err: fmt.Errorf("%s: %w", canary, context.DeadlineExceeded), want: "_OTHER"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, llmotel.Options{})
			// A response returned with an error is not trusted: its usage must
			// not be exported.
			fake := &testllm.FakeChatClient{ModelRef: anthropicModel, Func: func(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
				return llms.ChatResponse{Usage: llms.Usage{InputTokens: 9, ProviderRequestID: canary}}, tc.err
			}}
			emb := &testllm.FakeEmbeddingClient{ModelRef: anthropicModel, Err: tc.err}
			_, _ = h.tel.Chat()(fake).Complete(context.Background(), chatReq())
			_, _ = h.tel.ChatCall()(fake).Complete(context.Background(), chatReq())
			_, _ = h.tel.Embeddings()(emb).Embed(context.Background(), llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{{Text: canary}}})
			_, _ = h.tel.EmbeddingsCall()(emb).Embed(context.Background(), llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{{Text: canary}}})

			if got := len(h.ended()); got != 4 {
				t.Fatalf("spans = %v, want 4", h.names())
			}
			for _, s := range h.ended() {
				if s.Status().Code != codes.Error || s.Status().Description != tc.want {
					t.Errorf("%s: status = %+v, want Error %q", s.Name(), s.Status(), tc.want)
				}
				wantAttrs(t, s.Attributes(), map[string]any{"error.type": tc.want}, "gen_ai.usage.input_tokens", "gen_ai.response.id")
				if len(s.Events()) != 0 {
					t.Errorf("%s: span has events %v; errors must not be recorded", s.Name(), s.Events())
				}
			}
			assertNoCanary(t, h)
			rm := h.metrics()
			dur := h.histogram(&rm, "gen_ai.client.operation.duration")
			if len(dur) != 2 { // chat + embeddings attempt layers only
				t.Fatalf("duration points = %d, want 2", len(dur))
			}
			for _, p := range dur {
				wantAttrs(t, p.attrs.ToSlice(), map[string]any{"error.type": tc.want})
			}
			if tok := h.histogram(&rm, "gen_ai.client.token.usage"); len(tok) != 0 {
				t.Errorf("token usage recorded on failure: %+v", tok)
			}
		})
	}
}

// assertNoCanary checks every exported string: span names, attributes,
// status, events, and metric attributes.
func assertNoCanary(t *testing.T, h *harness) {
	t.Helper()
	for _, s := range h.ended() {
		dump := fmt.Sprint(s.Name(), s.Attributes(), s.Status(), s.Events(), s.Links())
		if strings.Contains(dump, "CANARY") {
			t.Errorf("span %s leaks content: %s", s.Name(), dump)
		}
	}
	rm := h.metrics()
	if dump := fmt.Sprintf("%+v", rm); strings.Contains(dump, "CANARY") {
		t.Errorf("metrics leak content: %s", dump)
	}
}

func TestNestedLayersUnderRetry(t *testing.T) {
	h := newHarness(t, llmotel.Options{})
	calls := 0
	fake := &testllm.FakeChatClient{ModelRef: anthropicModel, Func: func(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
		calls++
		if calls == 1 {
			return llms.ChatResponse{}, &llms.ProviderError{Kind: llms.ErrorKindUnavailable, StatusCode: 503}
		}
		return llms.ChatResponse{
			Message: llms.AssistantText("ok"), Text: "ok",
			Usage: llms.Usage{InputTokens: 30, OutputTokens: 5, BillableOutputTokens: 5},
		}, nil
	}}
	retry := middleware.RetryConfig{MaxAttempts: 3, InitialDelay: time.Millisecond, MaxDelay: time.Millisecond, BackoffFactor: 1}
	client := h.tel.ChatCall()(middleware.RecommendedChat(h.tel.Chat()(fake), middleware.RecommendedConfig{Retry: &retry}))
	if _, err := client.Complete(context.Background(), chatReq()); err != nil {
		t.Fatal(err)
	}

	logical := h.span("maestro_llms.call chat claude-test")
	if logical.SpanKind() != trace.SpanKindInternal {
		t.Errorf("logical kind = %v, want internal", logical.SpanKind())
	}
	wantAttrs(t, logical.Attributes(), map[string]any{"gen_ai.operation.name": "chat", "gen_ai.request.model": "claude-test"},
		"gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", "gen_ai.response.id", "maestro_llms.attempt", "error.type")

	var attempts []sdktrace.ReadOnlySpan
	for _, s := range h.ended() {
		if s.Name() == "chat claude-test" {
			attempts = append(attempts, s)
		}
	}
	if len(attempts) != 2 {
		t.Fatalf("attempt spans = %d, want 2 (%v)", len(attempts), h.names())
	}
	for i, s := range attempts {
		if s.Parent().SpanID() != logical.SpanContext().SpanID() {
			t.Errorf("attempt %d parent = %v, want the logical span", i+1, s.Parent().SpanID())
		}
		wantAttrs(t, s.Attributes(), map[string]any{"maestro_llms.attempt": i + 1})
	}
	wantAttrs(t, attempts[0].Attributes(), map[string]any{"error.type": "unavailable"}, "gen_ai.usage.input_tokens")
	wantAttrs(t, attempts[1].Attributes(), map[string]any{"gen_ai.usage.input_tokens": 30}, "error.type")

	rm := h.metrics()
	var durCount uint64
	var failed uint64
	for _, p := range h.histogram(&rm, "gen_ai.client.operation.duration") {
		durCount += p.count
		if v, ok := p.attrs.Value("error.type"); ok && v.AsString() == "unavailable" {
			failed += p.count
		}
	}
	if durCount != 2 || failed != 1 {
		t.Errorf("duration records = %d (failed %d), want exactly one per attempt (2, 1 failed)", durCount, failed)
	}
	sums := map[string]float64{}
	for _, p := range h.histogram(&rm, "gen_ai.client.token.usage") {
		v, _ := p.attrs.Value("gen_ai.token.type")
		sums[v.AsString()] += p.sum
	}
	if sums["input"] != 30 || sums["output"] != 5 {
		t.Errorf("token sums = %v, want the successful attempt's usage only (30/5)", sums)
	}
}

func TestEmbeddings(t *testing.T) {
	h := newHarness(t, llmotel.Options{})
	emb := &testllm.FakeEmbeddingClient{
		ModelRef: llms.ModelRef{Provider: llms.ProviderGoogle, Name: "gemini-embedding"},
		Usage:    llms.Usage{EmbeddingTokens: 12},
	}
	c := h.tel.Embeddings()(emb)
	if c.DefaultDimensions() != emb.DefaultDimensions() || c.Model() != emb.Model() {
		t.Error("embedding decorator must forward Model and DefaultDimensions")
	}
	req := llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{{Text: canary}}, Dimensions: 16, Purpose: llms.PurposeEmbedding}
	if _, err := c.Embed(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	s := h.span("embeddings gemini-embedding")
	if s.SpanKind() != trace.SpanKindClient {
		t.Errorf("kind = %v", s.SpanKind())
	}
	wantAttrs(t, s.Attributes(), map[string]any{
		"gen_ai.operation.name":             "embeddings",
		"gen_ai.provider.name":              "gcp.gen_ai",
		"gen_ai.request.model":              "gemini-embedding",
		"gen_ai.embeddings.dimension.count": 16,
		"gen_ai.usage.input_tokens":         12,
		"maestro_llms.purpose":              "embedding",
	}, "gen_ai.usage.output_tokens")
	rm := h.metrics()
	tok := h.histogram(&rm, "gen_ai.client.token.usage")
	if len(tok) != 1 || tok[0].sum != 12 {
		t.Fatalf("token points = %+v, want one input record of 12", tok)
	}
	wantAttrs(t, tok[0].attrs.ToSlice(), map[string]any{"gen_ai.token.type": "input", "gen_ai.operation.name": "embeddings"})
	assertNoCanary(t, h)
}

func TestProviderName(t *testing.T) {
	for id, want := range map[llms.ProviderID]string{
		llms.ProviderOpenAI: "openai", llms.ProviderAnthropic: "anthropic", llms.ProviderGoogle: "gcp.gen_ai",
		llms.ProviderOllama: "ollama", llms.ProviderOpenAICompat: "openaicompat", "custom": "custom",
	} {
		if got := llmotel.DefaultProviderName(llms.ModelRef{Provider: id}); got != want {
			t.Errorf("DefaultProviderName(%s) = %q, want %q", id, got, want)
		}
	}

	h := newHarness(t, llmotel.Options{ProviderName: func(llms.ModelRef) string { return "gcp.vertex_ai" }})
	fake := &testllm.FakeChatClient{ModelRef: llms.ModelRef{Provider: llms.ProviderGoogle}, Text: "ok"}
	if _, err := h.tel.Chat()(fake).Complete(context.Background(), chatReq()); err != nil {
		t.Fatal(err)
	}
	s := h.span("chat") // no model name: the operation alone
	wantAttrs(t, s.Attributes(), map[string]any{"gen_ai.provider.name": "gcp.vertex_ai"}, "gen_ai.request.model")
}

func echoTool(isError bool, err error) toolloop.Tool {
	return toolloop.Tool{
		Definition: llms.ToolDefinition{Name: "echo", Description: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Execute: func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) {
			return toolloop.ToolResult{Content: canary, IsError: isError}, err
		},
	}
}

func TestRunAndTools(t *testing.T) {
	h := newHarness(t, llmotel.Options{})
	tc := testllm.ToolCallResponse("call-1", "echo", `{"q":"`+canary+`"}`)
	tc.Usage = llms.Usage{InputTokens: 10, BillableOutputTokens: 3}
	final := testllm.TextResponse(canary)
	final.Usage = llms.Usage{InputTokens: 20, BillableOutputTokens: 4}
	fake := &testllm.FakeChatClient{ModelRef: anthropicModel, Responses: []llms.ChatResponse{tc, final}}

	out := h.tel.Run(context.Background(), toolloop.Config{
		Request: chatReq(),
		Client:  h.tel.Chat()(fake),
		Tools:   h.tel.Tools([]toolloop.Tool{echoTool(true, nil)}),
		// A compactor that changes nothing but reports a summarizer's usage:
		// the run total includes it (TotalUsage + CompactionUsage).
		Compact: func(context.Context, toolloop.CompactInput) (toolloop.CompactResult, error) {
			return toolloop.CompactResult{Usage: llms.Usage{InputTokens: 100, BillableOutputTokens: 50}}, nil
		},
	}, llmotel.RunOptions{AgentName: "reviewer"})
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("outcome = %s (%v)", out.Kind, out.Err)
	}
	if out.CompactionUsage.InputTokens < 100 || out.CompactionUsage.BillableOutputTokens < 50 {
		t.Fatalf("compactor usage not reported: %+v", out.CompactionUsage)
	}

	run := h.span("invoke_agent reviewer")
	if run.SpanKind() != trace.SpanKindInternal || run.Status().Code != codes.Unset {
		t.Errorf("run span kind/status = %v/%v", run.SpanKind(), run.Status())
	}
	wantAttrs(t, run.Attributes(), map[string]any{
		"gen_ai.operation.name":            "invoke_agent",
		"gen_ai.agent.name":                "reviewer",
		"gen_ai.provider.name":             "anthropic",
		"gen_ai.request.model":             "claude-test",
		"maestro_llms.toolloop.outcome":    "final_answer",
		"maestro_llms.toolloop.iterations": 1, // responses with tool calls
		"gen_ai.usage.input_tokens":        30 + out.CompactionUsage.InputTokens,
		"gen_ai.usage.output_tokens":       7 + out.CompactionUsage.BillableOutputTokens,
	}, "error.type")

	tool := h.span("execute_tool echo")
	if tool.SpanKind() != trace.SpanKindInternal {
		t.Errorf("tool kind = %v", tool.SpanKind())
	}
	wantAttrs(t, tool.Attributes(), map[string]any{
		"gen_ai.operation.name": "execute_tool", "gen_ai.tool.name": "echo",
		"gen_ai.tool.call.id": "call-1", "gen_ai.tool.type": "function", "error.type": "tool_error",
	})
	if tool.Status().Code != codes.Error {
		t.Errorf("IsError tool status = %v, want Error", tool.Status())
	}

	chats := 0
	for _, s := range h.ended() {
		if s.Name() == "chat claude-test" || s.Name() == "execute_tool echo" {
			if s.Parent().SpanID() != run.SpanContext().SpanID() {
				t.Errorf("%s is not a child of the run span", s.Name())
			}
			if s.Name() == "chat claude-test" {
				chats++
			}
		}
	}
	if chats != 2 {
		t.Errorf("chat spans = %d, want 2", chats)
	}
	assertNoCanary(t, h)
}

func TestToolExecuteError(t *testing.T) {
	h := newHarness(t, llmotel.Options{})
	tools := h.tel.Tools([]toolloop.Tool{echoTool(false, errors.New(canary))})
	if _, err := tools[0].Execute(context.Background(), llms.ToolCall{Name: "echo"}); err == nil {
		t.Fatal("want the Execute error passed through")
	}
	s := h.span("execute_tool echo")
	wantAttrs(t, s.Attributes(), map[string]any{"error.type": "_OTHER"}, "gen_ai.tool.call.id")
	if s.Status().Code != codes.Error || len(s.Events()) != 0 {
		t.Errorf("status = %v, events = %v", s.Status(), s.Events())
	}
	assertNoCanary(t, h)

	h2 := newHarness(t, llmotel.Options{})
	ok := h2.tel.Tools([]toolloop.Tool{echoTool(false, nil)})
	if _, err := ok[0].Execute(context.Background(), llms.ToolCall{ID: "c", Name: "echo"}); err != nil {
		t.Fatal(err)
	}
	s2 := h2.span("execute_tool echo")
	if s2.Status().Code != codes.Unset {
		t.Errorf("successful tool span status = %v, want unset", s2.Status())
	}
	wantAttrs(t, s2.Attributes(), map[string]any{"gen_ai.tool.call.id": "c"}, "error.type")
}

func TestToolsEdgeCases(t *testing.T) {
	h := newHarness(t, llmotel.Options{})
	if h.tel.Tools(nil) != nil {
		t.Error("Tools(nil) should be nil")
	}
	in := []toolloop.Tool{{Definition: llms.ToolDefinition{Name: "noexec"}}}
	out := h.tel.Tools(in)
	if len(out) != 1 || out[0].Execute != nil || out[0].Definition.Name != "noexec" {
		t.Errorf("nil Execute should be copied as-is: %+v", out)
	}
	orig := echoTool(false, nil)
	wrapped := h.tel.Tools([]toolloop.Tool{orig})
	if wrapped[0].Definition.Name != orig.Definition.Name {
		t.Error("definition changed")
	}
}

func TestRunFailureOutcome(t *testing.T) {
	h := newHarness(t, llmotel.Options{})
	fake := &testllm.FakeChatClient{ModelRef: anthropicModel, Err: &llms.ProviderError{Kind: llms.ErrorKindBadRequest, Message: canary}}
	out := h.tel.Run(context.Background(), toolloop.Config{Request: chatReq(), Client: fake}, llmotel.RunOptions{})
	if out.Kind != toolloop.OutcomeLLMError {
		t.Fatalf("outcome = %s", out.Kind)
	}
	run := h.span("invoke_agent")
	if run.Status().Code != codes.Error || run.Status().Description != "llm_error" {
		t.Errorf("status = %+v", run.Status())
	}
	wantAttrs(t, run.Attributes(), map[string]any{"error.type": "llm_error", "maestro_llms.toolloop.outcome": "llm_error"},
		"gen_ai.agent.name", "gen_ai.usage.input_tokens")
	assertNoCanary(t, h)
}

func TestGlobalProvidersByDefault(t *testing.T) {
	tel, err := llmotel.New(llmotel.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fake := &testllm.FakeChatClient{ModelRef: anthropicModel, Text: "ok"}
	if _, err := tel.Chat()(fake).Complete(context.Background(), chatReq()); err != nil {
		t.Fatal(err)
	}
}
