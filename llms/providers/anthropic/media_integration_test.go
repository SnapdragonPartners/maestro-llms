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
