//go:build integration

package google_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
)

// MaxTokens (maxOutputTokens) caps billable output, thinking included
// (#149, ADR-0044).
func TestIntegrationGoogleOutputCapText(t *testing.T) {
	livetest.OutputCapText(t, newGoogleToolLoopClient(t), "MAX_TOKENS")
}

func TestIntegrationGoogleOutputCapReasoning(t *testing.T) {
	livetest.OutputCapReasoning(t, newGoogleToolLoopClient(t), "MAX_TOKENS", &llms.ReasoningConfig{Effort: llms.ReasoningEffortHigh})
}
