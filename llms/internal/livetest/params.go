package livetest

import (
	"context"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// ProviderParamsOverride asks for a long answer with MaxTokens 1024 while
// params caps output at 16 tokens through the provider's own field. The
// provider must accept the merged request, and the small output proves the
// override won over the mapped MaxTokens (ADR-0024).
func ProviderParamsOverride(t *testing.T, c llms.ChatClient, params map[string]any) llms.ChatResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	resp, err := withRetry(c).Complete(ctx, llms.ChatRequest{
		Messages:       []llms.Message{llms.UserText("Write a 500-word essay about lighthouses.")},
		MaxTokens:      1024,
		ProviderParams: map[llms.ProviderID]map[string]any{c.Model().Provider: params},
	})
	if err != nil {
		t.Fatalf("Complete with ProviderParams %v: %v", params, err)
	}
	t.Logf("stop %q, billable output %d, text %q", resp.StopReason, resp.Usage.BillableOutputTokens, truncate(resp.Text, 60))
	if resp.Usage.BillableOutputTokens > 32 {
		t.Fatalf("billable output = %d with a 16-token cap override; the mapped MaxTokens (1024) won", resp.Usage.BillableOutputTokens)
	}
	return resp
}
