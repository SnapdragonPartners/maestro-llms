//go:build integration

package ollama_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/ollama"
)

// TestIntegrationOllamaContextOverflowTruncates pins why the ollama adapter
// has no ErrorKindContextLength detector (ADR-0026): an over-long prompt is
// not rejected, Ollama silently truncates it to num_ctx. If this starts
// failing because Ollama returns an error, capture the body and add a
// detector.
func TestIntegrationOllamaContextOverflowTruncates(t *testing.T) {
	skipIfDown(t)
	c, err := ollama.New(ollama.WithBaseURL(host()), ollama.WithModel(modelName()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	const words = 3000
	resp, err := c.Complete(ctx, llms.ChatRequest{
		Messages:  []llms.Message{llms.UserText(strings.Repeat("hello ", words) + "Reply with the word OK.")},
		MaxTokens: 8,
		ProviderParams: map[llms.ProviderID]map[string]any{
			llms.ProviderOllama: {"options": map[string]any{"num_ctx": 256}},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v (Ollama now rejects over-long prompts; add a detector)", err)
	}
	if resp.Usage.InputTokens >= words {
		t.Fatalf("InputTokens = %d, want < %d (truncated to num_ctx)", resp.Usage.InputTokens, words)
	}
	t.Logf("InputTokens = %d for a ~%d-token prompt with num_ctx 256", resp.Usage.InputTokens, words)
}
