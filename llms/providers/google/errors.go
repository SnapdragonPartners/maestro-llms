package google

import (
	"errors"
	"net/http"
	"strings"

	"google.golang.org/genai"

	"github.com/SnapdragonPartners/maestro-llms/llms/providers/internal/apierr"
)

// classifyError maps SDK and context errors to a *llms.ProviderError via the
// shared classifier, using the genai typed APIError for the real HTTP status.
// genai's APIError does not expose response headers, so Retry-After is not
// available (the classifier handles a nil header).
func classifyError(model string, err error) error {
	return apierr.ClassifyOverflow(providerName, model, err, func(e error) (int, http.Header, string, bool) {
		var ae genai.APIError
		if !errors.As(e, &ae) {
			return 0, nil, "", false
		}
		return ae.Code, nil, ae.Error(), true
	}, isContextOverflow)
}

// Context-window overflow signals (ADR-0026). Gemini sends 400
// INVALID_ARGUMENT with no structured code, and the wording differs by
// backend, so both fragments must appear. Captured 2026-10-18 with
// gemini-2.5-flash (testdata/*_input_token_count.json):
//
//	Gemini API: "The input token count exceeds the maximum number of tokens allowed (1048576)."
//	Vertex AI:  "The input token count exceeds the maximum number of tokens allowed 1048576."
//
// The Vertex wording was captured from generateContent on Vertex; Client
// speaks only the Gemini API today, but the Vertex EmbeddingClient shares
// this classifier. Older documented wording, "The input token count (N) exceeds the maximum
// number of tokens allowed (M).", also matches. An oversized
// maxOutputTokens is not an error: the API clamps it.
const (
	inputTokenCountText = "input token count"
	exceedsMaximumText  = "exceeds the maximum number of tokens allowed"
)

func isContextOverflow(err error) bool {
	var ae genai.APIError
	if !errors.As(err, &ae) || ae.Status != "INVALID_ARGUMENT" {
		return false
	}
	return strings.Contains(ae.Message, inputTokenCountText) && strings.Contains(ae.Message, exceedsMaximumText)
}
