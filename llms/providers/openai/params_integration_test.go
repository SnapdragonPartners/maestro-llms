//go:build integration

package openai_test

import (
	"os"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/openai"
)

func TestIntegrationOpenAIProviderParams(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	c, err := openai.NewChat(openai.WithAPIKey(key), openai.WithModel("gpt-4o-mini"))
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	livetest.ProviderParamsOverride(t, c, map[string]any{"max_output_tokens": 16})
}
