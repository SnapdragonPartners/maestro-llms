package google

import (
	"context"
	"os"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// ADR-0023 / #95: a real gemini-2.5-flash body (2026-10-17) where implicit
// caching served most of a repeated prompt.
func TestUsageCachedInputTokens(t *testing.T) {
	body, err := os.ReadFile("testdata/gemini25_flash_implicit_cache.json")
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, jsonHandler(t, 200, string(body)))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("q")}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	u := resp.Usage
	// promptTokenCount 9558 includes cachedContentTokenCount 9208.
	if u.InputTokens != 9558 || u.CacheReadTokens != 9208 || u.CacheWriteTokens != 0 {
		t.Fatalf("usage = %+v, want InputTokens 9558 including CacheReadTokens 9208", u)
	}
}
