package middleware

import "context"

// attemptKey is the context key RetryChat/RetryEmbeddings use to tag each
// attempt's context with its attempt number.
type attemptKey struct{}

func withAttempt(ctx context.Context, attempt int) context.Context {
	return context.WithValue(ctx, attemptKey{}, attempt)
}

// AttemptFromContext reports the 1-based attempt number RetryChat or
// RetryEmbeddings assigned to the attempt ctx belongs to. It returns false
// when no retry layer sits outside the caller (for example, a MetricsChat
// placed outside retry, or a chain with no retry at all). With nested retry
// layers it reports the innermost one's count.
//
// The number is the retry loop's own counter, so it stays exact when an
// attempt is rejected before reaching an inner layer: a *llms.LimitError from
// the rate limiter is retryable and consumes an attempt that an inner
// ContextObserver never sees, leaving a gap in the numbers it observes
// (ADR-0031).
func AttemptFromContext(ctx context.Context) (int, bool) {
	n, ok := ctx.Value(attemptKey{}).(int)
	return n, ok
}
