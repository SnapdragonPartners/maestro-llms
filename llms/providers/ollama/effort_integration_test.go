//go:build integration

package ollama_test

import (
	"os"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/ollama"
)

// ADR-0022: effort maps to think levels on a model that takes them
// (OLLAMA_THINK_MODEL, default gpt-oss:20b).
func TestIntegrationOllamaReasoningEffort(t *testing.T) {
	skipIfDown(t)
	m := os.Getenv("OLLAMA_THINK_MODEL")
	if m == "" {
		m = "gpt-oss:20b"
	}
	c, err := ollama.New(ollama.WithBaseURL(host()), ollama.WithModel(m))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A light prompt: the shared one at a higher level takes minutes on a
	// local 20B model, and the point is that each think level is accepted.
	livetest.ReasoningControlPrompt(t, c, "Is 91 prime? Answer yes or no.",
		livetest.Same(llms.ReasoningConfig{Effort: llms.ReasoningEffortLow}),
		livetest.Same(llms.ReasoningConfig{Effort: llms.ReasoningEffortHigh}),
	)
}
