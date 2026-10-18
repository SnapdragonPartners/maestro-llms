package livetest

import (
	"context"
	"reflect"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// effortPrompt needs some reasoning, so a lower setting visibly spends fewer
// reasoning tokens on models that report them.
const effortPrompt = "How many integers n with 1 <= n <= 300 have a digit sum divisible by 7? Answer with just the number."

// ReasoningSetting is one ReasoningConfig to send, with the AppliedReasoning
// the adapter must report for it (nil when nothing maps).
type ReasoningSetting struct {
	Config  *llms.ReasoningConfig
	Applied *llms.ReasoningConfig
}

// ReasoningControl sends the same reasoning-heavy prompt once per setting
// and checks that the provider accepts each request and that the adapter
// reports exactly what it mapped (ADR-0022). It returns each response's
// Usage, in order, so the caller can compare reasoning spend; token counts
// are logged, not asserted, because effort is a soft control on most models.
func ReasoningControl(t *testing.T, c llms.ChatClient, settings ...ReasoningSetting) []llms.Usage {
	t.Helper()
	return ReasoningControlPrompt(t, c, effortPrompt, settings...)
}

// ReasoningControlPrompt is ReasoningControl with a caller-chosen prompt, for
// slow local models where the default prompt at a high setting takes minutes.
func ReasoningControlPrompt(t *testing.T, c llms.ChatClient, prompt string, settings ...ReasoningSetting) []llms.Usage {
	t.Helper()
	usages := make([]llms.Usage, 0, len(settings))
	for _, s := range settings {
		// Each request gets the full timeout: a high setting on a slow
		// local model can take minutes on its own.
		ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
		resp, err := withRetry(c).Complete(ctx, llms.ChatRequest{
			Messages:  []llms.Message{llms.UserText(prompt)},
			MaxTokens: 16000,
			Reasoning: s.Config,
		})
		cancel()
		if err != nil {
			t.Fatalf("reasoning %+v: %v", s.Config, err)
		}
		if !reflect.DeepEqual(resp.AppliedReasoning, s.Applied) {
			t.Fatalf("reasoning %+v: AppliedReasoning = %+v, want %+v", s.Config, resp.AppliedReasoning, s.Applied)
		}
		t.Logf("reasoning %+v: applied %+v, reasoning tokens %d, output tokens %d, answer %q",
			s.Config, resp.AppliedReasoning, resp.Usage.ReasoningTokens, resp.Usage.OutputTokens, truncate(resp.Text, 40))
		usages = append(usages, resp.Usage)
	}
	return usages
}

// Same returns a setting whose config maps in full.
func Same(cfg llms.ReasoningConfig) ReasoningSetting {
	return ReasoningSetting{Config: &cfg, Applied: &cfg}
}
