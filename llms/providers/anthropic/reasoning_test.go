package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// ADR-0021: thinking and redacted_thinking blocks round-trip as
// ContentReasoning parts.

// opus55ThinkingToolUse is a real claude-opus-5-5 response (2026-10-17,
// thinking adaptive with display "summarized"): thinking, text, and two
// parallel tool_use blocks.
func opus55ThinkingToolUse(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/opus55_thinking_tool_use.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

// redactedThinkingJSON is synthetic: redacted_thinking cannot be triggered
// on demand. Its shape follows the SDK's RedactedThinkingBlock.
const redactedThinkingJSON = `{"id":"msg_r","type":"message","role":"assistant","model":"claude-test",
"content":[{"type":"redacted_thinking","data":"EmwKAhgBEgy3va3pzix/LafPsn4aDFIT2Xlxh0L5L8rLVyIwxtE3rAFBa8cr3qpP"},
{"type":"text","text":"done"}],
"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":9}}`

// fixtureBlocks returns the fixture's content blocks as generic JSON.
func fixtureBlocks(t *testing.T, body string) []map[string]any {
	t.Helper()
	var msg struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return msg.Content
}

func TestThinkingBlockCapturedAsReasoning(t *testing.T) {
	body := opus55ThinkingToolUse(t)
	c := newClient(t, respondJSON(t, 200, body, nil))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("q")}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	parts := resp.Message.Content
	types := make([]llms.ContentPartType, len(parts))
	for i := range parts {
		types[i] = parts[i].Type
	}
	want := []llms.ContentPartType{llms.ContentReasoning, llms.ContentText, llms.ContentToolCall, llms.ContentToolCall}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("part types = %v, want %v (original block order)", types, want)
	}
	r := parts[0].Reasoning
	blocks := fixtureBlocks(t, body)
	if r.Provider != llms.ProviderAnthropic {
		t.Fatalf("Provider = %q", r.Provider)
	}
	if r.Text != blocks[0]["thinking"] {
		t.Fatalf("Text = %q, want the summarized thinking", r.Text)
	}
	if strings.Contains(resp.Text, r.Text) {
		t.Fatalf("Text mirror %q must not include thinking", resp.Text)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %d, want 2", len(resp.ToolCalls))
	}
}

func TestReasoningRoundTripsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{
		{name: "thinking (real Opus 5.5)", body: opus55ThinkingToolUse(t)},
		{name: "redacted_thinking (synthetic)", body: redactedThinkingJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := newClient(t, respondJSON(t, 200, tc.body, nil))
			resp, err := first.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("q")}})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			msgs := []llms.Message{llms.UserText("q"), resp.Message}
			var results []llms.ToolResult
			for _, call := range resp.ToolCalls {
				results = append(results, llms.ToolResult{ToolCallID: call.ID, Content: "12C"})
			}
			if len(results) > 0 {
				msgs = append(msgs, llms.ToolResultMessage(results...))
			} else {
				msgs = append(msgs, llms.UserText("next"))
			}
			// Editing the readable Text must not change what is sent.
			msgs[1].Content[0].Reasoning.Text = "edited by the app"

			sent := captureMessages(t, msgs)
			asst, _ := sent[1].(map[string]any)
			gotBlocks, _ := asst["content"].([]any)
			wantBlocks := fixtureBlocks(t, tc.body)
			if len(gotBlocks) != len(wantBlocks) {
				t.Fatalf("assistant blocks = %d, want %d: %v", len(gotBlocks), len(wantBlocks), gotBlocks)
			}
			// The reasoning block goes back first and byte-equivalent to
			// what the API returned.
			if !reflect.DeepEqual(gotBlocks[0], any(wantBlocks[0])) {
				t.Fatalf("reasoning block sent back = %v, want %v", gotBlocks[0], wantBlocks[0])
			}
		})
	}
}

// captureMessages sends msgs and returns the wire "messages" array.
func captureMessages(t *testing.T, msgs []llms.Message) []any {
	t.Helper()
	var captured map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, textMsgJSON)
	})
	if _, err := c.Complete(context.Background(), llms.ChatRequest{Messages: msgs}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	sent, _ := captured["messages"].([]any)
	return sent
}

func TestForeignReasoningNotSent(t *testing.T) {
	msgs := []llms.Message{
		llms.UserText("q"),
		{Role: llms.RoleAssistant, Content: []llms.ContentPart{
			{Type: llms.ContentReasoning, Reasoning: &llms.Reasoning{Provider: llms.ProviderGoogle, ProviderData: []byte(`{"type":"thinking"}`)}},
			llms.Text("a"),
		}},
		llms.UserText("next"),
	}
	sent := captureMessages(t, msgs)
	asst, _ := sent[1].(map[string]any)
	blocks, _ := asst["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("foreign reasoning must be dropped: %v", blocks)
	}
	if b, _ := blocks[0].(map[string]any); b["type"] != "text" {
		t.Fatalf("remaining block = %v, want text", b)
	}
}

func TestInvalidReasoningRejected(t *testing.T) {
	own := func(data string) llms.ContentPart {
		return llms.ContentPart{Type: llms.ContentReasoning, Reasoning: &llms.Reasoning{Provider: llms.ProviderAnthropic, ProviderData: []byte(data)}}
	}
	for _, tc := range []struct {
		name, want string
		parts      []llms.ContentPart
	}{
		{"malformed ProviderData", "malformed ProviderData", []llms.ContentPart{own("not json"), llms.Text("a")}},
		{"unknown block type", "unknown block type", []llms.ContentPart{own(`{"type":"other"}`), llms.Text("a")}},
		{"thinking without signature", "without signature", []llms.ContentPart{own(`{"type":"thinking","thinking":"x"}`), llms.Text("a")}},
		{"redacted without data", "without data", []llms.ContentPart{own(`{"type":"redacted_thinking"}`), llms.Text("a")}},
		{"reasoning-only assistant", "assistant message has no content", []llms.ContentPart{own(`{"type":"redacted_thinking","data":"d"}`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, func(http.ResponseWriter, *http.Request) {
				t.Fatal("invalid reasoning must be rejected before any HTTP call")
			})
			_, err := c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{
				llms.UserText("q"),
				{Role: llms.RoleAssistant, Content: tc.parts},
				llms.UserText("next"),
			}})
			var pe *llms.ProviderError
			if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest || !strings.Contains(pe.Message, tc.want) {
				t.Fatalf("err = %v, want bad_request containing %q", err, tc.want)
			}
		})
	}
}
