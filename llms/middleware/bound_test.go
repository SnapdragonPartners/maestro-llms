package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/ratelimit"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
)

var _ InputBounder = DefaultEstimator{}

var (
	anthropicModel = llms.ModelRef{Provider: llms.ProviderAnthropic, Name: "claude"}
	openaiModel    = llms.ModelRef{Provider: llms.ProviderOpenAI, Name: "gpt"}
	googleModel    = llms.ModelRef{Provider: llms.ProviderGoogle, Name: "gemini"}
)

func mustBound(t *testing.T, m llms.ModelRef, req llms.ChatRequest) ratelimit.UsageUnits {
	t.Helper()
	u, ok, err := DefaultEstimator{}.BoundChat(m, req)
	if err != nil || !ok {
		t.Fatalf("BoundChat(%s): ok=%v err=%v, want a bound", m.Provider, ok, err)
	}
	return u
}

func assertNoBound(t *testing.T, m llms.ModelRef, req llms.ChatRequest) {
	t.Helper()
	u, ok, err := DefaultEstimator{}.BoundChat(m, req)
	if err != nil || ok {
		t.Fatalf("BoundChat(%s) = (%+v, %v, %v), want no bound", m.Provider, u, ok, err)
	}
}

func TestBoundChatTextIsBytesPlusMargins(t *testing.T) {
	req := llms.ChatRequest{
		System:    []llms.ContentPart{llms.Text("be brief")},                        // 8 bytes
		Messages:  []llms.Message{llms.UserText("héllo"), llms.AssistantText("hi")}, // 6 + 2 bytes
		MaxTokens: 100,
	}
	want := boundPerRequest + (boundPerPart + 8) +
		2*boundPerMessage + (boundPerPart + 6) + (boundPerPart + 2)
	for _, m := range []llms.ModelRef{anthropicModel, openaiModel, googleModel} {
		u := mustBound(t, m, req)
		if u.InputTokens != want || u.OutputTokensMax != 100 || u.TotalTokensMax != want+100 || u.Requests != 1 {
			t.Fatalf("%s: bound = %+v, want input %d, output 100, total %d", m.Provider, u, want, want+100)
		}
	}
}

