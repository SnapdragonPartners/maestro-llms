//go:build integration

package anthropicvertex_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
)

// ADR-0040: the shared Anthropic mapping on Vertex.
func TestIntegrationVertexAnthropicImageInput(t *testing.T) {
	livetest.ImageInput(t, newVertexClient(t))
}

func TestIntegrationVertexAnthropicPDFInput(t *testing.T) {
	livetest.PDFInput(t, newVertexClient(t))
}
