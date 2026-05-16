//go:build integration

package anthropic_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/anthropic"
)

// Live test against the real Anthropic Messages API. Build-tagged so normal
// `go test` / CI never runs it. Skips when ANTHROPIC_API_KEY is unset.
func TestIntegrationAnthropicChat(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}
	model := os.Getenv("ANTHROPIC_MODEL")
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}

	c, err := anthropic.New(anthropic.WithAPIKey(key), anthropic.WithModel(model))
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
