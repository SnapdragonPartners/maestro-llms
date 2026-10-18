package livetest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// cachePrompt is a long, stable prefix (~8k tokens) so providers that cache
// by prefix can serve repeats from cache.
func cachePrompt() string {
	var b strings.Builder
	for i := range 400 {
		fmt.Fprintf(&b, "Fact %d: the item numbered %d is filed under shelf %d in aisle %d. ", i, i, i%17, i%5)
	}
	return b.String()
}

// CachedInput sends the same long prompt three times and checks the
// ADR-0023 invariant on every response: CacheReadTokens and CacheWriteTokens
// are subsets of InputTokens. With breakpoint set the prompt is marked
// cacheable (ContentPart.CacheBreakpoint; Anthropic needs it, others cache
// implicitly). It returns each call's Usage so the caller can assert cache
// hits where a provider caches deterministically.
func CachedInput(t *testing.T, c llms.ChatClient, breakpoint bool) []llms.Usage {
	t.Helper()
	prefix := llms.Text(cachePrompt())
	prefix.CacheBreakpoint = breakpoint
	req := llms.ChatRequest{
		Messages: []llms.Message{{Role: llms.RoleUser, Content: []llms.ContentPart{
			prefix, llms.Text("Which aisle holds item 123? Answer with just the number."),
		}}},
		MaxTokens: 512,
	}
	usages := make([]llms.Usage, 0, 3)
	for i := range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
		resp, err := withRetry(c).Complete(ctx, req)
		cancel()
		if err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
		u := resp.Usage
		t.Logf("call %d: input %d, cache read %d, cache write %d, uncached %d",
			i+1, u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.InputTokens-u.CacheReadTokens-u.CacheWriteTokens)
		if u.CacheReadTokens < 0 || u.CacheWriteTokens < 0 || u.CacheReadTokens+u.CacheWriteTokens > u.InputTokens {
			t.Fatalf("call %d: cache tokens are not a subset of InputTokens: %+v", i+1, u)
		}
		usages = append(usages, u)
	}
	return usages
}

// AnyCacheRead reports whether any call read from the cache.
func AnyCacheRead(usages []llms.Usage) bool {
	for i := range usages {
		if usages[i].CacheReadTokens > 0 {
			return true
		}
	}
	return false
}
