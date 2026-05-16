# 0004. Retry and circuit middleware reuse `llms.Retryable`, not a ported classifier

- **Status:** Accepted
- **Date:** 2026-05-16

## Context

The v0.3 retry and circuit-breaker middleware must decide which errors are
worth retrying / counting as circuit failures. Maestro's retry middleware
(`pkg/agent/middleware/resilience/retry`) uses a **blocklist classifier** over
its `llmerrors.Error` type: retry everything *except* `context.Canceled`,
auth (401/403), bad request (400/404), and `ServiceUnavailable`.

`maestro-llms` already has a single, typed, `errors.As`-able error model:

- `*llms.ProviderError` with `Kind` and `Retryable()` (retryable kinds:
  `rate_limited`, `timeout`, `unavailable`, and `unknown`-conservative; not
  `auth`, `config`, `bad_request`, `content_policy`),
- `*llms.LimitError` (always retryable; carries `RetryAfter`),
- `llms.Retryable(err)` which resolves both via `errors.As`,
- `RetryAfter` on both error types for provider/limiter backoff hints.

This model is itself a recorded divergence from Maestro (divergence X3:
unified classification via `apierr`). Porting Maestro's separate blocklist on
top would mean **two** retry-classification policies that can disagree.

## Decision

Retry and circuit middleware classify **solely** via `llms.Retryable(err)`
and honor `RetryAfter`. We do **not** port Maestro's `llmerrors` blocklist
classifier. No second classification surface is introduced; an error is
retryable iff the core error model says so.

Recorded as divergence **M1** in `docs/MAESTRO_DIVERGENCES.md`.

## Consequences

- One classification policy, owned by `llms/error.go`, reused by every
  middleware and already dogfooded by the live integration retry helpers.
- Behavioral shift from Maestro: errors Maestro's blocklist would have
  retried (anything not explicitly excluded) now retry **only** if they map
  to a retryable `ProviderError` kind or a `LimitError`. Non-retryable kinds
  fail fast. This is the intended, stricter behavior; M1 is the cut-over
  checklist entry for validating Maestro flows that relied on broad retry.
- `unknown` stays conservatively retryable (see `ProviderError.Retryable`),
  so genuinely-unclassified transients are still retried once per policy.
- If a real consumer ever needs pluggable classification, that is a new ADR
  introducing a narrow predicate option — not a reason to fork the model now.

## References

- `llms/error.go` — `ProviderError`, `LimitError`, `Retryable`, `RetryAfter`
- `docs/MAESTRO_DIVERGENCES.md` — X3 (unified classification), M1 (this divergence)
- ADR-0002 (Ollama `apierr` typing feeds this model), ADR-0001 (process)
- Maestro reference: `pkg/agent/middleware/resilience/retry/policy.go`
