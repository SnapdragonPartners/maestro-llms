//go:build integration

package openaicompat_test

import (
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
)

// TestIntegrationLunaRouteContextOverflow is the ADR-0026 live trigger. It
// runs the default model (not LUNAROUTE_MODELS), whose 1,048,576-token
// window the 1.1M-token prompt (~6.6 MB) exceeds. deepseek-4.1-flash sends
// code context_length_exceeded; the message-only shape (GLM) is pinned by
// the testdata/lunaroute_glm_context_length.json unit fixture. Run with
// -test.parallel=1 (per-key concurrency limit 2).
func TestIntegrationLunaRouteContextOverflow(t *testing.T) {
	key := lunaRouteKeyOrSkip(t)
	for _, model := range strings.Split(lunaRouteDefaultModels, ",") {
		t.Run(model, func(t *testing.T) {
			livetest.ContextOverflow(t, newLunaRoute(t, key, model), 1_100_000)
		})
	}
}

// TestIntegrationLunaRouteCompactOnOverflow is the ADR-0030 live overflow
// scenario on the same default models as the ADR-0026 trigger. Run with
// -test.parallel=1.
func TestIntegrationLunaRouteCompactOnOverflow(t *testing.T) {
	key := lunaRouteKeyOrSkip(t)
	for _, model := range strings.Split(lunaRouteDefaultModels, ",") {
		t.Run(model, func(t *testing.T) {
			livetest.CompactOnOverflow(t, newLunaRoute(t, key, model), 1_100_000)
		})
	}
}
