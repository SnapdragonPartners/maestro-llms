package llms

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestUserTextAndAssistantText(t *testing.T) {
	u := UserText("hello")
	if u.Role != RoleUser || len(u.Content) != 1 || u.Content[0].Type != ContentText || u.Content[0].Text != "hello" {
		t.Fatalf("unexpected user message: %+v", u)
	}
	a := AssistantText("hi")
	if a.Role != RoleAssistant || a.Content[0].Text != "hi" {
		t.Fatalf("unexpected assistant message: %+v", a)
	}
}

// TestToolExchangeRoundTrip exercises the spec's worked tool exchange: an
// assistant tool_call turn followed by a RoleTool result, all as content parts.
func TestToolExchangeRoundTrip(t *testing.T) {
	assistant := Message{
		Role: RoleAssistant,
		Content: []ContentPart{{
			Type: ContentToolCall,
			ToolCall: &ToolCall{
				ID:         "toolu_123",
				Name:       "lookup_person",
				Parameters: json.RawMessage(`{"query":"Daniel Ratner"}`),
			},
		}},
	}
	toolMsg := ToolResultMessage(ToolResult{
		ToolCallID: "toolu_123",
		Content:    `{"matches":[{"id":"1585"}]}`,
	})

	if toolMsg.Role != RoleTool {
		t.Fatalf("tool result message role = %q, want %q", toolMsg.Role, RoleTool)
	}
	if len(toolMsg.Content) != 1 || toolMsg.Content[0].Type != ContentToolResult {
		t.Fatalf("unexpected tool message content: %+v", toolMsg.Content)
	}
	if got := toolMsg.Content[0].ToolResult.ToolCallID; got != "toolu_123" {
		t.Fatalf("tool result not linked to call: got %q", got)
	}

	conv := append([]Message{UserText("How am I related to Daniel Ratner?")}, assistant, toolMsg)
	if len(conv) != 3 {
		t.Fatalf("conversation length = %d, want 3", len(conv))
	}
}

// TestToolResultMessageMultiple verifies one RoleTool message can carry several
// tool_result parts (the multi-tool-call turn rule), each independently linked.
func TestToolResultMessageMultiple(t *testing.T) {
	msg := ToolResultMessage(
		ToolResult{ToolCallID: "call_a", Content: `{"ok":true}`},
		ToolResult{ToolCallID: "call_b", Content: `{"ok":true}`},
	)
	if len(msg.Content) != 2 {
		t.Fatalf("want 2 result parts, got %d", len(msg.Content))
	}
	ids := []string{msg.Content[0].ToolResult.ToolCallID, msg.Content[1].ToolResult.ToolCallID}
	if ids[0] != "call_a" || ids[1] != "call_b" {
		t.Fatalf("result parts not distinctly linked: %v", ids)
	}
}

func TestProviderErrorRetryableAndUnwrap(t *testing.T) {
	cause := errors.New("boom")
	cases := map[ErrorKind]bool{
		ErrorKindRateLimited:   true,
		ErrorKindTimeout:       true,
		ErrorKindUnavailable:   true,
		ErrorKindUnknown:       true,
		ErrorKindAuth:          false,
		ErrorKindConfig:        false,
		ErrorKindBadRequest:    false,
		ErrorKindContentPolicy: false,
	}
	for kind, wantRetryable := range cases {
		err := &ProviderError{Provider: "anthropic", Model: "claude", Kind: kind, Cause: cause}
		if err.Retryable() != wantRetryable {
			t.Errorf("kind %q Retryable() = %v, want %v", kind, err.Retryable(), wantRetryable)
		}
		if !errors.Is(err, cause) {
			t.Errorf("kind %q: errors.Is did not unwrap to cause", kind)
		}
		if Retryable(err) != wantRetryable {
			t.Errorf("kind %q: package Retryable disagrees with method", kind)
		}
	}
}

func TestLimitErrorIsAlwaysRetryable(t *testing.T) {
	le := &LimitError{Provider: "openai", Model: "gpt", Reason: "tokens", RetryAfter: 2 * time.Second}
	if !le.Retryable() || !Retryable(le) {
		t.Fatal("LimitError should always be retryable")
	}
	var pe *ProviderError
	if errors.As(le, &pe) {
		t.Fatal("LimitError must not satisfy errors.As(*ProviderError)")
	}
}

func TestRetryableUnknownErrorIsFalse(t *testing.T) {
	if Retryable(errors.New("some other error")) {
		t.Fatal("unclassified errors must not be reported retryable")
	}
}

func TestRetryAfter(t *testing.T) {
	if d := RetryAfter(errors.New("plain")); d != 0 {
		t.Fatalf("unknown error must have no hint, got %v", d)
	}
	pe := &ProviderError{Provider: "p", Model: "m", Kind: ErrorKindRateLimited, RetryAfter: 3 * time.Second}
	if d := RetryAfter(pe); d != 3*time.Second {
		t.Fatalf("ProviderError hint not surfaced, got %v", d)
	}
	le := &LimitError{Provider: "p", Model: "m", RetryAfter: 750 * time.Millisecond}
	if d := RetryAfter(le); d != 750*time.Millisecond {
		t.Fatalf("LimitError hint not surfaced, got %v", d)
	}
	// Must resolve through a wrapping error, like Retryable does.
	if d := RetryAfter(fmt.Errorf("wrapped: %w", pe)); d != 3*time.Second {
		t.Fatalf("RetryAfter must resolve through errors.As, got %v", d)
	}
	if d := RetryAfter(&ProviderError{Kind: ErrorKindUnavailable}); d != 0 {
		t.Fatalf("no hint set must be 0, got %v", d)
	}
}
