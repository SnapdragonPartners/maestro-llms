package llms

import (
	"errors"
	"fmt"
	"time"
)

// ErrorKind is a normalized classification of a provider failure. Applications
// inspect it via errors.As on *ProviderError.
type ErrorKind string

const (
	// ErrorKindConfig is a client misconfiguration (missing model, bad URL).
	ErrorKindConfig ErrorKind = "config"
	// ErrorKindAuth is an authentication/authorization failure.
	ErrorKindAuth ErrorKind = "auth"
	// ErrorKindRateLimited is provider-side throttling (e.g. HTTP 429).
	ErrorKindRateLimited ErrorKind = "rate_limited"
	// ErrorKindTimeout is a request that exceeded its deadline.
	ErrorKindTimeout ErrorKind = "timeout"
	// ErrorKindUnavailable is a transient provider/service outage (5xx).
	ErrorKindUnavailable ErrorKind = "unavailable"
	// ErrorKindBadRequest is a malformed or rejected request.
	ErrorKindBadRequest ErrorKind = "bad_request"
	// ErrorKindContentPolicy is a content-policy refusal.
	ErrorKindContentPolicy ErrorKind = "content_policy"
	// ErrorKindUnknown is an unclassified failure.
	ErrorKindUnknown ErrorKind = "unknown"
)

// ProviderError is a classified error returned by a provider client. It
// represents a failure of (or before) the provider call itself; local limiter
// rejections use LimitError instead so middleware can tell the two apart.
type ProviderError struct {
	Provider   string
	Model      string
	Kind       ErrorKind
	StatusCode int
	Message    string
	// RetryAfter captures a provider Retry-After hint when available; zero if
	// none was supplied.
	RetryAfter time.Duration
	Cause      error
}

// Error implements error.
func (e *ProviderError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s/%s: %s: %s", e.Provider, e.Model, e.Kind, e.Message)
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s/%s: %s: %v", e.Provider, e.Model, e.Kind, e.Cause)
	}
	return fmt.Sprintf("%s/%s: %s (status %d)", e.Provider, e.Model, e.Kind, e.StatusCode)
}

// Unwrap exposes the underlying cause for errors.Is/errors.As.
func (e *ProviderError) Unwrap() error { return e.Cause }

// Retryable reports whether this error kind is normally worth retrying.
// rate_limited, timeout, and unavailable are retryable; auth, config,
// bad_request, and content_policy are not. Unknown is treated as retryable
// (conservative: a single retry is usually cheaper than a missed transient).
func (e *ProviderError) Retryable() bool {
	switch e.Kind {
	case ErrorKindAuth, ErrorKindConfig, ErrorKindBadRequest, ErrorKindContentPolicy:
		return false
	default:
		return true
	}
}

// LimitError is returned when a local or shared rate limiter rejects a request
// before any provider call is made. It is deliberately distinct from
// ProviderError so retry/circuit middleware can distinguish "we throttled
// locally" from "the provider returned 429".
type LimitError struct {
	Provider string
	Model    string
	Reason   string
	// RetryAfter is how long the caller should wait before retrying; zero if
	// the limiter did not provide a hint.
	RetryAfter time.Duration
}

// Error implements error.
func (e *LimitError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("%s/%s: rate limited locally: %s", e.Provider, e.Model, e.Reason)
	}
	return fmt.Sprintf("%s/%s: rate limited locally", e.Provider, e.Model)
}

// Retryable returns true: a local limiter rejection is the most retryable
// condition there is. Callers should wait RetryAfter (if non-zero) first.
func (e *LimitError) Retryable() bool { return true }

// Retryable reports whether err is a known maestro-llms error that is worth
// retrying. Errors that are neither *ProviderError nor *LimitError are treated
// as non-retryable (the caller must classify them itself).
func Retryable(err error) bool {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.Retryable()
	}
	var le *LimitError
	if errors.As(err, &le) {
		return le.Retryable()
	}
	return false
}
