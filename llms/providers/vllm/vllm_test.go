package vllm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// Compile-time assertions: vllm.Client satisfies both ChatClient and the
// optional ModelLister capability. LatestInFamily is intentionally absent
// (ADR-0015); TestNoLatestInFamilyMethod is the regression guard.
var (
	_ llms.ChatClient  = (*Client)(nil)
	_ llms.ModelLister = (*Client)(nil)
)

// newClient builds a Client whose requests hit the given handler instead of
// a real vLLM server. It does NOT set an API key — exercising vLLM's
// no-auth default deployment path.
func newClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(
		WithBaseURL(srv.URL),
		WithModel("test-model"),
		WithHTTPClient(srv.Client()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func respondJSON(t *testing.T, status int, body string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

const textCompletionJSON = `{
  "id":"chatcmpl-1","object":"chat.completion","created":1730000000,"model":"test-model",
  "choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hello world"}}],
  "usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}
}`

func TestNewRejectsMissingConfig(t *testing.T) {
	t.Run("no base URL", func(t *testing.T) {
		_, err := New(WithModel("m"))
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindConfig {
			t.Fatalf("want config error, got %v", err)
		}
	})
	t.Run("no model", func(t *testing.T) {
		_, err := New(WithBaseURL("http://localhost:8000"))
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindConfig {
			t.Fatalf("want config error, got %v", err)
		}
	})
	t.Run("negative retries", func(t *testing.T) {
		_, err := New(WithBaseURL("http://localhost:8000"), WithModel("m"), WithMaxRetries(-1))
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindConfig {
			t.Fatalf("want config error, got %v", err)
		}
	})
	t.Run("empty API key is allowed", func(t *testing.T) {
		// Distinguishing feature vs hosted providers (see ADR-0015): vLLM's
		// default deployment has no auth, so empty key must NOT error here.
		_, err := New(WithBaseURL("http://localhost:8000"), WithModel("m"))
		if err != nil {
			t.Fatalf("empty API key should be allowed for vLLM, got %v", err)
		}
	})
}

func TestCompleteTextResponse(t *testing.T) {
	c := newClient(t, respondJSON(t, 200, textCompletionJSON))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// Message is the source of truth.
	if resp.Message.Role != llms.RoleAssistant ||
		len(resp.Message.Content) != 1 ||
		resp.Message.Content[0].Type != llms.ContentText ||
		resp.Message.Content[0].Text != "hello world" {
		t.Fatalf("unexpected message: %+v", resp.Message)
	}
	if resp.Text != "hello world" {
		t.Fatalf("Text mirror = %q", resp.Text)
	}
	if resp.StopReason != "stop" {
		t.Fatalf("StopReason = %q (raw vLLM finish_reason expected)", resp.StopReason)
	}
	if u := resp.Usage; u.InputTokens != 11 || u.OutputTokens != 7 || u.TotalTokens != 18 || u.ProviderRequestID != "chatcmpl-1" {
		t.Fatalf("usage mapping wrong: %+v", u)
	}
	if resp.Raw == nil {
		t.Fatal("Raw should carry the SDK ChatCompletion payload")
	}
}

func TestCompleteForwardsSystemAndTemperature(t *testing.T) {
	var got map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, textCompletionJSON)
	})
	// Use 0.5 (exactly representable in float32) so the assertion is not
	// fighting the float32→float64 promotion the SDK does internally.
	temp := float32(0.5)
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		System:      []llms.ContentPart{llms.Text("be brief")},
		Messages:    []llms.Message{llms.UserText("hi")},
		MaxTokens:   64,
		Temperature: &temp,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("expected 2 messages on wire (system + user), got %d", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be brief" {
		t.Errorf("first wire message = %+v, want system / be brief", first)
	}
	if mt, _ := got["max_tokens"].(float64); mt != 64 {
		t.Errorf("max_tokens = %v", got["max_tokens"])
	}
	if tp, _ := got["temperature"].(float64); tp != 0.5 {
		t.Errorf("temperature = %v", got["temperature"])
	}
}

