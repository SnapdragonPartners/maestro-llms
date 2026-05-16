//go:build integration

package openai_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/openai"
)

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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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
		resp, err = c.Complete(ctx, req)
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
