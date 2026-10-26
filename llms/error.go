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
	// ErrorKindContextLength is a request whose input does not fit the
	// model's context window (ADR-0026). It is not retryable: the same
	// request fails again, so the caller must shorten the conversation
	// (trim or compact the history) first. Adapters report it only where
	// the provider signals overflow deterministically; any other overflow
	// stays ErrorKindBadRequest, and Ollama silently truncates instead of
	// erroring.
	ErrorKindContextLength ErrorKind = "context_length"
	// ErrorKindUnknown is an unclassified failure.
	ErrorKindUnknown ErrorKind = "unknown"
)

// ProviderError is a classified error returned by a provider client. It
// represents a failure of (or before) the provider call itself; local limiter
// rejections use LimitError instead so middleware can tell the two apart.
type ProviderError struct {
	Provider   ProviderID
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
// bad_request, content_policy, and context_length are not. Unknown is
// treated as retryable (conservative: a single retry is usually cheaper
// than a missed transient).
func (e *ProviderError) Retryable() bool {
	switch e.Kind {
	case ErrorKindAuth, ErrorKindConfig, ErrorKindBadRequest, ErrorKindContentPolicy,
		ErrorKindContextLength:
		return false
	default:
		return true
	}
}

// LimitError is returned when a local or shared rate limiter rejects a request
// before any provider call is made. It is deliberately distinct from
// ProviderError so retry/circuit middleware can distinguish "we throttled
// locally" from "the provider returned 429": a LimitError is never evidence
// that the provider is unhealthy, so the circuit breaker treats it as
// neutral (ADR-0033).
type LimitError struct {
	// Cause is the error that ended the wait, when one did: the caller's
	// context error (context.Canceled or context.DeadlineExceeded) when the
	// caller's context ended the wait. It is nil for a temporary rejection
	// (capacity exhausted, MaxWait exceeded) and for a Permanent one.
	// Unwrap exposes it, so errors.Is(err, context.Canceled) holds.
	Cause    error
	Provider ProviderID
	Model    string
	Reason   string
	// RetryAfter is how long the caller should wait before retrying; zero if
	// the limiter did not provide a hint.
	RetryAfter time.Duration
	// Permanent reports that the request is not admissible under the limits
	// in force (for example its estimate exceeds the bucket's capacity), so
	// it is not retryable without changing the request or the configured
	// limits.
	Permanent bool
}

// Error implements error.
func (e *LimitError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("%s/%s: rate limited locally: %s", e.Provider, e.Model, e.Reason)
	}
	return fmt.Sprintf("%s/%s: rate limited locally", e.Provider, e.Model)
}

// Unwrap exposes Cause for errors.Is/errors.As.
func (e *LimitError) Unwrap() error { return e.Cause }

// Retryable reports whether retrying the same request can succeed: true for
// a temporary rejection (capacity exhausted, MaxWait exceeded), false when
// the rejection is Permanent or a Cause (the caller's context) ended the
// wait. Callers should wait RetryAfter (if non-zero) first.
func (e *LimitError) Retryable() bool { return !e.Permanent && e.Cause == nil }

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

// RetryAfter returns the backoff hint a known maestro-llms error carries: the
// provider's Retry-After (from *ProviderError) or the limiter's hint (from
// *LimitError). It returns 0 when there is no hint or the error is neither
// type. Symmetric with Retryable so retry/circuit middleware can wait the
// suggested delay before the next attempt.
func RetryAfter(err error) time.Duration {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.RetryAfter
	}
	var le *LimitError
	if errors.As(err, &le) {
		return le.RetryAfter
	}
	return 0
}
