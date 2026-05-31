package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

var _ llms.ChatClient = (*ChatClient)(nil)

const respTextJSON = `{"id":"resp_1","object":"response","status":"completed","model":"gpt-test",
"output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed",
 "content":[{"type":"output_text","text":"hello world","annotations":[]}]}],
"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}`

func newChat(t *testing.T, h http.HandlerFunc) *ChatClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewChat(WithAPIKey("k"), WithModel("gpt-test"),
		WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	return c
}

func jsonHandler(t *testing.T, status int, body string, hdr map[string]string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func TestChatTextResponse(t *testing.T) {
	c := newChat(t, jsonHandler(t, 200, respTextJSON, nil))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Message.Role != llms.RoleAssistant || len(resp.Message.Content) != 1 ||
		resp.Message.Content[0].Text != "hello world" {
		t.Fatalf("message not source of truth: %+v", resp.Message)
	}
	if resp.Text != "hello world" || resp.StopReason != "completed" {
		t.Fatalf("text/stop mirror wrong: %q %q", resp.Text, resp.StopReason)
	}
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 7 ||
		resp.Usage.TotalTokens != 18 || resp.Usage.ProviderRequestID != "resp_1" {
		t.Fatalf("usage mapping wrong: %+v", resp.Usage)
	}
	if resp.Raw == nil || c.Model().Provider != "openai" || c.Model().Name != "gpt-test" {
		t.Fatalf("raw/model wrong: raw=%v model=%+v", resp.Raw, c.Model())
	}
}

func TestChatToolCallResponse(t *testing.T) {
	body := `{"id":"resp_2","object":"response","status":"completed","model":"gpt-test",
"output":[{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"lookup","arguments":"{\"q\":\"Dan\"}","status":"completed"}],
"usage":{"input_tokens":5,"output_tokens":9,"total_tokens":14}}`
	c := newChat(t, jsonHandler(t, 200, body, nil))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("who is Dan?")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call_abc" || resp.ToolCalls[0].Name != "lookup" {
		t.Fatalf("tool call mirror wrong: %+v", resp.ToolCalls)
	}
	var args map[string]any
	if err := json.Unmarshal(resp.ToolCalls[0].Parameters, &args); err != nil || args["q"] != "Dan" {
		t.Fatalf("raw tool args not preserved: %s (%v)", resp.ToolCalls[0].Parameters, err)
	}
	if resp.Message.Content[0].Type != llms.ContentToolCall || resp.Message.Content[0].ToolCall == nil {
		t.Fatalf("message tool_call part missing: %+v", resp.Message.Content)
	}
}

// On a max-output truncation the Responses API returns status:"incomplete"
// with the real reason in incomplete_details.reason. StopReason must reflect
// that reason (not the envelope status), and tool calls present in the
// truncated output must still be mapped (tool-call mapping is independent of
// Status). Lets Maestro drop its Raw.(*responses.Response) workaround.
func TestChatIncompleteSurfacesTruncationReason(t *testing.T) {
	body := `{"id":"resp_3","object":"response","status":"incomplete","model":"gpt-test",
"incomplete_details":{"reason":"max_output_tokens"},
"output":[{"type":"function_call","id":"fc_9","call_id":"call_trunc","name":"lookup","arguments":"{\"q\":\"x\"}","status":"completed"}],
"usage":{"input_tokens":3,"output_tokens":7,"total_tokens":10}}`
	c := newChat(t, jsonHandler(t, 200, body, nil))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.StopReason != "max_output_tokens" {
		t.Fatalf("StopReason must be the truncation reason, not the envelope status; got %q", resp.StopReason)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call_trunc" || resp.ToolCalls[0].Name != "lookup" {
		t.Fatalf("tool calls must be preserved on a truncated response: %+v", resp.ToolCalls)
	}
}

