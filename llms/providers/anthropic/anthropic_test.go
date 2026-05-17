package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

var _ llms.ChatClient = (*Client)(nil)

const textMsgJSON = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test",
"content":[{"type":"text","text":"hello world"}],
"stop_reason":"end_turn","stop_sequence":null,
"usage":{"input_tokens":11,"output_tokens":7,"cache_creation_input_tokens":2,"cache_read_input_tokens":3}}`

// newClient returns a Client whose requests hit handler instead of the network.
// capturedBody, if non-nil, receives the decoded request body.
func newClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(
		WithAPIKey("test-key"),
		WithModel("claude-test"),
		WithBaseURL(srv.URL),
		WithHTTPClient(srv.Client()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func respondJSON(t *testing.T, status int, body string, headers map[string]string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func TestCompleteTextResponse(t *testing.T) {
	c := newClient(t, respondJSON(t, 200, textMsgJSON, nil))
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
	if resp.Text != "hello world" { // mirror matches Message
		t.Fatalf("Text mirror = %q", resp.Text)
	}
	if resp.StopReason != "end_turn" {
		t.Fatalf("stop reason = %q", resp.StopReason)
	}
	u := resp.Usage
	if u.InputTokens != 11 || u.OutputTokens != 7 || u.TotalTokens != 18 ||
		u.CacheWriteTokens != 2 || u.CacheReadTokens != 3 || u.ProviderRequestID != "msg_1" {
		t.Fatalf("usage mapping wrong: %+v", u)
	}
	if resp.Raw == nil {
		t.Fatal("Raw should carry the provider message")
	}
	if c.Model().Provider != "anthropic" || c.Model().Name != "claude-test" {
		t.Fatalf("model ref = %+v", c.Model())
	}
}

func TestCompleteToolUseResponse(t *testing.T) {
	body := `{"id":"msg_2","type":"message","role":"assistant","model":"claude-test",
"content":[{"type":"tool_use","id":"toolu_9","name":"lookup_person","input":{"query":"Dan"}}],
"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":9}}`
	c := newClient(t, respondJSON(t, 200, body, nil))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("who is Dan?")},
		Tools: []llms.ToolDefinition{{
			Name:        "lookup_person",
			Description: "look someone up",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
		}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "toolu_9" || resp.ToolCalls[0].Name != "lookup_person" {
		t.Fatalf("tool call mirror wrong: %+v", resp.ToolCalls)
	}
	// Raw provider JSON preserved verbatim.
	var got map[string]any
	if err := json.Unmarshal(resp.ToolCalls[0].Parameters, &got); err != nil || got["query"] != "Dan" {
		t.Fatalf("tool params not preserved: %s (%v)", resp.ToolCalls[0].Parameters, err)
	}
	if resp.Message.Content[0].Type != llms.ContentToolCall || resp.Message.Content[0].ToolCall == nil {
		t.Fatalf("message tool_call part missing: %+v", resp.Message.Content)
	}
}

func TestRequestTranslationToolRoundTrip(t *testing.T) {
	var captured map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, textMsgJSON)
	})
	temp := float32(0)
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		System: []llms.ContentPart{llms.Text("be terse")},
		Messages: []llms.Message{
			llms.UserText("relate me to Dan"),
			{Role: llms.RoleAssistant, Content: []llms.ContentPart{{
				Type:     llms.ContentToolCall,
				ToolCall: &llms.ToolCall{ID: "toolu_1", Name: "lookup", Parameters: json.RawMessage(`{"q":"Dan"}`)},
			}}},
			llms.ToolResultMessage(llms.ToolResult{ToolCallID: "toolu_1", Content: `{"ok":true}`}),
		},
		Temperature: &temp, // explicit 0 must be sent
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if captured["system"] == nil {
		t.Fatal("system not sent as top-level param")
	}
	if _, ok := captured["temperature"]; !ok {
		t.Fatal("explicit temperature 0 must be sent (pointer semantics)")
	}
	msgs, _ := captured["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("want 3 wire messages (user, assistant, user/tool_result), got %d: %v", len(msgs), captured["messages"])
	}
	last, _ := msgs[2].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("tool result must map to a user message, got role %v", last["role"])
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		status     int
		retryAfter string
		wantKind   llms.ErrorKind
		wantRetry  time.Duration
	}{
		{401, "", llms.ErrorKindAuth, 0},
		{403, "", llms.ErrorKindAuth, 0},
		{429, "2", llms.ErrorKindRateLimited, 2 * time.Second},
		{400, "", llms.ErrorKindBadRequest, 0},
		{500, "", llms.ErrorKindUnavailable, 0},
		{503, "", llms.ErrorKindUnavailable, 0},
	}
	for _, tc := range cases {
		hdr := map[string]string{}
		if tc.retryAfter != "" {
			hdr["Retry-After"] = tc.retryAfter
		}
		body := `{"type":"error","error":{"type":"x","message":"boom"}}`
		c := newClient(t, respondJSON(t, tc.status, body, hdr))
		_, err := c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}})
		var pe *llms.ProviderError
		if !errors.As(err, &pe) {
			t.Fatalf("status %d: want *llms.ProviderError, got %v", tc.status, err)
		}
		if pe.Kind != tc.wantKind || pe.StatusCode != tc.status {
			t.Fatalf("status %d: kind=%q code=%d, want kind=%q", tc.status, pe.Kind, pe.StatusCode, tc.wantKind)
		}
		if pe.RetryAfter != tc.wantRetry {
			t.Fatalf("status %d: RetryAfter=%v want %v", tc.status, pe.RetryAfter, tc.wantRetry)
		}
		if !pe.Retryable() && (tc.wantKind == llms.ErrorKindRateLimited || tc.wantKind == llms.ErrorKindUnavailable) {
			t.Fatalf("status %d: expected retryable", tc.status)
		}
	}
}

func TestContextCanceledClassified(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = io.WriteString(w, textMsgJSON)
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := c.Complete(ctx, llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}})
	// Caller cancellation is not a provider failure: returned as-is (not a
	// *ProviderError), non-retryable, still errors.Is(context.Canceled)
	// (see apierr / divergences X5).
	var pe *llms.ProviderError
	if errors.As(err, &pe) {
		t.Fatalf("context.Canceled must not be a ProviderError, got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want errors.Is(context.Canceled), got %v", err)
	}
	if llms.Retryable(err) {
		t.Fatalf("context.Canceled must be non-retryable, got %v", err)
	}
}

func TestNewConfigErrors(t *testing.T) {
	if _, err := New(WithModel("m")); err == nil {
		t.Fatal("missing API key should error")
	}
	if _, err := New(WithAPIKey("k")); err == nil {
		t.Fatal("missing model should error")
	}
	_, err := New(WithModel("m"))
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindConfig {
		t.Fatalf("want config ProviderError, got %v", err)
	}
}

func TestToolChoiceToolRequiresName(t *testing.T) {
	c := newClient(t, respondJSON(t, 200, textMsgJSON, nil))
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages:   []llms.Message{llms.UserText("hi")},
		ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceTool}, // Name empty
	})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
		t.Fatalf("want bad_request for tool choice without name, got %v", err)
	}
}

func TestToolChoiceRequiredSendsAny(t *testing.T) {
	var captured map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, textMsgJSON)
	})
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages:   []llms.Message{llms.UserText("hi")},
		Tools:      []llms.ToolDefinition{{Name: "t", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceRequired},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	tc, _ := captured["tool_choice"].(map[string]any)
	if tc["type"] != "any" {
		t.Fatalf("ToolChoiceRequired must send Anthropic tool_choice {type:any}, got %v", captured["tool_choice"])
	}
}

func TestEmptyContentPartRejected(t *testing.T) {
	c := newClient(t, respondJSON(t, 200, textMsgJSON, nil))
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{{Role: llms.RoleUser, Content: []llms.ContentPart{{Type: llms.ContentText, Text: ""}}}},
	})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
		t.Fatalf("want bad_request for empty text part (not silent drop), got %v", err)
	}
}

func TestNonObjectToolSchemaRejected(t *testing.T) {
	c := newClient(t, respondJSON(t, 200, textMsgJSON, nil))
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("hi")},
		Tools: []llms.ToolDefinition{{
			Name:        "bad",
			InputSchema: json.RawMessage(`{"type":"array","items":{"type":"string"}}`),
		}},
	})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
		t.Fatalf("want bad_request for non-object tool schema, got %v", err)
	}
}

func TestNegativeMaxRetriesRejected(t *testing.T) {
	_, err := New(WithAPIKey("k"), WithModel("m"), WithMaxRetries(-1))
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindConfig {
		t.Fatalf("want config error for negative max retries, got %v", err)
	}
}

func TestSystemMustBeTextOnly(t *testing.T) {
	c := newClient(t, respondJSON(t, 200, textMsgJSON, nil))
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		System: []llms.ContentPart{{
			Type:       llms.ContentToolResult,
			ToolResult: &llms.ToolResult{ToolCallID: "x", Content: "y"},
		}},
		Messages: []llms.Message{llms.UserText("hi")},
	})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
		t.Fatalf("want bad_request ProviderError for non-text system, got %v", err)
	}
}
