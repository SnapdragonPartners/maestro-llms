// Package apierr provides shared provider-error classification so every
// provider maps HTTP status, Retry-After, and context errors to
// *llms.ProviderError identically. It is SDK-agnostic: each provider passes a
// small Extract closure that pulls the status/headers/message out of that
// provider's own typed SDK error, keeping SDK imports in the provider package.
package apierr

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// Extract pulls classification inputs out of a provider SDK's typed error.
// ok is false when err is not that provider's API error type (the caller then
// classifies it as unknown). header may be nil.
type Extract func(err error) (status int, header http.Header, message string, ok bool)

// Classify maps err to a *llms.ProviderError. It returns nil for a nil err.
// Context deadline/cancellation are classified as timeout; a recognized API
// error uses its real status and Retry-After; anything else is unknown.
func Classify(provider, model string, err error, extract Extract) error {
	if err == nil {
		return nil
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		// A tripped deadline (often the per-attempt timeout middleware) is a
		// provider-slowness signal: classify as a retryable timeout.
		return &llms.ProviderError{
			Provider: provider, Model: model,
			Kind: llms.ErrorKindTimeout, Message: "request deadline exceeded", Cause: err,
		}
	case errors.Is(err, context.Canceled):
		// Caller-initiated cancellation / shutdown is NOT a provider-health
		// signal. Return it unwrapped so it stays non-retryable
		// (llms.Retryable is false for non-ProviderError/LimitError), the
		// circuit treats it as neutral, retry does not loop on it, and
		// callers can still match it with errors.Is(err, context.Canceled).
		return err
	}

	status, header, message, ok := extract(err)
	if !ok {
		return &llms.ProviderError{
			Provider: provider, Model: model,
			Kind: llms.ErrorKindUnknown, Message: err.Error(), Cause: err,
		}
	}

	pe := &llms.ProviderError{
		Provider:   provider,
		Model:      model,
		StatusCode: status,
		Kind:       KindForStatus(status),
		Message:    message,
		Cause:      err,
	}
	if header != nil {
		pe.RetryAfter = parseRetryAfter(header)
	}
	return pe
}

// KindForStatus maps an HTTP status to a normalized ErrorKind.
func KindForStatus(status int) llms.ErrorKind {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return llms.ErrorKindAuth
	case status == http.StatusTooManyRequests:
		return llms.ErrorKindRateLimited
	case status == http.StatusRequestTimeout:
		return llms.ErrorKindTimeout
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity ||
		status == http.StatusNotFound:
		// 404 (e.g. unknown/retired model) is a permanent client error, not
		// transient — classify as bad_request so it is not retried.
		return llms.ErrorKindBadRequest
	case status >= 500:
		return llms.ErrorKindUnavailable
	default:
		return llms.ErrorKindUnknown
	}
}

// maxRetryAfter caps the parsed Retry-After. The header is provider-controlled
// input: a huge delta-seconds value would overflow time.Duration to a negative
// number, and a multi-day delay is useless to a limiter/retry anyway, so any
// larger value is clamped to this bound.
const maxRetryAfter = 24 * time.Hour

// parseRetryAfter reads the Retry-After header (delta-seconds or HTTP-date),
// clamped to [0, maxRetryAfter].
func parseRetryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		// Compare in seconds before multiplying to avoid int64 overflow.
		if int64(secs) >= int64(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			if d > maxRetryAfter {
				return maxRetryAfter
			}
			return d
		}
	}
	return 0
}