func TestCompleteToolCallResponse(t *testing.T) {
	// Model emits a tool call. The response should populate both
	// Message.Content[…ToolCall] and the ToolCalls mirror; StopReason is
	// the raw vLLM finish_reason ("tool_calls").
	body := `{
  "id":"chatcmpl-2","object":"chat.completion","created":1730000000,"model":"test-model",
  "choices":[{"index":0,"finish_reason":"tool_calls","message":{
    "role":"assistant","content":"",
    "tool_calls":[{"id":"call_1","type":"function","function":{"name":"echo","arguments":"{\"x\":1}"}}]
  }}],
  "usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}
}`
	c := newClient(t, respondJSON(t, 200, body))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("call echo")},
		Tools: []llms.ToolDefinition{{
			Name:        "echo",
			Description: "echo args",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"integer"}}}`),
		}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "echo" || string(tc.Parameters) != `{"x":1}` {
		t.Fatalf("ToolCall = %+v", tc)
	}
	if resp.StopReason != "tool_calls" {
		t.Errorf("StopReason = %q, want tool_calls", resp.StopReason)
	}
	// Message.Content also carries the call (round-trip source of truth).
	found := false
	for _, p := range resp.Message.Content {
		if p.Type == llms.ContentToolCall && p.ToolCall.ID == "call_1" {
			found = true
		}
	}
	if !found {
		t.Fatal("Message.Content missing tool_call part")
	}
}

func TestToolRoundTripWireShape(t *testing.T) {
	// Send back a tool_result; verify the wire request has the expected
	// shape: prior assistant message with tool_calls, then a tool-role
	// message with tool_call_id.
	var got map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, textCompletionJSON)
	})
	priorCall := llms.ToolCall{ID: "call_1", Name: "echo", Parameters: json.RawMessage(`{"x":1}`)}
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{
			llms.UserText("call echo"),
			{Role: llms.RoleAssistant, Content: []llms.ContentPart{
				{Type: llms.ContentToolCall, ToolCall: &priorCall},
			}},
			llms.ToolResultMessage(llms.ToolResult{
				ToolCallID: "call_1",
				Content:    `{"echoed":1}`,
			}),
		},
		Tools: []llms.ToolDefinition{{Name: "echo"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("wire messages = %d, want 3 (user + assistant + tool)", len(msgs))
	}
	asst, _ := msgs[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Errorf("msg[1].role = %v, want assistant", asst["role"])
	}
	calls, ok := asst["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("msg[1].tool_calls = %v", asst["tool_calls"])
	}
	tool, _ := msgs[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" || tool["content"] != `{"echoed":1}` {
		t.Errorf("msg[2] = %+v, want tool/call_1/{echoed:1}", tool)
	}
}

func TestToolChoiceMapping(t *testing.T) {
	cases := []struct {
		name string
		tc   llms.ToolChoice
		want any
	}{
		{"auto", llms.ToolChoice{Type: llms.ToolChoiceAuto}, "auto"},
		{"none", llms.ToolChoice{Type: llms.ToolChoiceNone}, "none"},
		{"required", llms.ToolChoice{Type: llms.ToolChoiceRequired}, "required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&got)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, textCompletionJSON)
			})
			_, err := c.Complete(context.Background(), llms.ChatRequest{
				Messages:   []llms.Message{llms.UserText("hi")},
				Tools:      []llms.ToolDefinition{{Name: "echo"}},
				ToolChoice: tc.tc,
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if got["tool_choice"] != tc.want {
				t.Errorf("tool_choice = %v, want %v", got["tool_choice"], tc.want)
			}
		})
	}

	t.Run("named tool", func(t *testing.T) {
		var got map[string]any
		c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, textCompletionJSON)
		})
		_, err := c.Complete(context.Background(), llms.ChatRequest{
			Messages:   []llms.Message{llms.UserText("hi")},
			Tools:      []llms.ToolDefinition{{Name: "echo"}},
			ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceTool, Name: "echo"},
		})
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		tcMap, ok := got["tool_choice"].(map[string]any)
		if !ok {
			t.Fatalf("tool_choice = %T (%v), want object", got["tool_choice"], got["tool_choice"])
		}
		fn, _ := tcMap["function"].(map[string]any)
		if fn["name"] != "echo" {
			t.Errorf("tool_choice.function.name = %v", fn["name"])
		}
	})
}

// TestRejectsMalformedMessages covers the conversion-time guard rails
// brought into line with the other adapters: empty content slices, empty
// text parts, and tool schemas that unmarshal to nil (e.g. literal `null`).
// These would otherwise either reach the vLLM server as wire-invalid
// requests or — for the nil schema — panic at write-into-nil-map.
func TestRejectsMalformedMessages(t *testing.T) {
	cases := []struct {
		name    string
		req     llms.ChatRequest
		wantSub string
	}{
		{
			name:    "user message with empty content slice",
			req:     llms.ChatRequest{Messages: []llms.Message{{Role: llms.RoleUser}}},
			wantSub: "user message has empty content",
		},
		{
			name: "user message with empty text part",
			req: llms.ChatRequest{Messages: []llms.Message{
				{Role: llms.RoleUser, Content: []llms.ContentPart{{Type: llms.ContentText, Text: ""}}},
			}},
			wantSub: "user message has an empty text part",
		},
		{
			name: "assistant message with empty content slice",
			req: llms.ChatRequest{Messages: []llms.Message{
				llms.UserText("hi"),
				{Role: llms.RoleAssistant},
			}},
			wantSub: "assistant message has empty content",
		},
		{
			name: "assistant message with empty text part",
			req: llms.ChatRequest{Messages: []llms.Message{
				llms.UserText("hi"),
				{Role: llms.RoleAssistant, Content: []llms.ContentPart{{Type: llms.ContentText, Text: ""}}},
			}},
			wantSub: "assistant message has an empty text part",
		},
		{
			name: "tool schema unmarshalling to nil (json null)",
			req: llms.ChatRequest{
				Messages: []llms.Message{llms.UserText("hi")},
				Tools:    []llms.ToolDefinition{{Name: "broken", InputSchema: json.RawMessage(`null`)}},
			},
			wantSub: "must be a JSON object",
		},
		{
			name: "tool schema with non-object type field",
			req: llms.ChatRequest{
				Messages: []llms.Message{llms.UserText("hi")},
				Tools:    []llms.ToolDefinition{{Name: "broken", InputSchema: json.RawMessage(`{"type":"array"}`)}},
			},
			wantSub: `type must be "object"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, respondJSON(t, 200, textCompletionJSON))
			_, err := c.Complete(context.Background(), tc.req)
			var pe *llms.ProviderError
			if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
				t.Fatalf("want bad_request error, got %v", err)
			}
			if !strings.Contains(pe.Message, tc.wantSub) {
				t.Errorf("error %q does not contain %q", pe.Message, tc.wantSub)
			}
		})
	}
}

