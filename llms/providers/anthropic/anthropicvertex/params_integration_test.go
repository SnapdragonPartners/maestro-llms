//go:build integration

package anthropicvertex_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
)

// The ProviderParams middleware composes with the Vertex request middleware.
func TestIntegrationVertexAnthropicProviderParams(t *testing.T) {
	livetest.ProviderParamsOverride(t, newVertexClient(t), map[string]any{"max_tokens": 16})
}
