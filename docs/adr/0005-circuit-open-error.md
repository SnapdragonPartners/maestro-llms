# 0005. Circuit breaker emits a distinct, non-retryable `CircuitOpenError`

- **Status:** Accepted
- **Date:** 2026-05-16

## Context

The v0.3 circuit-breaker middleware (PR 3) must, when Open, reject calls
before they reach the inner client, and it must compose correctly with the
retry middleware. Per the spec's recommended order **retry is *outside* the
circuit** (`retry → timeout → circuit → ratelimit → provider`).

Two coupled questions:

1. **What error does an open breaker return?** Maestro's breaker emits a
   `ServiceUnavailableError` that is wired to drive Maestro's agent SUSPEND
   state — app-specific coupling that must not enter this package.
2. **Is that error retryable?** If it satisfied `llms.Retryable`, the
   outer retry middleware would, within a single `Complete` call, keep
   re-invoking an Open breaker that deterministically rejects until
   `OpenTimeout` — burning the entire retry/backoff budget doing nothing.
   The breaker's whole purpose is to *fail fast*.

Also: what counts as a breaker failure? `ProviderError` kinds like `auth` /
`bad_request` are caller problems, not provider-health signals; counting them
would trip the breaker on a misconfiguration and mask the real error.

## Decision

Introduce `middleware.CircuitOpenError{Provider, Model, RetryAfter}`:

- A **distinct typed error**, neither `*llms.ProviderError` nor
  `*llms.LimitError`. Therefore `llms.Retryable` returns **false** for it
  (the core helper only classifies those two types) — the breaker fails fast
  and the outer retry middleware stops immediately instead of spinning.
- `RetryAfter` carries the remaining time until a probe is admitted as a
  *caller hint*; it does not make the error retryable for middleware.
- Recovery is via the `Open → HalfOpen` transition on a **later** call after
  `OpenTimeout`, not via in-call retry.
- Only `llms.Retryable(err)` failures count against the breaker; successes
  reset a Closed failure streak; non-retryable/caller errors are **neutral**
  (pass through, no state change) — consistent with ADR-0004.

Lives in the `middleware` package (it is produced only by this middleware;
consumers using it already import the package). Recorded as divergence **M4**.

## Consequences

- Fail-fast works as intended; `Retry(Circuit(client))` does not waste
  attempts against an Open breaker (covered by a composition test).
- Callers classify it with `errors.As(err, *CircuitOpenError)`; it is
  deliberately outside the `Retryable` set, so no middleware retries it.
- Behavioral divergence from Maestro: no `ServiceUnavailableError`/SUSPEND
  coupling. Maestro must map `*CircuitOpenError` to its own
  suspend/backoff policy at the application boundary (M4 cut-over action).
- Misconfiguration (repeated `auth`/`bad_request`) no longer trips the
  breaker, so the real error is not masked by a circuit rejection.
- If a consumer ever needs the open-circuit error to participate in a
  higher-level retry/backoff, that is an application policy on top of
  `*CircuitOpenError`, not a change to its non-retryable nature here.

## References

- `llms/middleware/circuit.go` — `CircuitOpenError`, `breaker`
- `llms/error.go` — `llms.Retryable` (only `*ProviderError`/`*LimitError`)
- ADR-0004 (classification reuse), ADR-0001 (process)
- `docs/MAESTRO_DIVERGENCES.md` — M4
- Maestro reference: `pkg/agent/middleware/resilience/circuit/`
