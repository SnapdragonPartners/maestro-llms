package openaicompat

import (
	"errors"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3"

	"github.com/SnapdragonPartners/maestro-llms/llms/providers/internal/apierr"
)

// classifyError maps SDK and context errors to a *llms.ProviderError via the
// shared classifier. vLLM speaks the OpenAI HTTP error envelope shape, so
// the typed *openai.Error from the SDK carries the real HTTP status and
// response headers — same path as the sibling openai package.
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

// Context-window overflow signals (ADR-0026). vLLM and OpenAI-compatible
// gateways in front of it do not agree on a code, so the structured code is
// checked first and two documented message fragments second. Captured
// 2026-10-18 through LunaRoute (testdata/lunaroute_*_context_length.json):
//
//	deepseek-4.1-flash: code "context_length_exceeded", message
//	  "This model's maximum context length is 1048576 tokens. Your requested
//	  output of 16 tokens leaves room for at most 1048560 input tokens, ..."
//	glm-5.3: code "UPSTREAM_ERROR", message
//	  "The input (1100017 tokens) is longer than the model's context length
//	  (1048576 tokens)."
//
// The first fragment is also vLLM's own long-standing wording ("This
// model's maximum context length is N tokens. However, you requested M
// tokens ..."), which counts requested completion tokens too.
const (
	contextLengthCode     = "context_length_exceeded"
	maxContextLengthText  = "maximum context length is"
	longerThanContextText = "longer than the model's context length"
)

func isContextOverflow(err error) bool {
	var ae *openai.Error
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Code == contextLengthCode ||
		strings.Contains(ae.Message, maxContextLengthText) ||
		strings.Contains(ae.Message, longerThanContextText)
}
