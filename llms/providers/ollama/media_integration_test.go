//go:build integration

package ollama_test

import (
	"os"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/ollama"
)

// ADR-0040: images in the message images array. Needs a vision model, so it
// runs only with OLLAMA_VISION_MODEL set (for example mistral-small3.2).
func TestIntegrationOllamaImageInput(t *testing.T) {
	model := os.Getenv("OLLAMA_VISION_MODEL")
	if model == "" {
		t.Skip("OLLAMA_VISION_MODEL not set; skipping Ollama image test")
	}
	c, err := ollama.New(ollama.WithBaseURL(host()), ollama.WithModel(model))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	livetest.ImageInput(t, c)
}
