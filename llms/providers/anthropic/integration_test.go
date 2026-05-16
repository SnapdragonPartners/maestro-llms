//go:build integration

package anthropic_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/anthropic"
)

func anthropicModel() string {
	if m := os.Getenv("ANTHROPIC_MODEL"); m != "" {
		return m
	}
	return "claude-haiku-4-5-20251001"
}

// anthropicAPIKey resolves the key from ANTHROPIC_API_KEY first (CI secret),
// falling back to MAESTRO_ANTHROPIC_API_KEY. The fallback lets a local dev
// keep ANTHROPIC_API_KEY unset so Claude Code's OAuth subscription auth still
// works while these live tests use a separate prefixed key.
func anthropicAPIKey(t *testing.T) string {
	t.Helper()
	for _, n := range []string{"ANTHROPIC_API_KEY", "MAESTRO_ANTHROPIC_API_KEY"} {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	t.Skip("no Anthropic API key set (ANTHROPIC_API_KEY / MAESTRO_ANTHROPIC_API_KEY)")
	return ""
}

// complete retries only on errors our own classifier reports retryable, so
// provider 5xx/429 don't make the live test flaky (dogfoods llms.Retryable).
func complete(t *testing.T, c llms.ChatClient, req llms.ChatRequest) llms.ChatResponse {
	t.Helper()
	var resp llms.ChatResponse
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		// Per-attempt deadline (a shared ctx across retries would let a
		// first slow attempt poison the rest with an expired context).
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			resp, err = c.Complete(ctx, req)
		}()
		if err == nil || !llms.Retryable(err) {
			break
		}
		t.Logf("attempt %d retryable: %v", attempt+1, err)
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return resp
}

// Live test against the real Anthropic Messages API. Build-tagged so normal
// `go test` / CI never runs it. Skips when no Anthropic key is set.
func TestIntegrationAnthropicChat(t *testing.T) {
	key := anthropicAPIKey(t)

	c, err := anthropic.New(anthropic.WithAPIKey(key), anthropic.WithModel(anthropicModel()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	temp := float32(0)
	resp, err := c.Complete(ctx, llms.ChatRequest{
		Purpose:     llms.PurposeChat,
		System:      []llms.ContentPart{llms.Text("Answer with exactly one lowercase word.")},
		Messages:    []llms.Message{llms.UserText(`Reply with the single word: pong`)},
		MaxTokens:   16,
		Temperature: &temp,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Message.Role != llms.RoleAssistant || resp.Text == "" {
		t.Fatalf("unexpected response message: %+v", resp.Message)
	}
	if resp.StopReason == "" {
		t.Fatal("expected a stop reason")
	}
	if resp.Usage.InputTokens <= 0 || resp.Usage.OutputTokens <= 0 {
		t.Fatalf("expected positive token usage, got %+v", resp.Usage)
	}
	t.Logf("OK: text=%q stop=%q in=%d out=%d",
		resp.Text, resp.StopReason, resp.Usage.InputTokens, resp.Usage.OutputTokens)
}

// TestIntegrationAnthropicToolUse exercises the full live tool round trip: the
// model is given a tool, must call it, we feed a result back, and it must
// produce a coherent final answer. This is the most provider-divergent path
// (Anthropic encodes tool results as user-role content blocks) and only a live
// test proves the adapter translation works end to end.
func TestIntegrationAnthropicToolUse(t *testing.T) {
	key := anthropicAPIKey(t)
	c, err := anthropic.New(anthropic.WithAPIKey(key), anthropic.WithModel(anthropicModel()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	weather := llms.ToolDefinition{
		Name:        "get_weather",
		Description: "Get the current weather for a city.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
	}
	first := complete(t, c, llms.ChatRequest{
		Purpose:    llms.PurposeChat,
		System:     []llms.ContentPart{llms.Text("Use the get_weather tool when asked about weather.")},
		Messages:   []llms.Message{llms.UserText("What's the weather in Paris? Use the tool.")},
		Tools:      []llms.ToolDefinition{weather},
		ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceTool, Name: "get_weather"},
		MaxTokens:  512, // bound generation regardless of model verbosity
	})
	if len(first.ToolCalls) == 0 {
		t.Fatalf("model did not call the tool: %+v", first.Message)
	}
	tc := first.ToolCalls[0]
	if tc.Name != "get_weather" {
		t.Fatalf("unexpected tool: %q", tc.Name)
	}
	var args map[string]any
	if err := json.Unmarshal(tc.Parameters, &args); err != nil || args["city"] == nil {
		t.Fatalf("tool args not parseable/missing city: %s (%v)", tc.Parameters, err)
	}

	// Feed the tool result back and require a coherent final answer.
	final := complete(t, c, llms.ChatRequest{
		Purpose: llms.PurposeChat,
		System:  []llms.ContentPart{llms.Text("Use the get_weather tool when asked about weather.")},
		Messages: []llms.Message{
			llms.UserText("What's the weather in Paris? Use the tool."),
			first.Message,
			llms.ToolResultMessage(llms.ToolResult{
				ToolCallID: tc.ID,
				Content:    `{"city":"Paris","temp_c":18,"summary":"clear"}`,
			}),
		},
		Tools:     []llms.ToolDefinition{weather},
		MaxTokens: 512,
	})
	if final.Text == "" {
		t.Fatalf("no final answer after tool result: %+v", final.Message)
	}
	if !strings.Contains(strings.ToLower(final.Text), "paris") {
		t.Logf("note: final answer doesn't mention Paris (model wording): %q", final.Text)
	}
	t.Logf("OK tool-use: call=%s(%s) -> final=%q", tc.Name, tc.Parameters, final.Text)
}
