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

// ADR-0041: media inside tool_result on Vertex.
func TestIntegrationVertexAnthropicToolResultPDF(t *testing.T) {
	livetest.ToolResultPDF(t, newVertexClient(t))
}

func TestIntegrationVertexAnthropicToolResultImage(t *testing.T) {
	livetest.ToolResultImage(t, newVertexClient(t))
}
