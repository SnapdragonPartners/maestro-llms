//go:build integration

package anthropic_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/anthropic"
)

// MaxTokens caps billable output, thinking included (#149, ADR-0044).
func TestIntegrationAnthropicOutputCapText(t *testing.T) {
	livetest.OutputCapText(t, newAnthropicToolLoopClient(t), "max_tokens")
}

func TestIntegrationAnthropicOutputCapReasoning(t *testing.T) {
	c, err := anthropic.New(anthropic.WithAPIKey(anthropicAPIKey(t)), anthropic.WithModel(reasoningModel()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	livetest.OutputCapReasoning(t, c, "max_tokens", &llms.ReasoningConfig{Effort: llms.ReasoningEffortHigh})
}
