package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// toolResultTranscript is a user turn, one tool call and its result r.
func toolResultTranscript(r llms.ToolResult) []llms.Message {
	r.ToolCallID = "c1"
	return []llms.Message{
		llms.UserText("q"),
		{Role: llms.RoleAssistant, Content: []llms.ContentPart{{
			Type: llms.ContentToolCall, ToolCall: &llms.ToolCall{ID: "c1", Name: "get_document", Parameters: []byte(`{}`)},
		}}},
		llms.ToolResultMessage(r),
	}
}

// TestToolResultTextParts: tool messages are text-only, so text parts are
// joined by a blank line (ADR-0041).
func TestToolResultTextParts(t *testing.T) {
	var captured map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respTextJSON)
	})
	_, err := c.Complete(context.Background(), llms.ChatRequest{Messages: toolResultTranscript(llms.ToolResult{
		Parts: []llms.ContentPart{llms.Text("first"), llms.Text("second")},
	})})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	msgs, _ := captured["messages"].([]any)
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "tool" || last["tool_call_id"] != "c1" || last["content"] != "first\n\nsecond" {
		t.Fatalf("tool message = %v", last)
	}
}

func TestToolResultMediaRejected(t *testing.T) {
	for name, r := range map[string]llms.ToolResult{
		"image":             {Parts: []llms.ContentPart{llms.Text("see"), llms.Image("image/png", []byte("x"))}},
		"document":          {Parts: []llms.ContentPart{llms.Document("application/pdf", []byte("%PDF"))}},
		"content and parts": {Content: "x", Parts: []llms.ContentPart{llms.Text("y")}},
	} {
		t.Run(name, func(t *testing.T) {
			c := newClient(t, func(http.ResponseWriter, *http.Request) {
				t.Error("request sent; want a pre-call rejection")
			})
			_, err := c.Complete(context.Background(), llms.ChatRequest{Messages: toolResultTranscript(r)})
			var pe *llms.ProviderError
			if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
				t.Fatalf("err = %v, want bad_request", err)
			}
		})
	}
}
