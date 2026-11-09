//go:build integration

package openai_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
)

// ADR-0040: input_image and input_file.
func TestIntegrationOpenAIImageInput(t *testing.T) {
	livetest.ImageInput(t, newOpenAIToolLoopClient(t))
}

func TestIntegrationOpenAIPDFInput(t *testing.T) {
	livetest.PDFInput(t, newOpenAIToolLoopClient(t))
}

// ADR-0041: input_image and input_file in function_call_output.
func TestIntegrationOpenAIToolResultPDF(t *testing.T) {
	livetest.ToolResultPDF(t, newOpenAIToolLoopClient(t))
}

func TestIntegrationOpenAIToolResultImage(t *testing.T) {
	livetest.ToolResultImage(t, newOpenAIToolLoopClient(t))
}