// status:"incomplete" with no incomplete_details.reason falls back to the
// envelope status (no spurious empty StopReason).
func TestChatIncompleteWithoutReasonFallsBack(t *testing.T) {
	body := `{"id":"resp_4","object":"response","status":"incomplete","model":"gpt-test",
"incomplete_details":{"reason":""},
"output":[{"type":"message","id":"m1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi"}]}],
"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	c := newChat(t, jsonHandler(t, 200, body, nil))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.StopReason != "incomplete" {
		t.Fatalf("no incomplete_details.reason should fall back to status; got %q", resp.StopReason)
	}
}

func TestChatResponsePreservesInterleavedOrder(t *testing.T) {
	// Output interleaves text, a tool call, then more text. Message must
	// keep that exact order; Text mirror is the flattened concatenation.
	body := `{"id":"resp_3","object":"response","status":"completed","model":"gpt-test",
"output":[
 {"type":"message","id":"m1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"first ","annotations":[]}]},
 {"type":"function_call","id":"fc1","call_id":"call_x","name":"do","arguments":"{}","status":"completed"},
 {"type":"message","id":"m2","role":"assistant","status":"completed","content":[{"type":"output_text","text":"second","annotations":[]}]}],
"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`
	c := newChat(t, jsonHandler(t, 200, body, nil))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("go")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got := make([]string, len(resp.Message.Content))
	for i, p := range resp.Message.Content {
		if p.Type == llms.ContentToolCall {
			got[i] = "tool:" + p.ToolCall.Name
		} else {
			got[i] = "text:" + p.Text
		}
	}
	want := []string{"text:first ", "tool:do", "text:second"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("order not preserved: got %v want %v", got, want)
	}
	if resp.Text != "first second" {
		t.Fatalf("text mirror = %q, want flattened %q", resp.Text, "first second")
	}
}

func TestChatToolChoiceRequiredSendsRequired(t *testing.T) {
	var captured map[string]any
	c := newChat(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respTextJSON)
	})
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages:   []llms.Message{llms.UserText("hi")},
		Tools:      []llms.ToolDefinition{{Name: "t", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceRequired},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if captured["tool_choice"] != "required" {
		t.Fatalf("ToolChoiceRequired must send Responses tool_choice \"required\", got %v", captured["tool_choice"])
	}
}

func TestChatToolChoiceRequiredWithoutToolsRejected(t *testing.T) {
	c := newChat(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respTextJSON)
	})
	for _, tc := range []llms.ToolChoice{
		{Type: llms.ToolChoiceRequired},
		{Type: llms.ToolChoiceTool, Name: "x"},
	} {
		_, err := c.Complete(context.Background(), llms.ChatRequest{
			Messages:   []llms.Message{llms.UserText("hi")},
			ToolChoice: tc,
		})
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
			t.Fatalf("%s with no tools must be bad_request, got %v", tc.Type, err)
		}
	}
}

func TestCacheBreakpointIgnoredGracefully(t *testing.T) {
	c := newChat(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respTextJSON)
	})
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		System:   []llms.ContentPart{{Type: llms.ContentText, Text: "s", CacheBreakpoint: true}},
		Messages: []llms.Message{{Role: llms.RoleUser, Content: []llms.ContentPart{{Type: llms.ContentText, Text: "hi", CacheBreakpoint: true}}}},
	})
	if err != nil {
		t.Fatalf("cache hint must be a safe no-op for OpenAI, got %v", err)
	}
}

