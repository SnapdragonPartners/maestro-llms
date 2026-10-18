//go:build integration

package anthropicvertex_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
)

// TestIntegrationVertexAnthropicContextOverflow is the ADR-0026 live
// trigger on Vertex. 1.1M tokens (~6.6 MB) exceeds the 1M window of the
// default claude-opus-4-6, so it is rejected unbilled; a
// VERTEX_ANTHROPIC_MODEL with a larger window would be billed instead, so
// the test skips for any other model.
func TestIntegrationVertexAnthropicContextOverflow(t *testing.T) {
	if m := envOr("VERTEX_ANTHROPIC_MODEL", "claude-opus-4-6"); m != "claude-opus-4-6" {
		t.Skipf("prompt is sized for claude-opus-4-6, not %s", m)
	}
	livetest.ContextOverflow(t, newVertexClient(t), 1_100_000)
}
