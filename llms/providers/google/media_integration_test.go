//go:build integration

package google_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
)

// ADR-0040: inline data on the Gemini API and on Vertex.
func TestIntegrationGoogleImageInput(t *testing.T) {
	livetest.ImageInput(t, newGoogleToolLoopClient(t))
}

func TestIntegrationGooglePDFInput(t *testing.T) {
	livetest.PDFInput(t, newGoogleToolLoopClient(t))
}

func TestIntegrationVertexGeminiImageInput(t *testing.T) {
	livetest.ImageInput(t, newVertexGemini(t, vertexModel()))
}

func TestIntegrationVertexGeminiPDFInput(t *testing.T) {
	livetest.PDFInput(t, newVertexGemini(t, vertexModel()))
}
