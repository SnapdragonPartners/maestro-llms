//go:build integration

package ollama_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/ollama"
)

func TestIntegrationOllamaProviderParams(t *testing.T) {
	skipIfDown(t)
	c, err := ollama.New(ollama.WithBaseURL(host()), ollama.WithModel(modelName()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	livetest.ProviderParamsOverride(t, c, map[string]any{"options": map[string]any{"num_predict": 16}})
}
