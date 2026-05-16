package google

import (
	"errors"
	"net/http"

	"google.golang.org/genai"

	"github.com/SnapdragonPartners/maestro-llms/llms/providers/internal/apierr"
)

// classifyError maps SDK and context errors to a *llms.ProviderError via the
// shared classifier, using the genai typed APIError for the real HTTP status.
// genai's APIError does not expose response headers, so Retry-After is not
// available (the classifier handles a nil header).
func classifyError(model string, err error) error {
	return apierr.Classify(providerName, model, err, func(e error) (int, http.Header, string, bool) {
		var ae genai.APIError
		if !errors.As(e, &ae) {
			return 0, nil, "", false
		}
		return ae.Code, nil, ae.Error(), true
	})
}
