package google

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

var _ llms.ChatClient = (*Client)(nil)

const respTextJSON = `{
"candidates":[{"content":{"role":"model","parts":[{"text":"hello world"}]},"finishReason":"STOP"}],
"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":7,"totalTokenCount":18},
"responseId":"resp_g1"}`

func newClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(WithAPIKey("k"), WithModel("gemini-test"),
		WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func jsonHandler(t *testing.T, status int, body string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func TestChatTextResponse(t *testing.T) {
	c := newClient(t, jsonHandler(t, 200, respTextJSON))
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
	if resp.Text != "hello world" || resp.StopReason != "STOP" {
		t.Fatalf("text/stop mirror wrong: %q %q", resp.Text, resp.StopReason)
	}
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 7 ||
		resp.Usage.TotalTokens != 18 || resp.Usage.ProviderRequestID != "resp_g1" {
		t.Fatalf("usage mapping wrong: %+v", resp.Usage)
	}
	if resp.Raw == nil || c.Model().Provider != "google" || c.Model().Name != "gemini-test" {
		t.Fatalf("raw/model wrong")
	}
}

func TestChatToolCallResponse(t *testing.T) {
	body := `{"candidates":[{"content":{"role":"model","parts":[
 {"functionCall":{"name":"lookup","args":{"q":"Dan"}}}]},"finishReason":"STOP"}],
"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":9,"totalTokenCount":14}}`
	c := newClient(t, jsonHandler(t, 200, body))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("who is Dan?")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "lookup" {
		t.Fatalf("tool call mirror wrong: %+v", resp.ToolCalls)
	}
	// Gemini has no call id; ID falls back to the function name.
	if resp.ToolCalls[0].ID != "lookup" {
		t.Fatalf("expected ID to fall back to name, got %q", resp.ToolCalls[0].ID)
	}
	var args map[string]any
	if err := json.Unmarshal(resp.ToolCalls[0].Parameters, &args); err != nil || args["q"] != "Dan" {
		t.Fatalf("tool args not preserved: %s (%v)", resp.ToolCalls[0].Parameters, err)
	}
	if resp.Message.Content[0].Type != llms.ContentToolCall {
		t.Fatalf("message tool_call part missing: %+v", resp.Message.Content)
	}
}

func TestToolChoiceRequiredSendsAnyMode(t *testing.T) {
	var captured map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
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
	tcfg, _ := captured["toolConfig"].(map[string]any)
	fcc, _ := tcfg["functionCallingConfig"].(map[string]any)
	if fcc["mode"] != "ANY" {
		t.Fatalf("ToolChoiceRequired must send functionCallingConfig.mode ANY, got %v", captured["toolConfig"])
	}
	if fcc["allowedFunctionNames"] != nil {
		t.Fatalf("Required must not restrict tool names, got %v", fcc["allowedFunctionNames"])
	}
}

func TestToolChoiceRequiredWithoutToolsRejected(t *testing.T) {
	c := newClient(t, jsonHandler(t, 200, respTextJSON))
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
	c := newClient(t, jsonHandler(t, 200, respTextJSON))
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		System:   []llms.ContentPart{{Type: llms.ContentText, Text: "s", CacheBreakpoint: true}},
		Messages: []llms.Message{{Role: llms.RoleUser, Content: []llms.ContentPart{{Type: llms.ContentText, Text: "hi", CacheBreakpoint: true}}}},
	})
	if err != nil {
		t.Fatalf("cache hint must be a safe no-op for Gemini, got %v", err)
	}
}

func TestChatRequestTranslation(t *testing.T) {
	var captured map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
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
				ToolCall: &llms.ToolCall{ID: "lookup", Name: "lookup", Parameters: json.RawMessage(`{"q":"Dan"}`)},
			}}},
			llms.ToolResultMessage(llms.ToolResult{ToolCallID: "lookup", Content: `{"ok":true}`}),
		},
		Temperature: &temp,
		Tools: []llms.ToolDefinition{{
			Name: "lookup", Description: "look up",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
		}},
		ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceTool, Name: "lookup"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if captured["systemInstruction"] == nil {
		t.Fatal("system not sent as systemInstruction")
	}
	contents, _ := captured["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("want 3 contents (user, model, user), got %d: %v", len(contents), captured["contents"])
	}
	roles := make([]string, 3)
	for i, ct := range contents {
		m, _ := ct.(map[string]any)
		roles[i], _ = m["role"].(string)
	}
	if roles[0] != "user" || roles[1] != "model" || roles[2] != "user" {
		t.Fatalf("content roles wrong: %v", roles)
	}
	tcfg, _ := captured["toolConfig"].(map[string]any)
	fcc, _ := tcfg["functionCallingConfig"].(map[string]any)
	if fcc["mode"] != "ANY" {
		t.Fatalf("forced tool choice not sent: %v", captured["toolConfig"])
	}
}

func TestChatErrorClassification(t *testing.T) {
	body := `{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`
	c := newClient(t, jsonHandler(t, 429, body))
	_, err := c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindRateLimited || pe.StatusCode != 429 {
		t.Fatalf("want rate_limited ProviderError, got %v", err)
	}
}

func TestChatBadRequests(t *testing.T) {
	c := newClient(t, jsonHandler(t, 200, respTextJSON))
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

func TestChatUnsupportedSchemaTypeRejected(t *testing.T) {
	c := newClient(t, jsonHandler(t, 200, respTextJSON))
	for name, schema := range map[string]string{
		"unknown leaf type": `{"type":"object","properties":{"x":{"type":"widget"}}}`,
		"null type":         `{"type":"object","properties":{"x":{"type":"null"}}}`,
		"type array":        `{"type":"object","properties":{"x":{"type":["string","null"]}}}`,
	} {
		_, err := c.Complete(context.Background(), llms.ChatRequest{
			Messages: []llms.Message{llms.UserText("hi")},
			Tools:    []llms.ToolDefinition{{Name: "t", InputSchema: json.RawMessage(schema)}},
		})
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
			t.Fatalf("%s: want bad_request (no silent string coercion), got %v", name, err)
		}
	}
}

func TestChatToolResultNameResolvedFromPriorCall(t *testing.T) {
	var captured map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respTextJSON)
	})
	// Opaque, non-name ID (as OpenAI/Anthropic would produce).
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{
			llms.UserText("weather?"),
			{Role: llms.RoleAssistant, Content: []llms.ContentPart{{
				Type:     llms.ContentToolCall,
				ToolCall: &llms.ToolCall{ID: "call_abc123", Name: "get_weather", Parameters: json.RawMessage(`{}`)},
			}}},
			llms.ToolResultMessage(llms.ToolResult{ToolCallID: "call_abc123", Content: `{}`}),
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	contents, _ := captured["contents"].([]any)
	last, _ := contents[len(contents)-1].(map[string]any)
	parts, _ := last["parts"].([]any)
	fr, _ := parts[0].(map[string]any)
	resp, _ := fr["functionResponse"].(map[string]any)
	if resp["name"] != "get_weather" {
		t.Fatalf("functionResponse.name not resolved from prior call: %v (want get_weather)", resp["name"])
	}
}

func TestNewConfigErrors(t *testing.T) {
	for _, opts := range [][]Option{
		{WithModel("m")},
		{WithAPIKey("k")},
		{WithAPIKey("k"), WithModel("m"), WithMaxRetries(-1)},
	} {
		_, err := New(opts...)
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindConfig {
			t.Fatalf("opts %v: want config ProviderError, got %v", opts, err)
		}
	}
}
