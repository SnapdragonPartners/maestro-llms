//go:build integration

package google_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
)

// ADR-0023 / #95: Gemini implicit caching is best effort, so only the
// invariant is asserted.
func TestIntegrationGoogleCachedInput(t *testing.T) {
	livetest.CachedInput(t, newGemini(t, model()), false)
}
