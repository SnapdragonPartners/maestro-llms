//go:build integration

package openaicompat_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
)

// ADR-0022 / #86: LunaRoute passes reasoning_effort through, and DeepSeek
// turns reasoning off at none.
func TestIntegrationLunaRouteReasoningEffort(t *testing.T) {
	key := lunaRouteKeyOrSkip(t)
	t.Run("deepseek-4.1-flash", func(t *testing.T) {
		u := livetest.ReasoningControl(t, newLunaRoute(t, key, "deepseek-4.1-flash"),
			livetest.Same(llms.ReasoningConfig{Effort: llms.ReasoningEffortNone}),
			livetest.Same(llms.ReasoningConfig{Effort: llms.ReasoningEffortLow}),
			livetest.Same(llms.ReasoningConfig{Effort: llms.ReasoningEffortHigh}),
		)
		if u[0].ReasoningTokens != 0 {
			t.Fatalf("deepseek at effort none: ReasoningTokens = %d, want 0", u[0].ReasoningTokens)
		}
	})
}
