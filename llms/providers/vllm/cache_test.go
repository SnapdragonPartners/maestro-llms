package vllm

import (
	"context"
	"os"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// ADR-0023 / #95: a real Chat Completions body (LunaRoute,
// deepseek-4.1-flash, 2026-10-17) whose second identical prompt hit the
// prefix cache.
func TestUsageCachedInputTokens(t *testing.T) {
	body, err := os.ReadFile("testdata/lunaroute_chat_cached.json")
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, respondJSON(t, 200, string(body)))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("q")}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	u := resp.Usage
	// prompt_tokens 8043 includes cached_tokens 7936 (ADR-0023).
	if u.InputTokens != 8043 || u.CacheReadTokens != 7936 || u.CacheWriteTokens != 0 {
		t.Fatalf("usage = %+v, want InputTokens 8043 including CacheReadTokens 7936", u)
	}
}

// cache_write_tokens is OpenAI's standard field and is mapped when present.
func TestUsageCacheWriteTokens(t *testing.T) {
	const body = `{"id":"c","object":"chat.completion","created":1,"model":"m",
"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],
"usage":{"prompt_tokens":100,"completion_tokens":2,"total_tokens":102,
"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":50}}}`
	c := newClient(t, respondJSON(t, 200, body))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("q")}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if u := resp.Usage; u.InputTokens != 100 || u.CacheReadTokens != 40 || u.CacheWriteTokens != 50 {
		t.Fatalf("usage = %+v, want 100 input with 40 read and 50 written", u)
	}
}

// #95: LunaRoute reports cache writes as the non-standard
// created_cache_tokens; a real first-call body (nothing cached yet, 7936
// tokens written) maps it to CacheWriteTokens.
func TestUsageCreatedCacheTokens(t *testing.T) {
	body, err := os.ReadFile("testdata/lunaroute_chat_cache_write.json")
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, respondJSON(t, 200, string(body)))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("q")}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if u := resp.Usage; u.InputTokens != 8043 || u.CacheReadTokens != 0 || u.CacheWriteTokens != 7936 {
		t.Fatalf("usage = %+v, want 8043 input with 7936 written to cache", u)
	}
}

// The standard field wins when a server sends both.
func TestUsageCacheWritePrefersStandardField(t *testing.T) {
	const body = `{"id":"c","object":"chat.completion","created":1,"model":"m",
"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],
"usage":{"prompt_tokens":100,"completion_tokens":2,"total_tokens":102,
"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":30,"created_cache_tokens":90}}}`
	c := newClient(t, respondJSON(t, 200, body))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("q")}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if u := resp.Usage; u.CacheWriteTokens != 30 {
		t.Fatalf("CacheWriteTokens = %d, want the standard field's 30", u.CacheWriteTokens)
	}
}
