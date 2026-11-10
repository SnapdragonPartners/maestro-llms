//go:build integration

package google_test

import (
	"os"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/google"
)

func newGemini(t *testing.T, model string) llms.ChatClient {
	t.Helper()
	c, err := google.New(google.WithAPIKey(apiKey(t)), google.WithModel(model))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// ADR-0022: BudgetTokens maps to thinkingBudget (Gemini 2.5).
func TestIntegrationGoogleReasoningBudget(t *testing.T) {
	livetest.ReasoningControl(t, newGemini(t, model()),
		livetest.Same(llms.ReasoningConfig{BudgetTokens: 128}),
		livetest.Same(llms.ReasoningConfig{BudgetTokens: 4096}),
	)
}

// levelModel is a model that takes thinkingLevel (Gemini 3), for tests that
// set an effort: GOOGLE_LEVEL_MODEL, default gemini-3-flash-preview.
func levelModel() string {
	if m := os.Getenv("GOOGLE_LEVEL_MODEL"); m != "" {
		return m
	}
	return defaultLevelModel
}

// ADR-0022: Effort maps to thinkingLevel (Gemini 3).
func TestIntegrationGoogleReasoningEffort(t *testing.T) {
	livetest.ReasoningControl(t, newGemini(t, levelModel()),
		livetest.Same(llms.ReasoningConfig{Effort: llms.ReasoningEffortLow}),
		livetest.Same(llms.ReasoningConfig{Effort: llms.ReasoningEffortHigh}),
	)
}
