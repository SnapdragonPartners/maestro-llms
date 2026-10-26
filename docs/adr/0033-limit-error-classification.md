# 0033. Limiter rejections: neutral to the circuit, permanent and caller-ended waits not retryable

- **Status:** Accepted
- **Date:** 2026-10-26

## Context

The #84 step 2 whole-API audit found that a local rate-limit rejection can
open the circuit breaker. Three documented behaviors combine:

- [ADR-0004](0004-retry-circuit-reuse-llms-retryable.md) made `*llms.LimitError`
  "always retryable", and its doc said "a local limiter rejection is the
  most retryable condition there is".
- [ADR-0005](0005-circuit-open-error.md) counts every `llms.Retryable`
  failure against the breaker.
- The recommended chain is `validation → retry → timeout → circuit → rate
  limit → provider`, so the breaker sees every limiter rejection.

Reproduced with `RecommendedChat` and an `InMemoryLimiter` of 100 tokens per
minute: one request whose estimate exceeds capacity fails with "request can
never be admitted". Retry still spends all five attempts on it, which also
reaches the default `FailureThreshold` of five, so the breaker opens. The
next small, valid request gets `*CircuitOpenError` for 30 seconds, and the
provider is never called. Local backpressure was taken as a sign that the
provider was down.

The audit found two related problems:

- A request that can never fit is retried as if it might fit next time.
- A caller cancelling its context while it waits for capacity gets a
  retryable `LimitError` with no link to the context error, so
  `errors.Is(err, context.Canceled)` is false. ADR-0026 and the toolloop's
  `classifyStop` rely on matching context errors.

All three are documented semantics. After v1 they couldn't change without
contradicting covered docs, so they change now, in v0.15 (#84 step 3).

The underlying error is that two questions were treated as one: "is this
worth retrying?" and "is this evidence the provider is unhealthy?" For
provider errors the answers usually agree. For limiter rejections they
don't: a temporary rejection is worth retrying, and no limiter rejection
says anything about the provider.

## Decision

1. **The circuit breaker treats every `*llms.LimitError` as neutral**,
   retryable or not. It goes through the existing neutral branch of
   `breaker.record`: no failure count, no success count, no state change,
   and a HalfOpen probe that ends in a `LimitError` releases the probe gate
   without closing or reopening the breaker. Provider errors are counted as
   before.
2. **`LimitError.Permanent`** (and `bucket.LimitError.Permanent`) means that
   the request isn't admissible under the limits in force, so it isn't
   retryable without changing the request or the configured limits. It is
   not a claim that no configuration could ever admit it. The in-memory
   limiter sets it when the estimate exceeds the maximum capacity or the
   requested concurrency exceeds the maximum; the bucket wait sets it for a
   backend's `Impossible` result.
3. **`LimitError.Cause`** (and `bucket.LimitError.Cause`) holds the caller's
   context error when the caller's context ended the wait, and `Unwrap`
   exposes it. So `errors.As` still finds the `LimitError`, and `errors.Is`
   matches `context.Canceled` or `context.DeadlineExceeded`. `MaxWait` is
   the limiter's own budget, not the caller's context: it remains a
   temporary rejection with no `Cause`.
4. **`LimitError.Retryable()` is `!Permanent && Cause == nil`.** A temporary
   rejection (capacity exhausted, or `MaxWait` exceeded) stays retryable and
   still honors `RetryAfter`. `llms.Retryable` remains the single classifier
   (ADR-0004).
5. **`ratelimit.FromBucket` carries `Permanent` and `Cause` across** into the
   `*llms.LimitError` it returns, along with `RetryAfter`.

## Consequences

- A burst of local throttling, or a single oversized request, no longer
  stops traffic that is within limits. The breaker reacts only to the
  provider.
- An oversized request now fails after one attempt instead of after
  `MaxAttempts`. Callers see the same error type, and `Permanent` tells them
  to change the request or the limits.
- A cancelled wait is reported as a `LimitError` that also matches the
  context error, so toolloop and app code that test `errors.Is(err,
  context.Canceled)` behave the same whether the cancellation hit the
  provider call or the limiter wait.
- **Behavior change for consumers**, for the release notes:
  - local limiter rejections no longer trip the circuit;
  - permanent and caller-ended rejections are no longer retried;
  - code that assumed every `LimitError` is retryable must check
    `llms.Retryable` (or `Permanent` and `Cause`).

  No signature changes: the new fields are additive.
- `LimitError` and `bucket.LimitError` gain an `error` field. They remain
  comparable at compile time, but `==` on two values with non-comparable
  dynamic causes would panic. Comparability of these error structs isn't
  promised; PR 5 of #84 narrows the README's comparability promise to a
  named list.
- Another middleware could later want "unhealthy provider" signals other
  than `Retryable`. If that happens, it gets its own ADR. This decision adds
  no predicate option.

## Amends

- [ADR-0004](0004-retry-circuit-reuse-llms-retryable.md): `LimitError` is no
  longer "always retryable", and the circuit doesn't count `LimitError` even
  when it is retryable.
- [ADR-0005](0005-circuit-open-error.md): the "only `llms.Retryable`
  failures count" rule gains the `LimitError` exception.
- [ADR-0025](0025-bucket-distributed-limiter.md): `bucket.LimitError` gains
  `Permanent` and `Cause`, and `FromBucket` maps both.

## References

- `llms/error.go`: `LimitError`, `Retryable`
- `llms/middleware/circuit.go`: `breaker.record`
- `llms/ratelimit/memory.go`, `llms/ratelimit/bucket.go`,
  `bucket/bucket.go`, `bucket/internal/core/core.go`
- `llms/middleware/limit_classification_test.go`: the boundary tests
  (impossible request, temporary exhaustion, caller cancellation and
  deadline, `MaxWait`, provider 429, provider outage, and a HalfOpen probe
  rejected by the limiter)
- `docs/MAESTRO_DIVERGENCES.md` row M6
- #84 (the v1 plan and the v0.15 scope)
