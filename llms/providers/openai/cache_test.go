package openai

import (
	"context"
	"os"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// ADR-0023 / #95: a real Responses body (LunaRoute, deepseek-4.1-flash,
// 2026-10-17) whose second identical prompt hit the provider's prefix cache.
func TestUsageCachedInputTokens(t *testing.T) {
	body, err := os.ReadFile("testdata/lunaroute_responses_cached.json")
	if err != nil {
		t.Fatal(err)
	}
	c := newChat(t, jsonHandler(t, 200, string(body), nil))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("q")}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	u := resp.Usage
	// input_tokens 8043 includes cached_tokens 7936 (ADR-0023).
	if u.InputTokens != 8043 || u.CacheReadTokens != 7936 || u.CacheWriteTokens != 0 {
		t.Fatalf("usage = %+v, want InputTokens 8043 including CacheReadTokens 7936", u)
	}
}
