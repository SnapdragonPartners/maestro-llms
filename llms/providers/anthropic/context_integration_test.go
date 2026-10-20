//go:build integration

package anthropic_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/anthropic"
)

// TestIntegrationAnthropicContextOverflow is the ADR-0026 live trigger. The
// model is fixed (not ANTHROPIC_MODEL) because the prompt is sized to its
// 200k window: 260k tokens is rejected unbilled.
func TestIntegrationAnthropicContextOverflow(t *testing.T) {
	c, err := anthropic.New(anthropic.WithAPIKey(anthropicAPIKey(t)), anthropic.WithModel("claude-haiku-4-5-20251001"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	livetest.ContextOverflow(t, c, 260_000)
}

// TestIntegrationAnthropicCompactOnOverflow is the ADR-0030 live overflow
// scenario, sized like the ADR-0026 trigger.
func TestIntegrationAnthropicCompactOnOverflow(t *testing.T) {
	c, err := anthropic.New(anthropic.WithAPIKey(anthropicAPIKey(t)), anthropic.WithModel("claude-haiku-4-5-20251001"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	livetest.CompactOnOverflow(t, c, 260_000)
}