func TestChatNullRequiredCoercedToArray(t *testing.T) {
	var captured map[string]any
	c := newChat(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respTextJSON)
	})
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("hi")},
		Tools: []llms.ToolDefinition{{
			Name:        "t",
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"required":null}`),
		}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	tools, _ := captured["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %v", captured["tools"])
	}
	fn, _ := tools[0].(map[string]any)
	params, _ := fn["parameters"].(map[string]any)
	req, ok := params["required"].([]any)
	if !ok || len(req) != 0 {
		t.Fatalf("null required not coerced to []: %#v", params["required"])
	}
}

func TestChatRequestTranslationStructured(t *testing.T) {
	var captured map[string]any
	c := newChat(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respTextJSON)
	})
	temp := float32(0)
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		System: []llms.ContentPart{llms.Text("be terse")},
		Messages: []llms.Message{
			llms.UserText("relate me to Dan"),
			{Role: llms.RoleAssistant, Content: []llms.ContentPart{{
				Type:     llms.ContentToolCall,
				ToolCall: &llms.ToolCall{ID: "call_1", Name: "lookup", Parameters: json.RawMessage(`{"q":"Dan"}`)},
			}}},
			llms.ToolResultMessage(llms.ToolResult{ToolCallID: "call_1", Content: `{"ok":true}`}),
		},
		Temperature: &temp,
		Tools: []llms.ToolDefinition{{
			Name: "lookup", Description: "look up",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		}},
		ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceTool, Name: "lookup"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if captured["instructions"] != "be terse" {
		t.Fatalf("system not sent as instructions: %v", captured["instructions"])
	}
	if _, ok := captured["temperature"]; !ok {
		t.Fatal("explicit zero temperature must be sent")
	}
	// Structured input items, NOT a flattened string.
	input, ok := captured["input"].([]any)
	if !ok || len(input) != 3 {
		t.Fatalf("input must be a 3-item structured list, got %T %v", captured["input"], captured["input"])
	}
	types := make([]string, 3)
	for i, it := range input {
		m, _ := it.(map[string]any)
		types[i], _ = m["type"].(string)
	}
	if types[1] != "function_call" || types[2] != "function_call_output" {
		t.Fatalf("tool round-trip not structured: item types = %v", types)
	}
	tc, _ := captured["tool_choice"].(map[string]any)
	if tc["name"] != "lookup" {
		t.Fatalf("forced tool choice not sent: %v", captured["tool_choice"])
	}
}

func TestChatErrorClassification(t *testing.T) {
	c := newChat(t, jsonHandler(t, 429, `{"error":{"message":"slow down"}}`, map[string]string{"Retry-After": "3"}))
	_, err := c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindRateLimited || pe.StatusCode != 429 {
		t.Fatalf("want rate_limited ProviderError, got %v", err)
	}
}

func TestChatBadRequests(t *testing.T) {
	c := newChat(t, jsonHandler(t, 200, respTextJSON, nil))
	cases := map[string]llms.ChatRequest{
		"no messages": {},
		"empty text": {Messages: []llms.Message{{Role: llms.RoleUser,
			Content: []llms.ContentPart{{Type: llms.ContentText}}}}},
		"system non-text": {
			System:   []llms.ContentPart{{Type: llms.ContentToolResult, ToolResult: &llms.ToolResult{}}},
			Messages: []llms.Message{llms.UserText("hi")}},
		"tool choice no name": {
			Messages:   []llms.Message{llms.UserText("hi")},
			ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceTool}},
		"non-object schema": {
			Messages: []llms.Message{llms.UserText("hi")},
			Tools:    []llms.ToolDefinition{{Name: "x", InputSchema: json.RawMessage(`{"type":"array"}`)}}},
	}
	for name, req := range cases {
		_, err := c.Complete(context.Background(), req)
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
			t.Fatalf("%s: want bad_request ProviderError, got %v", name, err)
		}
	}
}

func TestNewChatConfigErrors(t *testing.T) {
	for _, opts := range [][]Option{
		{WithModel("m")},
		{WithAPIKey("k")},
		{WithAPIKey("k"), WithModel("m"), WithMaxRetries(-1)},
	} {
		_, err := NewChat(opts...)
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindConfig {
			t.Fatalf("opts %v: want config ProviderError, got %v", opts, err)
		}
	}
}

// TestUsageReasoningTokensNormalized pins ADR-0016: the OpenAI wire
// output_tokens is TOTAL (visible + reasoning), with the reasoning
// portion exposed as a subset under output_tokens_details. The
// toolkit normalizes Usage.OutputTokens to VISIBLE-only by subtracting
// reasoning_tokens; ReasoningTokens carries the subset; the wire total
// is preserved as BillableOutputTokens for cost math.
func TestUsageReasoningTokensNormalized(t *testing.T) {
	// Simulate an o-series response: 250 total output tokens with 200
	// of them reasoning; visible output is therefore 50.
	body := `{"id":"resp_o1","object":"response","status":"completed","model":"o1-test",
"output":[{"type":"message","id":"msg_o","role":"assistant","status":"completed",
 "content":[{"type":"output_text","text":"short answer","annotations":[]}]}],
"usage":{"input_tokens":40,"output_tokens":250,"total_tokens":290,
 "output_tokens_details":{"reasoning_tokens":200}}}`
	c := newChat(t, jsonHandler(t, 200, body, nil))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	u := resp.Usage
	if u.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50 (visible: wire 250 - reasoning 200)", u.OutputTokens)
	}
	if u.ReasoningTokens != 200 {
		t.Errorf("ReasoningTokens = %d, want 200", u.ReasoningTokens)
	}
	if u.BillableOutputTokens != 250 {
		t.Errorf("BillableOutputTokens = %d, want 250 (wire output_tokens preserved)", u.BillableOutputTokens)
	}
	// Identity: OutputTokens + ReasoningTokens == BillableOutputTokens.
	if u.OutputTokens+u.ReasoningTokens != u.BillableOutputTokens {
		t.Errorf("billable identity broken: %d + %d != %d",
			u.OutputTokens, u.ReasoningTokens, u.BillableOutputTokens)
	}
}

// TestUsageNonReasoningUnchanged is a regression guard: pre-ADR-0016
// callers relied on Usage.OutputTokens being the wire output_tokens.
// For non-reasoning responses (reasoning_tokens=0) the new semantics
// produce the same number — back-compat for everyone except o-series
// consumers.
func TestUsageNonReasoningUnchanged(t *testing.T) {
	c := newChat(t, jsonHandler(t, 200, respTextJSON, nil))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	u := resp.Usage
	if u.OutputTokens != 7 || u.ReasoningTokens != 0 || u.BillableOutputTokens != 7 {
		t.Errorf("non-reasoning usage drifted: %+v", u)
	}
}