func TestRequiresToolsWithNoTools(t *testing.T) {
	c := newClient(t, respondJSON(t, 200, textCompletionJSON))
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages:   []llms.Message{llms.UserText("hi")},
		ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceRequired},
	})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
		t.Fatalf("want bad_request error, got %v", err)
	}
	if !strings.Contains(pe.Message, "requires at least one tool") {
		t.Errorf("error message = %q", pe.Message)
	}
}

func TestClassifiesAuthError(t *testing.T) {
	c := newClient(t, respondJSON(t, 401, `{"error":{"message":"invalid key","type":"unauthorized"}}`))
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("hi")},
	})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *llms.ProviderError, got %T: %v", err, err)
	}
	if pe.Kind != llms.ErrorKindAuth {
		t.Errorf("Kind = %q, want auth", pe.Kind)
	}
}

func TestClassifiesServerError(t *testing.T) {
	c := newClient(t, respondJSON(t, 503, `{"error":{"message":"backend overloaded"}}`))
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("hi")},
	})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *llms.ProviderError, got %T: %v", err, err)
	}
	if !llms.Retryable(pe) {
		t.Errorf("503 should be retryable, got %v", pe)
	}
}

// TestNoLatestInFamilyMethod is a regression guard: ADR-0015 says vLLM
// implements ModelLister only and does NOT provide a LatestInFamily helper.
// HuggingFace-style names have no canonical family; if this ever changes
// it's a structural decision that needs an ADR amendment, not a quiet add.
func TestNoLatestInFamilyMethod(t *testing.T) {
	var c any = (*Client)(nil)
	if _, ok := c.(interface {
		LatestInFamily(context.Context, string) (llms.ModelInfo, bool, error)
	}); ok {
		t.Fatal("vLLM *Client unexpectedly implements LatestInFamily; ADR-0015 says it should NOT")
	}
}

// TestUsageReasoningTokensNormalized pins ADR-0016 for vLLM via Chat
// Completions: if a served model exposes reasoning_tokens in its
// usage details, OutputTokens is normalized to visible-only by
// subtraction and BillableOutputTokens preserves the wire total. Most
// served models won't populate this; future reasoning-capable models
// inherit the right shape without an adapter change.
func TestUsageReasoningTokensNormalized(t *testing.T) {
	body := `{"id":"chatcmpl-r","object":"chat.completion","created":1730000000,"model":"test-model",
"choices":[{"index":0,"finish_reason":"length","message":{"role":"assistant","content":"short"}}],
"usage":{"prompt_tokens":40,"completion_tokens":250,"total_tokens":290,
 "completion_tokens_details":{"reasoning_tokens":200}}}`
	c := newClient(t, respondJSON(t, 200, body))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	u := resp.Usage
	if u.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50 (visible)", u.OutputTokens)
	}
	if u.ReasoningTokens != 200 {
		t.Errorf("ReasoningTokens = %d, want 200", u.ReasoningTokens)
	}
	if u.BillableOutputTokens != 250 {
		t.Errorf("BillableOutputTokens = %d, want 250", u.BillableOutputTokens)
	}
}
