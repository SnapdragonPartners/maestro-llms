//go:build integration

package openai_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/openai"
)

func openaiChatModel() string {
	if m := os.Getenv("OPENAI_CHAT_MODEL"); m != "" {
		return m
	}
	return "gpt-4o-mini"
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

// Live test against the real OpenAI Responses API (chat). Skips when
// OPENAI_API_KEY is unset. OPENAI_CHAT_MODEL overrides the default model.
func TestIntegrationOpenAIChat(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	model := os.Getenv("OPENAI_CHAT_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}

	c, err := openai.NewChat(openai.WithAPIKey(key), openai.WithModel(model))
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}

	temp := float32(0)
	req := llms.ChatRequest{
		Purpose:     llms.PurposeChat,
		System:      []llms.ContentPart{llms.Text("Answer with exactly one lowercase word.")},
		Messages:    []llms.Message{llms.UserText("Reply with the single word: pong")},
		MaxTokens:   16,
		Temperature: &temp,
	}
	// Live providers have transient 5xx/429 hiccups; this test exercises our
	// integration, not provider uptime. Retry only on errors our own
	// classifier reports retryable (dogfoods llms.Retryable).
	var resp llms.ChatResponse
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
		t.Logf("attempt %d retryable error: %v", attempt+1, err)
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Message.Role != llms.RoleAssistant || resp.Text == "" {
		t.Fatalf("unexpected response: %+v", resp.Message)
	}
	if resp.Usage.InputTokens <= 0 || resp.Usage.OutputTokens <= 0 {
		t.Fatalf("expected positive token usage, got %+v", resp.Usage)
	}
	t.Logf("OK: text=%q stop=%q in=%d out=%d",
		resp.Text, resp.StopReason, resp.Usage.InputTokens, resp.Usage.OutputTokens)
}

// Live test against the real OpenAI embeddings API. Build-tagged so normal
// `go test` / CI (which is deliberately network-free) never runs it. Skips
// when OPENAI_API_KEY is unset.
func TestIntegrationOpenAIEmbeddings(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	model := os.Getenv("OPENAI_EMBED_MODEL")
	if model == "" {
		model = "text-embedding-3-small"
	}

	c, err := openai.New(openai.WithAPIKey(key), openai.WithModel(model))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.Embed(ctx, llms.EmbeddingRequest{
		Purpose:    llms.PurposeEmbedding,
		Dimensions: 256, // exercise the per-request dimension override
		Inputs: []llms.EmbeddingInput{
			{ID: "a", Text: "the quick brown fox"},
			{ID: "b", Text: "jumps over the lazy dog"},
		},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(resp.Vectors) != 2 {
		t.Fatalf("want 2 vectors, got %d", len(resp.Vectors))
	}
	if resp.Vectors[0].ID != "a" || resp.Vectors[1].ID != "b" {
		t.Fatalf("input order/IDs not preserved: %+v", resp.Vectors)
	}
	for _, v := range resp.Vectors {
		if len(v.Values) != 256 {
			t.Fatalf("dimension override ignored: vector %q has %d dims, want 256", v.ID, len(v.Values))
		}
	}
	if resp.Usage.EmbeddingTokens <= 0 {
		t.Fatalf("expected positive EmbeddingTokens, got %+v", resp.Usage)
	}
	t.Logf("OK: 2 vectors, 256 dims, EmbeddingTokens=%d TotalTokens=%d",
		resp.Usage.EmbeddingTokens, resp.Usage.TotalTokens)
}

// TestIntegrationOpenAIToolUse exercises the full live tool round trip: the
// model is given a tool, must call it, we feed a result back, and it must
// produce a coherent final answer. This is the most provider-divergent path
// (OpenAI Responses uses function_call / function_call_output items) and only
// a live test proves the adapter translation works end to end.
func TestIntegrationOpenAIToolUse(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	c, err := openai.NewChat(openai.WithAPIKey(key), openai.WithModel(openaiChatModel()))
	if err != nil {
		t.Fatalf("NewChat: %v", err)
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
