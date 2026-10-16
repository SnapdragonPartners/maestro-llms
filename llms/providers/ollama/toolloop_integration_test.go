//go:build integration

package ollama_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/ollama"
)

func newOllamaToolLoopClient(t *testing.T) llms.ChatClient {
	t.Helper()
	skipIfDown(t)
	c, err := ollama.New(ollama.WithBaseURL(host()), ollama.WithModel(modelName()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// Live toolloop scenarios shared across providers (issue #51); see
// llms/internal/livetest.
func TestIntegrationOllamaToolLoopConvergence(t *testing.T) {
	livetest.ToolLoopConvergence(t, newOllamaToolLoopClient(t))
}

func TestIntegrationOllamaLongToolConversation(t *testing.T) {
	livetest.LongToolConversation(t, newOllamaToolLoopClient(t))
}
