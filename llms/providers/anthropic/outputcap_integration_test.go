//go:build integration

package anthropic_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
)

// MaxTokens caps billable output, thinking included (#149, ADR-0044).
func TestIntegrationAnthropicOutputCapText(t *testing.T) {
	livetest.OutputCapText(t, newAnthropicToolLoopClient(t), "max_tokens")
}

func TestIntegrationAnthropicOutputCapReasoning(t *testing.T) {
	livetest.OutputCapReasoning(t, newAnthropic(t, reasoningModel()), "max_tokens", &llms.ReasoningConfig{Effort: llms.ReasoningEffortHigh})
}
