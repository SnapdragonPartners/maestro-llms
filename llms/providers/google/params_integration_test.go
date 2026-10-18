//go:build integration

package google_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
)

// A nested override (generationConfig.maxOutputTokens) deep-merges with the
// adapter's generationConfig.
func TestIntegrationGoogleProviderParams(t *testing.T) {
	livetest.ProviderParamsOverride(t, newGemini(t, model()), map[string]any{
		"generationConfig": map[string]any{"maxOutputTokens": 16},
	})
}
