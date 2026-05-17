package middleware

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
)

// countingFake records how many times Complete reached the inner client.
func countingFake(calls *int32) *testllm.FakeChatClient {
	return &testllm.FakeChatClient{
		Func: func(_ context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
			atomic.AddInt32(calls, 1)
			return llms.ChatResponse{Text: "ok"}, nil
		},
	}
}

func asstCall(ids ...string) llms.Message {
	parts := make([]llms.ContentPart, len(ids))
	for i, id := range ids {
		parts[i] = llms.ContentPart{Type: llms.ContentToolCall, ToolCall: &llms.ToolCall{ID: id, Name: "f"}}
	}
	return llms.Message{Role: llms.RoleAssistant, Content: parts}
}

func mustReject(t *testing.T, calls *int32, req llms.ChatRequest, wantSubstr string) {
	t.Helper()
	c := ValidationChat()(countingFake(calls))
	before := atomic.LoadInt32(calls)
	_, err := c.Complete(context.Background(), req)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *ValidationError, got %v", err)
	}
	if wantSubstr != "" && !strings.Contains(ve.Reason, wantSubstr) {
		t.Fatalf("reason %q does not mention %q", ve.Reason, wantSubstr)
	}
	if atomic.LoadInt32(calls) != before {
		t.Fatal("invalid request must not reach the inner client")
	}
}

func TestValidationValidRequestPassesThrough(t *testing.T) {
	var calls int32
	c := ValidationChat()(countingFake(&calls))
	req := llms.ChatRequest{
		System:   []llms.ContentPart{llms.Text("be brief")},
		Messages: []llms.Message{llms.UserText("hi")},
	}
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if calls != 1 {
		t.Fatalf("inner client should be called once, got %d", calls)
	}
}

func TestValidationToolRoundTripValid(t *testing.T) {
	var calls int32
	c := ValidationChat()(countingFake(&calls))
	req := llms.ChatRequest{
		Messages: []llms.Message{
			llms.UserText("weather?"),
			asstCall("a", "b"),
			llms.ToolResultMessage(
				llms.ToolResult{ToolCallID: "a", Content: "sunny"},
				llms.ToolResult{ToolCallID: "b", Content: "18C"},
			),
		},
	}
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("valid tool round trip rejected: %v", err)
	}
	if calls != 1 {
		t.Fatalf("inner client should be called once, got %d", calls)
	}
}

func TestValidationRejections(t *testing.T) {
	var calls int32
	tc := func(name, want string, req llms.ChatRequest) {
		t.Run(name, func(t *testing.T) { mustReject(t, &calls, req, want) })
	}

	tc("empty messages", "at least one message", llms.ChatRequest{})
	tc("non-text system", "only text is allowed", llms.ChatRequest{
		System:   []llms.ContentPart{{Type: llms.ContentToolCall, ToolCall: &llms.ToolCall{ID: "x"}}},
		Messages: []llms.Message{llms.UserText("hi")},
	})
	tc("empty content", "empty content", llms.ChatRequest{
		Messages: []llms.Message{{Role: llms.RoleUser}},
	})
	tc("tool_call missing ID", "tool_call missing", llms.ChatRequest{
		Messages: []llms.Message{{Role: llms.RoleAssistant, Content: []llms.ContentPart{
			{Type: llms.ContentToolCall, ToolCall: &llms.ToolCall{ID: ""}},
		}}},
	})
	tc("unknown content type", "unknown content type", llms.ChatRequest{
		Messages: []llms.Message{{Role: llms.RoleUser, Content: []llms.ContentPart{{Type: "image"}}}},
	})
	tc("missing results at end", "lacking results", llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("q"), asstCall("a")},
	})
	tc("orphaned tool_result", "orphaned tool_result", llms.ChatRequest{
		Messages: []llms.Message{
			llms.UserText("q"),
			llms.ToolResultMessage(llms.ToolResult{ToolCallID: "ghost"}),
		},
	})
	tc("duplicate tool_call id", "duplicate tool_call", llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("q"), asstCall("dup", "dup")},
	})
	tc("duplicate tool_result", "duplicate tool_result", llms.ChatRequest{
		Messages: []llms.Message{
			llms.UserText("q"),
			asstCall("a"),
			llms.ToolResultMessage(
				llms.ToolResult{ToolCallID: "a"},
				llms.ToolResult{ToolCallID: "a"},
			),
		},
	})
	tc("user turn before results", "user turn before tool results", llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("q"), asstCall("a"), llms.UserText("interrupt")},
	})
	tc("assistant before prior results", "previous tool calls have no results", llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("q"), asstCall("a"), asstCall("b")},
	})
	tc("tool_result nil payload", "tool_result missing", llms.ChatRequest{
		Messages: []llms.Message{{Role: llms.RoleTool, Content: []llms.ContentPart{
			{Type: llms.ContentToolResult, ToolResult: nil},
		}}},
	})
	tc("tool_result empty ToolCallID", "tool_result missing", llms.ChatRequest{
		Messages: []llms.Message{{Role: llms.RoleTool, Content: []llms.ContentPart{
			{Type: llms.ContentToolResult, ToolResult: &llms.ToolResult{ToolCallID: ""}},
		}}},
	})
	tc("non-result part on tool message", "may only contain tool_result", llms.ChatRequest{
		Messages: []llms.Message{{Role: llms.RoleTool, Content: []llms.ContentPart{llms.Text("hi")}}},
	})
	tc("unknown role", "unknown role", llms.ChatRequest{
		Messages: []llms.Message{{Role: llms.Role("system"), Content: []llms.ContentPart{llms.Text("x")}}},
	})
}

func TestValidationErrorIsNotRetryable(t *testing.T) {
	if llms.Retryable(&ValidationError{Reason: "x"}) {
		t.Fatal("ValidationError must be non-retryable")
	}
}
