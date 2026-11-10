//go:build integration

package openai_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
)

// MaxTokens (max_output_tokens) caps billable output, reasoning included
// (#149, ADR-0044).
func TestIntegrationOpenAIOutputCapText(t *testing.T) {
	livetest.OutputCapText(t, newOpenAIToolLoopClient(t), "max_output_tokens")
}

func TestIntegrationOpenAIOutputCapReasoning(t *testing.T) {
	livetest.OutputCapReasoning(t, newOpenAIReasoningClient(t), "max_output_tokens", &llms.ReasoningConfig{Effort: llms.ReasoningEffortHigh})
}