// TestBoundChatAtLeastEstimate keeps the bound coherent with the estimate: a
// bound below an estimate of the same request would be a bug in one of them.
func TestBoundChatAtLeastEstimate(t *testing.T) {
	req := llms.ChatRequest{
		System: []llms.ContentPart{llms.Text("You are terse.")},
		Messages: []llms.Message{
			llms.UserText("What is the weather in Paris, and should I bring an umbrella?"),
			{Role: llms.RoleAssistant, Content: []llms.ContentPart{
				{Type: llms.ContentReasoning, Reasoning: &llms.Reasoning{Provider: llms.ProviderAnthropic, Text: "check weather", ProviderData: []byte(`{"signature":"abc"}`)}},
				{Type: llms.ContentToolCall, ToolCall: &llms.ToolCall{ID: "toolu_01", Name: "weather", Parameters: json.RawMessage(`{"city":"Paris"}`)}},
			}},
			llms.ToolResultMessage(llms.ToolResult{ToolCallID: "toolu_01", Content: "rain, 12C"}),
		},
		Tools:     []llms.ToolDefinition{{Name: "weather", Description: "Current weather.", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		MaxTokens: 512,
	}
	est := DefaultEstimator{}.EstimateChat(req)
	b := mustBound(t, anthropicModel, req)
	if b.InputTokens < est.InputTokens || b.TotalTokensMax < est.TotalTokensMax {
		t.Fatalf("bound %+v below estimate %+v", b, est)
	}
}

func TestBoundChatCountsEveryField(t *testing.T) {
	base := llms.ChatRequest{Messages: []llms.Message{llms.UserText("x")}, MaxTokens: 10}
	baseline := mustBound(t, anthropicModel, base).InputTokens
	grow := func(name string, msg llms.Message, wantExtra int) {
		t.Helper()
		req := base
		req.Messages = append([]llms.Message{msg}, base.Messages...)
		got := mustBound(t, anthropicModel, req).InputTokens - baseline
		if got != wantExtra {
			t.Fatalf("%s: bound grew by %d, want %d", name, got, wantExtra)
		}
	}
	grow("tool call", llms.Message{Role: llms.RoleAssistant, Content: []llms.ContentPart{
		{Type: llms.ContentToolCall, ToolCall: &llms.ToolCall{ID: "id12", Name: "nm", Parameters: json.RawMessage(`{}`), ProviderSignature: []byte("sig")}},
	}}, boundPerMessage+boundPerPart+4+2+2+4) // "sig" is sent as base64 "c2ln"
	// With no matching call in the history, a result's ID is counted twice:
	// Gemini sends it again as the function name.
	grow("tool result content", llms.ToolResultMessage(llms.ToolResult{ToolCallID: "id12", Content: "abcde"}),
		boundPerMessage+boundPerPart+4+4+5)
	grow("tool result parts", llms.ToolResultMessage(llms.ToolResult{ToolCallID: "id12", Parts: []llms.ContentPart{llms.Text("abc")}}),
		boundPerMessage+boundPerPart+4+4+boundPerPart+3)
	grow("reasoning", llms.Message{Role: llms.RoleAssistant, Content: []llms.ContentPart{
		{Type: llms.ContentReasoning, Reasoning: &llms.Reasoning{Provider: llms.ProviderAnthropic, Text: "ab", ProviderData: []byte("opaque")}},
	}}, boundPerMessage+boundPerPart+2+6)
}

func TestBoundChatTools(t *testing.T) {
	tool := llms.ToolDefinition{Name: "t", Description: "dd", InputSchema: json.RawMessage(`{}`)}
	req := llms.ChatRequest{Messages: []llms.Message{llms.UserText("x")}, Tools: []llms.ToolDefinition{tool}, MaxTokens: 10}
	noTools := req
	noTools.Tools = nil

	got := mustBound(t, anthropicModel, req).InputTokens - mustBound(t, anthropicModel, noTools).InputTokens
	if want := anthropicToolOverhead + boundPerTool + 1 + 2 + 2; got != want {
		t.Fatalf("Anthropic tools add %d, want %d", got, want)
	}
	forced := req
	forced.ToolChoice = llms.ToolChoice{Type: llms.ToolChoiceTool, Name: "t"}
	if d := mustBound(t, anthropicModel, forced).InputTokens - mustBound(t, anthropicModel, req).InputTokens; d != 1 {
		t.Fatalf("forced tool name adds %d, want 1", d)
	}
	// OpenAI and Google document no hidden tool overhead.
	assertNoBound(t, openaiModel, req)
	assertNoBound(t, googleModel, req)
}

func TestBoundChatNoBound(t *testing.T) {
	ok := llms.ChatRequest{Messages: []llms.Message{llms.UserText("x")}, MaxTokens: 10}
	mustBound(t, anthropicModel, ok)

	noMax := ok
	noMax.MaxTokens = 0
	assertNoBound(t, anthropicModel, noMax)

	params := ok
	params.ProviderParams = map[llms.ProviderID]map[string]any{llms.ProviderAnthropic: {"x": 1}}
	assertNoBound(t, anthropicModel, params)

	for _, p := range []llms.ProviderID{llms.ProviderOllama, llms.ProviderOpenAICompat, "other"} {
		assertNoBound(t, llms.ModelRef{Provider: p, Name: "m"}, ok)
	}

	img := &llms.Media{MediaType: "image/png", Data: pngOf(t, 8, 8)}
	withImage := ok
	withImage.Messages = []llms.Message{{Role: llms.RoleUser, Content: []llms.ContentPart{{Type: llms.ContentImage, Media: img}}}}
	assertNoBound(t, anthropicModel, withImage)

	resultImage := ok
	resultImage.Messages = []llms.Message{llms.ToolResultMessage(llms.ToolResult{ToolCallID: "a", Parts: []llms.ContentPart{{Type: llms.ContentImage, Media: img}}})}
	assertNoBound(t, anthropicModel, resultImage)

	doc := ok
	doc.Messages = []llms.Message{{Role: llms.RoleUser, Content: []llms.ContentPart{{Type: llms.ContentDocument, Media: &llms.Media{MediaType: "text/plain", Data: []byte("hi")}}}}}
	assertNoBound(t, anthropicModel, doc)
}

func TestBoundEmbeddingsNone(t *testing.T) {
	u, ok, err := DefaultEstimator{}.BoundEmbeddings(openaiModel, llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{{ID: "1", Text: "a"}}})
	if ok || err != nil {
		t.Fatalf("BoundEmbeddings = (%+v, %v, %v), want no bound", u, ok, err)
	}
}

// captureLimiter records each ReservationRequest and admits it.
type captureLimiter struct {
	reqs []ratelimit.ReservationRequest
}

func (c *captureLimiter) Reserve(_ context.Context, req ratelimit.ReservationRequest) (ratelimit.Reservation, error) {
	c.reqs = append(c.reqs, req)
	return nopReservation{}, nil
}

type nopReservation struct{}

func (nopReservation) Commit(context.Context, llms.Usage) error { return nil }
func (nopReservation) Release(context.Context) error            { return nil }

// estimateOnly is a TokenEstimator without InputBounder. It wraps rather
// than embeds DefaultEstimator: embedding would promote BoundChat.
type estimateOnly struct{ d DefaultEstimator }

func (e estimateOnly) EstimateChat(req llms.ChatRequest) ratelimit.UsageUnits {
	return e.d.EstimateChat(req)
}

func (e estimateOnly) EstimateEmbeddings(req llms.EmbeddingRequest) ratelimit.UsageUnits {
	return e.d.EstimateEmbeddings(req)
}

// customEstimate embeds DefaultEstimator and overrides only EstimateChat, the
// usual way to customize the estimate.
type customEstimate struct{ DefaultEstimator }

func (customEstimate) EstimateChat(llms.ChatRequest) ratelimit.UsageUnits {
	return ratelimit.UsageUnits{InputTokens: 1, Requests: 1}
}

// TestEmbeddedDefaultEstimatorInheritsBound pins the documented behavior: an
// estimator that embeds DefaultEstimator inherits its bound (ADR-0043).
func TestEmbeddedDefaultEstimatorInheritsBound(t *testing.T) {
	if _, ok := any(customEstimate{}).(InputBounder); !ok {
		t.Fatal("an estimator embedding DefaultEstimator should inherit InputBounder")
	}
}

// failingBounder's BoundChat and BoundEmbeddings fail.
type failingBounder struct{ DefaultEstimator }

var errBound = errors.New("bound store down")

func (failingBounder) BoundChat(llms.ModelRef, llms.ChatRequest) (ratelimit.UsageUnits, bool, error) {
	return ratelimit.UsageUnits{}, false, errBound
}

func (failingBounder) BoundEmbeddings(llms.ModelRef, llms.EmbeddingRequest) (ratelimit.UsageUnits, bool, error) {
	return ratelimit.UsageUnits{}, false, errBound
}

func TestRateLimitChatPassesBound(t *testing.T) {
	req := llms.ChatRequest{Messages: []llms.Message{llms.UserText("hello")}, MaxTokens: 50}
	fake := &testllm.FakeChatClient{ModelRef: anthropicModel, Text: "ok"}

	lim := &captureLimiter{}
	if _, err := RateLimitChat(lim, nil)(fake).Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	want := mustBound(t, anthropicModel, req)
	if b := lim.reqs[0].BoundUnits; b == nil || *b != want {
		t.Fatalf("BoundUnits = %v, want %+v (nil estimator selects DefaultEstimator)", b, want)
	}

	// No bound for this request: nil, not a zero bound.
	noMax := req
	noMax.MaxTokens = 0
	if _, err := RateLimitChat(lim, nil)(fake).Complete(context.Background(), noMax); err != nil {
		t.Fatal(err)
	}
	if b := lim.reqs[1].BoundUnits; b != nil {
		t.Fatalf("BoundUnits = %+v, want nil when no bound is established", *b)
	}

	// An estimator without InputBounder supplies no bound.
	if _, err := RateLimitChat(lim, estimateOnly{})(fake).Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if b := lim.reqs[2].BoundUnits; b != nil {
		t.Fatalf("BoundUnits = %+v, want nil for an estimator without InputBounder", *b)
	}
}

func TestRateLimitBounderErrorStopsBeforeReserve(t *testing.T) {
	lim := &captureLimiter{}
	fake := &testllm.FakeChatClient{ModelRef: anthropicModel, Text: "ok"}
	_, err := RateLimitChat(lim, failingBounder{})(fake).Complete(context.Background(), userReq("x"))
	if !errors.Is(err, errBound) {
		t.Fatalf("err = %v, want the bounder's error", err)
	}
	if len(lim.reqs) != 0 || len(fake.Calls()) != 0 {
		t.Fatalf("reserved %d, called %d; want neither after a bounder error", len(lim.reqs), len(fake.Calls()))
	}

	emb := &testllm.FakeEmbeddingClient{Dims: 4}
	_, err = RateLimitEmbeddings(lim, failingBounder{})(emb).Embed(context.Background(),
		llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{{ID: "1", Text: "a"}}})
	if !errors.Is(err, errBound) || len(lim.reqs) != 0 {
		t.Fatalf("embeddings: err = %v, reserved %d; want the bounder's error and no reserve", err, len(lim.reqs))
	}
}

