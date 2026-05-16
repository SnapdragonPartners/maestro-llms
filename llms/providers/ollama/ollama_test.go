package ollama

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

const respTextJSON = `{"model":"m","message":{"role":"assistant","content":"hello world"},` +
	`"done":true,"done_reason":"stop","prompt_eval_count":11,"eval_count":7}`

func newClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(WithModel("m"), WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
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
	if resp.Text != "hello world" || resp.StopReason != "stop" {
		t.Fatalf("text/stop mirror wrong: %q %q", resp.Text, resp.StopReason)
	}
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 7 || resp.Usage.TotalTokens != 18 {
		t.Fatalf("usage mapping wrong: %+v", resp.Usage)
	}
	if resp.Raw == nil || c.Model().Provider != "ollama" || c.Model().Name != "m" {
		t.Fatalf("raw/model wrong")
	}
}

func TestChatToolCallResponse(t *testing.T) {
	body := `{"model":"m","message":{"role":"assistant","content":"",` +
		`"tool_calls":[{"function":{"name":"lookup","arguments":{"q":"Dan"}}}]},` +
		`"done":true,"done_reason":"stop","prompt_eval_count":5,"eval_count":9}`
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
	// Ollama returns no tool-call id; a synthetic stable id is assigned.
	if resp.ToolCalls[0].ID != "call_0" {
		t.Fatalf("expected synthetic id call_0, got %q", resp.ToolCalls[0].ID)
	}
	var args map[string]any
	if err := json.Unmarshal(resp.ToolCalls[0].Parameters, &args); err != nil || args["q"] != "Dan" {
		t.Fatalf("tool args not preserved: %s (%v)", resp.ToolCalls[0].Parameters, err)
	}
	if resp.Message.Content[0].Type != llms.ContentToolCall {
		t.Fatalf("message tool_call part missing: %+v", resp.Message.Content)
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
				ToolCall: &llms.ToolCall{ID: "call_x", Name: "lookup", Parameters: json.RawMessage(`{"q":"Dan"}`)},
			}}},
			llms.ToolResultMessage(llms.ToolResult{ToolCallID: "call_x", Content: `{"ok":true}`}),
		},
		Temperature: &temp,
		Tools: []llms.ToolDefinition{{
			Name: "lookup", Description: "look up",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
		}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	msgs, _ := captured["messages"].([]any)
	if len(msgs) != 4 { // system, user, assistant(tool_call), tool(result)
		t.Fatalf("want 4 messages, got %d: %v", len(msgs), captured["messages"])
	}
	roles := make([]string, len(msgs))
	for i, m := range msgs {
		mm, _ := m.(map[string]any)
		roles[i], _ = mm["role"].(string)
	}
	if roles[0] != "system" || roles[1] != "user" || roles[2] != "assistant" || roles[3] != "tool" {
		t.Fatalf("message roles wrong: %v", roles)
	}
	toolMsg, _ := msgs[3].(map[string]any)
	if toolMsg["tool_call_id"] != "call_x" {
		t.Fatalf("tool result tool_call_id not sent: %v", toolMsg)
	}
	if captured["tools"] == nil {
		t.Fatal("tools not sent")
	}
	opts, _ := captured["options"].(map[string]any)
	if _, ok := opts["temperature"]; !ok {
		t.Fatalf("temperature option not sent: %v", captured["options"])
	}
}

func TestToolChoiceNoneOmitsTools(t *testing.T) {
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
		ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceNone},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if captured["tools"] != nil {
		t.Fatalf("ToolChoiceNone must omit tools, got %v", captured["tools"])
	}
}

func TestChatErrorClassification(t *testing.T) {
	c := newClient(t, jsonHandler(t, 404, `{"error":"model 'm' not found"}`))
	_, err := c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest || pe.StatusCode != 404 {
		t.Fatalf("want bad_request 404 ProviderError, got %v", err)
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
		"non-object schema (array)": {
			Messages: []llms.Message{llms.UserText("hi")},
			Tools:    []llms.ToolDefinition{{Name: "x", InputSchema: json.RawMessage(`{"type":"array"}`)}}},
		"non-object schema (null)": {
			Messages: []llms.Message{llms.UserText("hi")},
			Tools:    []llms.ToolDefinition{{Name: "x", InputSchema: json.RawMessage(`null`)}}},
		"tool_call on user message": {Messages: []llms.Message{{Role: llms.RoleUser,
			Content: []llms.ContentPart{{Type: llms.ContentToolCall, ToolCall: &llms.ToolCall{Name: "x"}}}}}},
		"tool_result on assistant message": {Messages: []llms.Message{{Role: llms.RoleAssistant,
			Content: []llms.ContentPart{{Type: llms.ContentToolResult, ToolResult: &llms.ToolResult{ToolCallID: "i"}}}}}},
		"text on tool message": {Messages: []llms.Message{{Role: llms.RoleTool,
			Content: []llms.ContentPart{{Type: llms.ContentText, Text: "x"}}}}},
		"tool choice not in tools": {
			Messages:   []llms.Message{llms.UserText("hi")},
			Tools:      []llms.ToolDefinition{{Name: "have", InputSchema: json.RawMessage(`{"type":"object"}`)}},
			ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceTool, Name: "missing"}},
	}
	for name, req := range cases {
		_, err := c.Complete(context.Background(), req)
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
			t.Fatalf("%s: want bad_request ProviderError, got %v", name, err)
		}
	}
}

func TestNewConfigErrors(t *testing.T) {
	if _, err := New(); err == nil {
		t.Fatal("missing model should error")
	}
	for _, opts := range [][]Option{
		{WithModel("m"), WithMaxRetries(-1)},
		{WithModel("m"), WithBaseURL("http://[::1]:namedport")},
		{WithModel("m"), WithBaseURL("ftp://localhost:11434")},
		{WithModel("m"), WithBaseURL("not-a-url")},
	} {
		_, err := New(opts...)
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindConfig {
			t.Fatalf("opts %v: want config ProviderError, got %v", opts, err)
		}
	}
}
