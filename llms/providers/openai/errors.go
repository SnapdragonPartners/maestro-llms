package openai

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/openai/openai-go"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// classifyError maps SDK and context errors to a *llms.ProviderError using the
// SDK's typed *openai.Error (real HTTP status + response headers) rather than
// string-parsing the message.
func classifyError(model string, err error) error {
	if err == nil {
		return nil
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &llms.ProviderError{
			Provider: providerName, Model: model,
			Kind: llms.ErrorKindTimeout, Message: "request deadline exceeded", Cause: err,
		}
	case errors.Is(err, context.Canceled):
		return &llms.ProviderError{
			Provider: providerName, Model: model,
			Kind: llms.ErrorKindTimeout, Message: "request canceled", Cause: err,
		}
	}

	var apiErr *openai.Error
	if !errors.As(err, &apiErr) {
		return &llms.ProviderError{
			Provider: providerName, Model: model,
			Kind: llms.ErrorKindUnknown, Message: err.Error(), Cause: err,
		}
	}

	pe := &llms.ProviderError{
		Provider:   providerName,
		Model:      model,
		StatusCode: apiErr.StatusCode,
		Kind:       kindForStatus(apiErr.StatusCode),
		Message:    apiErr.Error(),
		Cause:      err,
	}
	if apiErr.Response != nil {
		pe.RetryAfter = parseRetryAfter(apiErr.Response.Header)
	}
	return pe
}

func kindForStatus(status int) llms.ErrorKind {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return llms.ErrorKindAuth
	case status == http.StatusTooManyRequests:
		return llms.ErrorKindRateLimited
	case status == http.StatusRequestTimeout:
		return llms.ErrorKindTimeout
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return llms.ErrorKindBadRequest
	case status >= 500:
		return llms.ErrorKindUnavailable
	default:
		return llms.ErrorKindUnknown
	}
}

// parseRetryAfter reads the Retry-After header (delta-seconds or HTTP-date).
func parseRetryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
