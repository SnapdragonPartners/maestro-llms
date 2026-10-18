package openai

import (
	"errors"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3"

	"github.com/SnapdragonPartners/maestro-llms/llms/providers/internal/apierr"
)

// classifyError maps SDK and context errors to a *llms.ProviderError via the
// shared classifier, using the SDK's typed *openai.Error for the real HTTP
// status and response headers (not message string-parsing).
func classifyError(model string, err error) error {
	return apierr.ClassifyOverflow(providerName, model, err, func(e error) (int, http.Header, string, bool) {
		var ae *openai.Error
		if !errors.As(e, &ae) {
			return 0, nil, "", false
		}
		var h http.Header
		if ae.Response != nil {
			h = ae.Response.Header
		}
		return ae.StatusCode, h, ae.Error(), true
	}, isContextOverflow)
}

// contextLengthCode is OpenAI's structured error code for a context-window
// overflow (ADR-0026). Captured 2026-10-18 from POST /v1/responses with
// gpt-4o-mini (testdata/context_length_exceeded.json):
//
//	{"message": "Your input exceeds the context window of this model. ...",
//	 "type": "invalid_request_error", "param": "input",
//	 "code": "context_length_exceeded"}
//
// The embeddings endpoint reports the same condition with a null code, so
// it is matched on the message (testdata/embeddings_context_length.json):
//
//	"Invalid 'input': maximum context length is 8192 tokens."
//
// An oversized max_output_tokens is not an error: the API clamps it.
const (
	contextLengthCode    = "context_length_exceeded"
	maxContextLengthText = "maximum context length is"
)

// isContextOverflow matches the structured code first and falls back to the
// documented message fragment for endpoints that send no code.
func isContextOverflow(err error) bool {
	var ae *openai.Error
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Code == contextLengthCode || strings.Contains(ae.Message, maxContextLengthText)
}
