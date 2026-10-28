//go:build integration

package anthropicvertex_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"

	"cloud.google.com/go/auth/credentials"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/anthropic/anthropicvertex"
)

// enforceThinkingBinding plays an app that knows more about the model than
// the toolkit: it adds the preserved-thinking beta header and sets thinking
// with prefix_mismatch_behavior "error" on every request. anthropicvertex has
// no request-options hook, so this wraps the transport the test passes to
// WithHTTPClient. That client carries no Google auth: WithCredentials
// authenticates it (ADR-0029), so this test is also the live proof of that.
type enforceThinkingBinding struct{ base http.RoundTripper }

func (e enforceThinkingBinding) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil && r.Method == http.MethodPost {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		var body map[string]any
		if err := json.Unmarshal(b, &body); err == nil {
			body["thinking"] = map[string]any{
				"type":          "adaptive",
				"display":       "summarized",
				"block_binding": map[string]any{"prefix_mismatch_behavior": "error"},
			}
			body["output_config"] = map[string]any{"effort": "max"}
			if nb, err := json.Marshal(body); err == nil {
				b = nb
			}
		}
		r = r.Clone(r.Context())
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.ContentLength = int64(len(b))
		r.Header.Set("anthropic-beta", "thinking-binding-controls-2026-08-01")
	}
	return e.base.RoundTrip(r)
}

// TestIntegrationVertexAnthropicReasoningReplay is the ADR-0021 replay check
// on Vertex. VERTEX_ANTHROPIC_REASONING_MODEL defaults to claude-opus-5-5.
func TestIntegrationVertexAnthropicReasoningReplay(t *testing.T) {
	project := os.Getenv("VERTEX_PROJECT")
	if project == "" {
		t.Skip("VERTEX_PROJECT not set")
	}
	region := envOr("VERTEX_REGION", "global")
	model := envOr("VERTEX_ANTHROPIC_REASONING_MODEL", "claude-opus-5-5")
	creds, err := credentials.DetectDefault(&credentials.DetectOptions{
		Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
	})
	if err != nil {
		t.Skipf("no Google credentials (run `gcloud auth application-default login`): %v", err)
	}
	hc := &http.Client{Transport: enforceThinkingBinding{base: http.DefaultTransport}}
	c, err := anthropicvertex.New(
		anthropicvertex.WithVertex(project, region),
		anthropicvertex.WithModel(model),
		anthropicvertex.WithCredentials(creds),
		anthropicvertex.WithHTTPClient(hc),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Logf("Anthropic on Vertex: project=%s region=%s model=%s", project, region, model)
	livetest.ReasoningReplay(t, c)
}
