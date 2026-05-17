package middleware

import (
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/ratelimit"
)

// RecommendedConfig parameterizes the recommended middleware chain. The zero
// value is usable: validation/retry/circuit are always applied (with default
// configs), and the optional layers are skipped.
type RecommendedConfig struct {
	// Pointer-bearing interface fields first (fieldalignment).

	// Limiter, if non-nil, adds the rate-limit reservation middleware.
	Limiter ratelimit.Limiter
	// Estimator is used only when Limiter is non-nil; nil => DefaultEstimator.
	Estimator TokenEstimator
	// Observer, if non-nil, adds the innermost metrics middleware.
	Observer Observer
	// Retry policy. Zero value => default schedule with jitter OFF; use
	// DefaultRetryConfig() for the recommended jittered policy.
	Retry RetryConfig
	// Circuit policy. Zero value => DefaultCircuitConfig().
	Circuit CircuitConfig
	// Timeout per attempt. <= 0 omits the timeout middleware entirely.
	Timeout time.Duration
}

// RecommendedChat composes base with the spec's recommended middleware order:
//
//	validation -> retry -> per-attempt timeout -> circuit -> rate limit -> metrics -> base
//
// This is an opinionated convenience. The ordering is load-bearing (see the
// spec's "Recommended order" and retry-vs-reservation tradeoffs): each retry
// attempt independently flows through timeout, circuit, and the rate-limit
// reservation, and a malformed request is rejected before any of that work.
// Applications that need a different order or subset compose ChainChat
// directly — that remains the primitive; this is just the common default.
//
// Validation, retry, and circuit are always included (defaults when their
// configs are zero). Timeout is included only when cfg.Timeout > 0; rate
// limiting only when cfg.Limiter != nil; metrics only when cfg.Observer != nil.
func RecommendedChat(base llms.ChatClient, cfg RecommendedConfig) llms.ChatClient {
	mws := []ChatMiddleware{ValidationChat(), RetryChat(cfg.Retry)}
	if cfg.Timeout > 0 {
		mws = append(mws, TimeoutChat(cfg.Timeout))
	}
	mws = append(mws, CircuitChat(cfg.Circuit))
	if cfg.Limiter != nil {
		mws = append(mws, RateLimitChat(cfg.Limiter, estimatorOrDefault(cfg.Estimator)))
	}
	if cfg.Observer != nil {
		mws = append(mws, MetricsChat(cfg.Observer))
	}
	return ChainChat(base, mws...)
}

// RecommendedEmbeddings is the embedding-side counterpart. It omits validation
// (there is no structural embedding validation — see ADR-0006); the order is
// otherwise the same: retry -> timeout -> circuit -> rate limit -> metrics.
func RecommendedEmbeddings(base llms.EmbeddingClient, cfg RecommendedConfig) llms.EmbeddingClient {
	mws := []EmbeddingMiddleware{RetryEmbeddings(cfg.Retry)}
	if cfg.Timeout > 0 {
		mws = append(mws, TimeoutEmbeddings(cfg.Timeout))
	}
	mws = append(mws, CircuitEmbeddings(cfg.Circuit))
	if cfg.Limiter != nil {
		mws = append(mws, RateLimitEmbeddings(cfg.Limiter, estimatorOrDefault(cfg.Estimator)))
	}
	if cfg.Observer != nil {
		mws = append(mws, MetricsEmbeddings(cfg.Observer))
	}
	return ChainEmbeddings(base, mws...)
}

func estimatorOrDefault(est TokenEstimator) TokenEstimator {
	if est == nil {
		return DefaultEstimator{}
	}
	return est
}
