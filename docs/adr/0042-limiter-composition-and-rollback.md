# 0042. Limiter composition (`ratelimit.All`) and reservation rollback

- **Status:** Accepted
- **Date:** 2026-11-09

## Context

maestro-agents enforces hard per-run token and cost budgets (#149). The
engineering review on #149 found that the existing limiter slot already
serves as the attempt-level admission point it asked for:

- `RecommendedConfig.Limiter` sits inside `RetryChat`, so `Reserve` runs once
  per attempt, before the provider call, on the attempt's context
  (`middleware.AttemptFromContext`, ADR-0031).
- A `*llms.LimitError{Permanent: true}`, or any error `llms.Retryable` does
  not recognize, is not retried and is neutral to the circuit (ADR-0033).
- A failed attempt, or a success with no reported usage, keeps its
  reservation (no `Commit`), so a budget limiter can charge the full
  reservation.

So ADR-0031's rejection of a `RecommendedConfig.Attempt` slot stands. Two
gaps remain. This ADR closes the first; the second (a labeled input bound
beside the estimate) is a separate decision.

1. **One limiter slot.** maestro-agents always composes its budget limiter
   with the host's rate limiter. Every enforcing consumer would otherwise
   write the same all-or-nothing composition with rollback.
2. **A rolled-back reservation looks like a failed attempt.** When a
   composition reserves the budget and a later limiter refuses, the budget's
   reservation gets `Release` without `Commit`, the signal for "sent and
   failed". The budget would charge in full for a call that never went out.
   `Reservation` is frozen (README "Stability contract (v1)"), so a new
   method can't be added to it.

## Decision

1. **`ratelimit.All(limiters ...Limiter) (Limiter, error)`** composes
   limiters into one that admits a request only when every limiter admits
   it. It rejects an empty list and a nil limiter with an error, as
   `NewInMemoryLimiter` rejects invalid config (ADR-0034).
   - `Reserve` reserves on each limiter in the order given, with the same
     `ReservationRequest`, and stops at the first refusal. It then rolls back
     the reservations already held, in reverse order, on
     `context.WithoutCancel(ctx)`, and returns the refusing limiter's error
     **unwrapped**, so retry and circuit classify it exactly as they would
     that limiter alone. Rollback errors are discarded, like `Release`
     errors in the rate-limit middleware.
   - The composite reservation commits the same usage to every member and
     releases them in reverse order, joining errors; a failing member does
     not stop the others. The first `Release` or `Rollback` ends it; later
     calls to either are no-ops.
   - Order is the caller's policy. The documented guidance is to put a
     limiter that waits (a rate limiter) before one that holds a scarce
     resource (a budget), so the wait happens before the budget is held.
2. **`ratelimit.Rollbacker`**, an optional capability a `Reservation` may
   implement, discovered by type assertion (like `LimiterStats`):
   `Rollback(ctx) error` undoes a reservation whose request was never sent.
   `All` calls it **instead of** `Release`, at most once, and never calls
   `Commit` or `Release` on that reservation afterwards, so `Rollback` must
   free everything `Release` would. A reservation without it is released,
   the conservative fallback (the limiter keeps its charge). The composite
   reservation implements `Rollbacker` itself, so an `All` nests in another.
3. **The built-in limiters do not implement `Rollbacker`.** A rolled-back
   `InMemoryLimiter` or `FromBucket` reservation is released and keeps its
   token charge, overcounting by one never-sent request, which is the safe
   direction for a rate limiter. With the recommended order (rate limiter
   first, budget last), a rate-limiter reservation is rolled back only when
   the budget refuses.

## Consequences

- Additive: one function and one optional interface. `Limiter`,
  `Reservation`, the middleware and `RecommendedConfig` are unchanged.
- A budget limiter can tell three outcomes apart: `Commit` (sent, usage
  known), `Release` without `Commit` (sent, outcome or usage unknown: charge
  in full), and `Rollback` (never sent: refund in full).
- Budget policy stays with the consumer; this package ships only the
  composition and the signal.
- Built-in limiters could gain `Rollback` (a full refund) later without a
  contract change.

## References

- #149 and its engineering-review thread
- [ADR-0031](0031-attempt-context-observer.md) (attempt context; no attempt slot)
- [ADR-0033](0033-limit-error-classification.md) (limiter rejections neutral to the circuit)
- [ADR-0034](0034-limiter-config-and-charging.md) (validated limiter config)
- `llms/ratelimit/all.go`
