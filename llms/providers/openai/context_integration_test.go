//go:build integration

package openai_test

import (
	"os"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/openai"
)

// TestIntegrationOpenAIContextOverflow is the ADR-0026 live trigger. The
// model is fixed (not OPENAI_CHAT_MODEL) because the prompt is sized to its
// 128k window: 140k tokens is rejected unbilled.
func TestIntegrationOpenAIContextOverflow(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	c, err := openai.New(openai.WithAPIKey(key), openai.WithModel("gpt-4o-mini"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	livetest.ContextOverflow(t, c, 140_000)
}

// TestIntegrationOpenAICompactOnOverflow is the ADR-0030 live overflow
// scenario, sized like the ADR-0026 trigger.
func TestIntegrationOpenAICompactOnOverflow(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	c, err := openai.New(openai.WithAPIKey(key), openai.WithModel("gpt-4o-mini"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	livetest.CompactOnOverflow(t, c, 140_000)
}
