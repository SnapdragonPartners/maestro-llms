//go:build integration

package anthropic_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
)

// ADR-0040: image, PDF and text documents as user content parts.
func TestIntegrationAnthropicImageInput(t *testing.T) {
	livetest.ImageInput(t, newAnthropic(t, anthropicModel()))
}

func TestIntegrationAnthropicPDFInput(t *testing.T) {
	livetest.PDFInput(t, newAnthropic(t, anthropicModel()))
}

func TestIntegrationAnthropicTextDocumentInput(t *testing.T) {
	livetest.TextDocumentInput(t, newAnthropic(t, anthropicModel()))
}

// ADR-0041: image and document blocks inside tool_result.
func TestIntegrationAnthropicToolResultPDF(t *testing.T) {
	livetest.ToolResultPDF(t, newAnthropic(t, anthropicModel()))
}

func TestIntegrationAnthropicToolResultCachedPDF(t *testing.T) {
	livetest.ToolResultCachedPDF(t, newAnthropic(t, anthropicModel()))
}

func TestIntegrationAnthropicToolResultImage(t *testing.T) {
	livetest.ToolResultImage(t, newAnthropic(t, anthropicModel()))
}
