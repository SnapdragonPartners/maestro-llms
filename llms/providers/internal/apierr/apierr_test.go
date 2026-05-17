package apierr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

func TestKindForStatus(t *testing.T) {
	cases := map[int]llms.ErrorKind{
		401: llms.ErrorKindAuth,
		403: llms.ErrorKindAuth,
		429: llms.ErrorKindRateLimited,
		408: llms.ErrorKindTimeout,
		400: llms.ErrorKindBadRequest,
		404: llms.ErrorKindBadRequest,
		422: llms.ErrorKindBadRequest,
		500: llms.ErrorKindUnavailable,
		503: llms.ErrorKindUnavailable,
		418: llms.ErrorKindUnknown,
	}
	for status, want := range cases {
		if got := KindForStatus(status); got != want {
			t.Errorf("KindForStatus(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter(http.Header{}); d != 0 {
		t.Errorf("missing header = %v, want 0", d)
	}
	h := http.Header{}
	h.Set("Retry-After", "7")
	if d := parseRetryAfter(h); d != 7*time.Second {
		t.Errorf("seconds = %v, want 7s", d)
	}
	h.Set("Retry-After", "-3")
	if d := parseRetryAfter(h); d != 0 {
		t.Errorf("negative = %v, want 0", d)
	}
	h.Set("Retry-After", "garbage")
	if d := parseRetryAfter(h); d != 0 {
		t.Errorf("garbage = %v, want 0", d)
	}
	// Over-cap and overflow-prone values clamp to maxRetryAfter (never go
	// negative from int64 overflow).
	h.Set("Retry-After", "100000") // 27.7h > 24h cap
	if d := parseRetryAfter(h); d != maxRetryAfter {
		t.Errorf("over-cap = %v, want %v", d, maxRetryAfter)
	}
	h.Set("Retry-After", "9223372036") // ~maxInt64/1e9; would overflow if multiplied
	if d := parseRetryAfter(h); d != maxRetryAfter {
		t.Errorf("overflow-prone = %v, want %v (must not be negative)", d, maxRetryAfter)
	}
	h.Set("Retry-After", time.Now().Add(5*time.Second).UTC().Format(http.TimeFormat))
	if d := parseRetryAfter(h); d <= 0 || d > 6*time.Second {
		t.Errorf("http-date = %v, want ~5s", d)
	}
	h.Set("Retry-After", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
	if d := parseRetryAfter(h); d != 0 {
		t.Errorf("past http-date = %v, want 0", d)
	}
}

func okExtract(status int, h http.Header, msg string) Extract {
	return func(error) (int, http.Header, string, bool) { return status, h, msg, true }
}

func notMineExtract() Extract {
	return func(error) (int, http.Header, string, bool) { return 0, nil, "", false }
}

func TestClassifyNilReturnsNil(t *testing.T) {
	if err := Classify("p", "m", nil, notMineExtract()); err != nil {
		t.Fatalf("nil err must classify to nil, got %v", err)
	}
}

func TestClassifyDeadlineExceededIsRetryableTimeout(t *testing.T) {
	for _, in := range []error{context.DeadlineExceeded,
		fmt.Errorf("wrapped: %w", context.DeadlineExceeded)} {
		err := Classify("p", "m", in, okExtract(500, nil, "x"))
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindTimeout {
			t.Fatalf("deadline %v -> want timeout ProviderError, got %v", in, err)
		}
		if !errors.Is(err, in) {
			t.Fatalf("classified error should unwrap to the cause")
		}
		if !llms.Retryable(err) {
			t.Fatalf("deadline-exceeded must stay retryable (per-attempt timeout composition)")
		}
	}
}

func TestClassifyCanceledIsUnwrappedAndNonRetryable(t *testing.T) {
	for _, in := range []error{context.Canceled,
		fmt.Errorf("wrapped: %w", context.Canceled)} {
		err := Classify("p", "m", in, okExtract(500, nil, "x"))
		// Returned as-is (not converted to *llms.ProviderError, even when the
		// input is itself wrapped): non-retryable, but still discoverable via
		// errors.Is for callers and shutdown logic.
		var pe *llms.ProviderError
		if errors.As(err, &pe) {
			t.Fatalf("context.Canceled must NOT be classified as a ProviderError, got %v", err)
		}
		if llms.Retryable(err) {
			t.Fatalf("context.Canceled must be non-retryable, got retryable %v", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("context.Canceled must remain errors.Is-matchable, got %v", err)
		}
	}
}

func TestClassifyUnrecognizedIsUnknown(t *testing.T) {
	err := Classify("p", "m", errors.New("weird"), notMineExtract())
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindUnknown || pe.Message != "weird" {
		t.Fatalf("unrecognized -> want unknown with message, got %+v", err)
	}
}

func TestClassifyAPIErrorWithRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "12")
	err := Classify("anthropic", "claude", errors.New("boom"),
		okExtract(429, h, "rate limited"))
	var pe *llms.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *llms.ProviderError, got %v", err)
	}
	if pe.Provider != "anthropic" || pe.Model != "claude" || pe.StatusCode != 429 ||
		pe.Kind != llms.ErrorKindRateLimited || pe.Message != "rate limited" ||
		pe.RetryAfter != 12*time.Second {
		t.Fatalf("unexpected classification: %+v", pe)
	}
	if !pe.Retryable() {
		t.Fatal("429 should be retryable")
	}
}
