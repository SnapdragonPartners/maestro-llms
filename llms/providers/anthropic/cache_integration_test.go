//go:build integration

package anthropic_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
)

// ADR-0023: with a cache breakpoint the first call writes the cache and the
// later ones read it; both are counted inside InputTokens.
func TestIntegrationAnthropicCachedInput(t *testing.T) {
	u := livetest.CachedInput(t, newAnthropic(t, anthropicModel()), true)
	if u[0].CacheWriteTokens == 0 && u[0].CacheReadTokens == 0 {
		t.Fatalf("first call neither wrote nor read the cache: %+v", u[0])
	}
	if u[2].CacheReadTokens == 0 {
		t.Fatalf("third call did not read the cache: %+v", u[2])
	}
}