// The wire-expansion cases (ADR-0043): each field counts at the size an
// adapter can put on the wire, not its size in the request.

func TestBoundChatToolSchemaReencoded(t *testing.T) {
	// Anthropic re-encodes a tool schema; 1e20 becomes 21 digits.
	schema := json.RawMessage(`{"type":"object","properties":{"n":{"type":"number","maximum":1e20}}}`)
	reencoded := `{"properties":{"n":{"maximum":100000000000000000000,"type":"number"}},"type":"object"}`
	withTool := func(s json.RawMessage) llms.ChatRequest {
		return llms.ChatRequest{
			Messages:  []llms.Message{llms.UserText("x")},
			Tools:     []llms.ToolDefinition{{Name: "t", InputSchema: s}},
			MaxTokens: 10,
		}
	}
	got := mustBound(t, anthropicModel, withTool(schema)).InputTokens -
		mustBound(t, anthropicModel, withTool(json.RawMessage(`{}`))).InputTokens
	if want := len(reencoded) - len(`{}`); got != want {
		t.Fatalf("schema counted %d bytes over {}, want the re-encoded %d (raw is %d)", got, want, len(schema)-2)
	}
}

func TestBoundChatToolArgumentsReencoded(t *testing.T) {
	// Gemini decodes tool call arguments into a map that genai re-encodes.
	call := func(args string) llms.ChatRequest {
		return llms.ChatRequest{Messages: []llms.Message{
			{Role: llms.RoleAssistant, Content: []llms.ContentPart{{Type: llms.ContentToolCall,
				ToolCall: &llms.ToolCall{ID: "c", Name: "f", Parameters: json.RawMessage(args)}}}},
		}, MaxTokens: 10}
	}
	got := mustBound(t, googleModel, call(`{"v":1e20}`)).InputTokens - mustBound(t, googleModel, call(`{}`)).InputTokens
	if want := len(`{"v":100000000000000000000}`) - len(`{}`); got != want {
		t.Fatalf("arguments counted %d bytes over {}, want the re-encoded %d", got, want)
	}
}

