//go:build integration

package anthropic_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/anthropic"
)

func newAnthropic(t *testing.T, model string) llms.ChatClient {
	t.Helper()
	c, err := anthropic.New(anthropic.WithAPIKey(anthropicAPIKey(t)), anthropic.WithModel(model))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// ADR-0022: effort maps to output_config.effort on a current model.
func TestIntegrationAnthropicReasoningEffort(t *testing.T) {
	c := newAnthropic(t, reasoningModel())
	livetest.ReasoningControl(t, c,
		livetest.Same(llms.ReasoningConfig{Effort: llms.ReasoningEffortLow}),
		livetest.Same(llms.ReasoningConfig{Effort: llms.ReasoningEffortHigh}),
	)
}

// ADR-0022: BudgetTokens maps to thinking.budget_tokens on a model that
// takes one (ANTHROPIC_BUDGET_MODEL, default Haiku 4.5).
func TestIntegrationAnthropicReasoningBudget(t *testing.T) {
	model := os.Getenv("ANTHROPIC_BUDGET_MODEL")
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	u := livetest.ReasoningControl(t, newAnthropic(t, model),
		livetest.Same(llms.ReasoningConfig{BudgetTokens: 1024}),
	)
	if u[0].ReasoningTokens <= 0 {
		t.Fatalf("budget_tokens should turn thinking on, got ReasoningTokens = %d", u[0].ReasoningTokens)
	}
}

// ADR-0022 option (a): effort none maps to thinking disabled, which models
// from Opus 5.5 on reject. That is the provider's error, not predicted.
func TestIntegrationAnthropicReasoningNoneRejectedByModel(t *testing.T) {
	_, err := newAnthropic(t, reasoningModel()).Complete(context.Background(), llms.ChatRequest{
		Messages:  []llms.Message{llms.UserText("hi")},
		MaxTokens: 256,
		Reasoning: &llms.ReasoningConfig{Effort: llms.ReasoningEffortNone},
	})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
		t.Fatalf("err = %v, want the model's bad_request for thinking disabled", err)
	}
	t.Logf("rejected as expected: %v", err)
}
