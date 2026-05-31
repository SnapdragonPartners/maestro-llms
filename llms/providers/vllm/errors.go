package vllm

import (
	"errors"
	"net/http"

	"github.com/openai/openai-go"

	"github.com/SnapdragonPartners/maestro-llms/llms/providers/internal/apierr"
)

// classifyError maps SDK and context errors to a *llms.ProviderError via the
// shared classifier. vLLM speaks the OpenAI HTTP error envelope shape, so
// the typed *openai.Error from the SDK carries the real HTTP status and
// response headers — same path as the sibling openai package.
func classifyError(model string, err error) error {
	return apierr.Classify(providerName, model, err, func(e error) (int, http.Header, string, bool) {
		var ae *openai.Error
		if !errors.As(e, &ae) {
			return 0, nil, "", false
		}
		var h http.Header
		if ae.Response != nil {
			h = ae.Response.Header
		}
		return ae.StatusCode, h, ae.Error(), true
	})
}
