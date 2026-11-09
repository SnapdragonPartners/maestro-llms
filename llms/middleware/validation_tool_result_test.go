package middleware

import (
	"context"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// toolResultReq is a user turn, one tool call and its result r (ADR-0041).
func toolResultReq(r llms.ToolResult) llms.ChatRequest {
	r.ToolCallID = "c1"
	return llms.ChatRequest{Messages: []llms.Message{
		llms.UserText("q"),
		{Role: llms.RoleAssistant, Content: []llms.ContentPart{{Type: llms.ContentToolCall, ToolCall: &llms.ToolCall{ID: "c1", Name: "f"}}}},
		llms.ToolResultMessage(r),
	}}
}

func TestValidationToolResultPartsValid(t *testing.T) {
	var calls int32
	c := ValidationChat()(countingFake(&calls))
	req := toolResultReq(llms.ToolResult{Parts: []llms.ContentPart{
		llms.Text("here"),
		llms.Image("image/png", []byte("x")),
		llms.Document("application/pdf", []byte("x")),
	}, IsError: true})
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("tool result parts rejected: %v", err)
	}
	if calls != 1 {
		t.Fatalf("inner client calls = %d, want 1", calls)
	}
}

func TestValidationToolResultPartsRejections(t *testing.T) {
	var calls int32
	cases := []struct {
		name string
		want string
		r    llms.ToolResult
	}{
		{"content and parts", "both Content and Parts", llms.ToolResult{Content: "x", Parts: []llms.ContentPart{llms.Text("y")}}},
		{"empty text", "empty text part", llms.ToolResult{Parts: []llms.ContentPart{llms.Text("")}}},
		{"empty media", "empty Media.Data", llms.ToolResult{Parts: []llms.ContentPart{llms.Image("image/png", nil)}}},
		{"image with document type", "non-image media type", llms.ToolResult{Parts: []llms.ContentPart{llms.Image("application/pdf", []byte("x"))}}},
		{"nested tool result", "not allowed", llms.ToolResult{Parts: []llms.ContentPart{{
			Type: llms.ContentToolResult, ToolResult: &llms.ToolResult{ToolCallID: "c1", Content: "x"},
		}}}},
		{"nested tool call", "not allowed", llms.ToolResult{Parts: []llms.ContentPart{{
			Type: llms.ContentToolCall, ToolCall: &llms.ToolCall{ID: "z", Name: "f"},
		}}}},
		{"reasoning", "not allowed", llms.ToolResult{Parts: []llms.ContentPart{{
			Type: llms.ContentReasoning, Reasoning: &llms.Reasoning{Provider: llms.ProviderAnthropic},
		}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { mustReject(t, &calls, toolResultReq(c.r), c.want) })
	}
}
