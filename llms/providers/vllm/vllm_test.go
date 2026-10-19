package vllm_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/openaicompat"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/vllm" //nolint:staticcheck // SA1019: this test covers the deprecated alias
)

// The alias must stay interchangeable with openaicompat: same client type,
// same option type, and every forwarding option applied (ADR-0027).
func TestAliasForwardsToOpenAICompat(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"m",`+
			`"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()

	opts := []openaicompat.Option{ // vllm.Option is the same type
		vllm.WithBaseURL(srv.URL + "/v1"),
		vllm.WithModel("m"),
		vllm.WithAPIKey("k"),
		vllm.WithHTTPClient(srv.Client()),
		vllm.WithMaxRetries(0),
	}
	var c *openaicompat.Client // vllm.Client is the same type
	c, err := vllm.New(opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "hi" || gotAuth != "Bearer k" || gotPath != "/v1/chat/completions" {
		t.Fatalf("text=%q auth=%q path=%q", resp.Text, gotAuth, gotPath)
	}
	if got := c.Model().Provider; got != llms.ProviderOpenAICompat {
		t.Fatalf("provider = %q, want %q", got, llms.ProviderOpenAICompat)
	}
	if _, err := vllm.New(vllm.WithModel("m")); err == nil {
		t.Fatal("missing base URL: want openaicompat's config error through the alias")
	}
}
