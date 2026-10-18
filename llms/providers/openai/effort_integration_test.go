//go:build integration

package openai_test

import (
	"os"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/openai"
)

// ADR-0022: effort maps to reasoning.effort. OPENAI_REASONING_MODEL must be
// a reasoning model (default gpt-5-mini).
func TestIntegrationOpenAIReasoningEffort(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	model := os.Getenv("OPENAI_REASONING_MODEL")
	if model == "" {
		model = "gpt-5-mini"
	}
	c, err := openai.NewChat(openai.WithAPIKey(key), openai.WithModel(model))
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	livetest.ReasoningControl(t, c,
		livetest.Same(llms.ReasoningConfig{Effort: llms.ReasoningEffortMinimal}),
		livetest.Same(llms.ReasoningConfig{Effort: llms.ReasoningEffortHigh}),
	)
}
