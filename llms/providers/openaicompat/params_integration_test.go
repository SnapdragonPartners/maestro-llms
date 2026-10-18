//go:build integration

package openaicompat_test

import (
	"context"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
)

func TestIntegrationLunaRouteProviderParams(t *testing.T) {
	key := lunaRouteKeyOrSkip(t)
	t.Run("max_tokens", func(t *testing.T) {
		livetest.ProviderParamsOverride(t, newLunaRoute(t, key, "deepseek-4.1-flash"), map[string]any{"max_tokens": 16})
	})
	// The override replaces the mapped reasoning_effort, and
	// AppliedReasoning stops reporting it.
	t.Run("reasoning_effort", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		resp, err := newLunaRoute(t, key, "deepseek-4.1-flash").Complete(ctx, llms.ChatRequest{
			Messages:       []llms.Message{llms.UserText("What is 17*23? Reply with just the number.")},
			MaxTokens:      2048,
			Reasoning:      &llms.ReasoningConfig{Effort: llms.ReasoningEffortHigh},
			ProviderParams: map[llms.ProviderID]map[string]any{llms.ProviderOpenAICompat: {"reasoning_effort": "none"}},
		})
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		t.Logf("reasoning tokens %d, applied %+v, text %q", resp.Usage.ReasoningTokens, resp.AppliedReasoning, resp.Text)
		if resp.Usage.ReasoningTokens != 0 || resp.AppliedReasoning != nil {
			t.Fatalf("want the none override to win (0 reasoning tokens) and AppliedReasoning nil")
		}
	})
}