func TestBoundChatToolResultResendsName(t *testing.T) {
	name := "a_rather_long_function_name_for_gemini"
	req := llms.ChatRequest{Messages: []llms.Message{
		{Role: llms.RoleAssistant, Content: []llms.ContentPart{{Type: llms.ContentToolCall,
			ToolCall: &llms.ToolCall{ID: "c1", Name: name, Parameters: json.RawMessage(`{}`)}}}},
		llms.ToolResultMessage(llms.ToolResult{ToolCallID: "c1", Content: "ok"}),
	}, MaxTokens: 10}
	callOnly := req
	callOnly.Messages = req.Messages[:1]
	got := mustBound(t, googleModel, req).InputTokens - mustBound(t, googleModel, callOnly).InputTokens
	// The result message: its framing, the ID, the resolved name sent
	// again, and the content.
	if want := boundPerMessage + boundPerPart + len("c1") + len(name) + len("ok"); got != want {
		t.Fatalf("tool result counted %d, want %d including the resolved name", got, want)
	}
}

func TestBoundChatSignatureBase64(t *testing.T) {
	sig := make([]byte, 300) // 400 bytes as base64
	withSig := func(sig []byte) llms.ChatRequest {
		return llms.ChatRequest{Messages: []llms.Message{
			{Role: llms.RoleAssistant, Content: []llms.ContentPart{{Type: llms.ContentToolCall,
				ToolCall: &llms.ToolCall{ID: "c", Name: "f", Parameters: json.RawMessage(`{}`), ProviderSignature: sig}}}},
		}, MaxTokens: 10}
	}
	if got := mustBound(t, googleModel, withSig(sig)).InputTokens - mustBound(t, googleModel, withSig(nil)).InputTokens; got != 400 {
		t.Fatalf("300-byte signature counted %d, want its base64 length 400", got)
	}
}
