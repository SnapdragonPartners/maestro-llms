//go:build integration

package openaicompat_test

import (
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
)

// TestIntegrationLunaRouteContextOverflow is the ADR-0026 live trigger. It
// runs the default models (not LUNAROUTE_MODELS), whose 1,048,576-token
// windows the 1.1M-token prompt (~6.6 MB) exceeds; they cover both signal
// shapes: deepseek-4.1-flash sends code context_length_exceeded, glm-5.3
// only a message. Run with -test.parallel=1 (per-key concurrency limit 2).
func TestIntegrationLunaRouteContextOverflow(t *testing.T) {
	key := lunaRouteKeyOrSkip(t)
	for _, model := range strings.Split(lunaRouteDefaultModels, ",") {
		t.Run(model, func(t *testing.T) {
			livetest.ContextOverflow(t, newLunaRoute(t, key, model), 1_100_000)
		})
	}
}
