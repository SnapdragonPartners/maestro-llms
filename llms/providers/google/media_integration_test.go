//go:build integration

package google_test

import (
	"os"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/google"
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

// ADR-0041: media in a function response. Multimodal function responses
// need Gemini 3, so these use defaultLevelModel (or VERTEX_GEMINI_MODEL on
// Vertex). On 2026-11-08 gemini-2.5-flash returned a 400 on the Gemini API
// ("Multimodal function responses are not supported for this model") but
// was accepted on Vertex and answered without seeing the media.
func TestIntegrationGoogleToolResultPDF(t *testing.T) {
	c, err := google.New(google.WithAPIKey(apiKey(t)), google.WithModel(defaultLevelModel))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	livetest.ToolResultPDF(t, c)
}

func TestIntegrationVertexGeminiToolResultPDF(t *testing.T) {
	livetest.ToolResultPDF(t, newVertexGemini(t, vertexToolResultModel()))
}

func TestIntegrationVertexGeminiToolResultImage(t *testing.T) {
	livetest.ToolResultImage(t, newVertexGemini(t, vertexToolResultModel()))
}

func vertexToolResultModel() string {
	if m := os.Getenv("VERTEX_GEMINI_MODEL"); m != "" {
		return m
	}
	return defaultLevelModel
}
