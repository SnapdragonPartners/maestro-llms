package anthropic

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared"

	"github.com/SnapdragonPartners/maestro-llms/llms/providers/internal/apierr"
)

// classifyError maps SDK and context errors to a *llms.ProviderError via the
// shared classifier, using the SDK's typed *anthropic.Error for the real HTTP
// status and response headers (not message string-parsing).
func classifyError(model string, err error) error {
	return apierr.ClassifyOverflow(providerName, model, err, func(e error) (int, http.Header, string, bool) {
		var ae *anthropic.Error
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

// promptTooLong is the message prefix Anthropic uses for a context-window
// overflow (ADR-0026). Anthropic has no structured code for it, only the
// invalid_request_error type. Captured 2026-10-18, identical on the direct
// API and on Vertex (testdata/prompt_too_long*.json):
//
//	"prompt is too long: 260024 tokens > 200000 maximum"
//
// An input that fits but leaves too little room for max_tokens is not an
// error: the API lowers max_tokens instead.
const promptTooLong = "prompt is too long"

func isContextOverflow(err error) bool {
	var ae *anthropic.Error
	if !errors.As(err, &ae) || ae.Type() != shared.ErrorTypeInvalidRequestError {
		return false
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(ae.RawJSON()), &body) != nil {
		return false
	}
	return strings.HasPrefix(body.Error.Message, promptTooLong)
}
