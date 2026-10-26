package llms

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestLimitErrorClassification pins ADR-0033: a Permanent rejection or a
// canceled wait is not retryable; a temporary rejection or a deadline is.
// The context error stays matchable through wrapping.
func TestLimitErrorClassification(t *testing.T) {
	cases := []struct {
		err       *LimitError
		name      string
		retryable bool
	}{
		{name: "temporary", err: &LimitError{Reason: "wait exceeded MaxWait"}, retryable: true},
		{name: "permanent", err: &LimitError{Reason: "can never fit", Permanent: true}},
		{name: "canceled", err: &LimitError{Reason: "context done", Cause: context.Canceled}},
		// A deadline may be per-attempt (timeout inside retry), so it stays
		// retryable, as a provider deadline is; the retry loop stops on the
		// caller's own context.
		{name: "deadline", err: &LimitError{Reason: "context done", Cause: context.DeadlineExceeded}, retryable: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := fmt.Errorf("outer: %w", tc.err)
			if got := Retryable(wrapped); got != tc.retryable {
				t.Fatalf("Retryable = %v, want %v", got, tc.retryable)
			}
			if tc.err.Cause != nil && !errors.Is(wrapped, tc.err.Cause) {
				t.Fatalf("errors.Is(err, %v) = false through the LimitError", tc.err.Cause)
			}
			var le *LimitError
			if !errors.As(wrapped, &le) || le != tc.err {
				t.Fatal("errors.As must still resolve the *LimitError")
			}
		})
	}
}
