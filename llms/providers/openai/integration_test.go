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
