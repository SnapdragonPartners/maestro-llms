//go:build integration

package vllm_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
)

// ADR-0023 / #95: LunaRoute's Chat Completions path caches repeats.
func TestIntegrationLunaRouteCachedInput(t *testing.T) {
	c := newLunaRoute(t, lunaRouteKeyOrSkip(t), "deepseek-4.1-flash")
	if u := livetest.CachedInput(t, c, false); !livetest.AnyCacheRead(u) {
		t.Fatalf("no call read the cache: %+v", u)
	}
}
