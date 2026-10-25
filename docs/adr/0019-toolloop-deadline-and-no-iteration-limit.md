# 0019. Tool loop `OutcomeDeadline` and `NoIterationLimit`

- **Status:** Accepted; amended by [ADR-0032](0032-enum-like-types-v1.md) (`OutcomeKind` may grow after v1; switches need a `default` case)
- **Date:** 2026-10-16

## Context

`toolloop.Run` (ADR-0011, ADR-0017) classifies a stop caused by the loop's
context only when it is a cancellation. `context.DeadlineExceeded` was
deliberately not counted as cancellation, so a run whose context deadline
expired ended as `OutcomeLLMError` (a `timeout` `*llms.ProviderError` from
the model call) or `OutcomeToolError` (an executor that was running when
the deadline passed). Neither can be told apart from a real provider or
tool failure. Callers that bound runs by wall-clock time (buggrit, #73)
had to inspect `ctx.Err()` themselves.

Those callers also had no clean way to say "no iteration ceiling".
`MaxIterations <= 0` meant the default of 8, so an unbounded run needed a
huge sentinel value. After #74 that is cheap, but it is still implicit.

## Decision

Amend ADR-0011 (outcome set, `MaxIterations` default) and ADR-0017
(`MaxIterations` semantics) on exactly these points. Everything else in
both ADRs stands.

**1. `OutcomeDeadline`.** A new `OutcomeKind` (`"deadline"`), returned when
a provider or executor error surfaces while the loop's own context has
`ctx.Err() == context.DeadlineExceeded`. It carries `Response`, `Messages`,
`Iterations`, and `TotalUsage` like the other non-final outcomes.

- **The loop's context is authoritative.** One choke point classifies
  every provider and executor error. If `ctx` is done, the kind follows
  `ctx.Err()`: `Canceled` gives `OutcomeCanceled`, `DeadlineExceeded`
  gives `OutcomeDeadline`, whatever the call returned. Otherwise an error
  matching `context.Canceled` is still `OutcomeCanceled` (X5), and any
  other error, including a `DeadlineExceeded` from a derived context, is
  `OutcomeLLMError` / `OutcomeToolError`.
- **A per-attempt deadline is not the loop's deadline.** The `Timeout`
  middleware, or an executor's own sub-deadline, expires while the loop's
  context is live. That remains an LLM or tool error, because the caller's
  time budget is not exhausted and the failure is real.
- **`Err` matches the context.** For `OutcomeDeadline`,
  `errors.Is(err, context.DeadlineExceeded)` holds; for `OutcomeCanceled`,
  `errors.Is(err, context.Canceled)` holds. The call's error is returned
  as-is when it already matches, and otherwise wrapped together with
  `ctx.Err()` so neither cause is lost. (Previously a canceled context
  paired with an unrelated provider error returned that error unwrapped,
  contradicting the documented `errors.Is` contract.)

**2. `NoIterationLimit`.** `Config.MaxIterations`:

- **Zero** still means the default of 8. That protects callers who never
  set the field.
- **Negative** means no iteration limit. `toolloop.NoIterationLimit = -1`
  names it so the choice is visibly deliberate. The loop then stops only
  on a final answer, a terminal tool, an error, or its context.
- **No context deadline is required.** The toolkit rejects malformed
  requests; it does not enforce good practice. A deliberately unbounded
  run, such as a daemon agent whose tool blocks waiting for events, is
  legitimate.

Two daemon patterns are documented in the README: one endless `Run` with a
blocking tool, whose transcript grows until it reaches the model's context
limit (#79 adds detection, #80 a compaction hook); and one `Run` per event,
with memory owned by the app.

## Consequences

- Callers can tell "ran out of time" from "something broke" by `Kind`
  alone. `OutcomeDeadline` is useful for bounded runs too.
- **Behavior change (pre-1.0 minor):** a run whose context deadline expires
  now returns `OutcomeDeadline` instead of `OutcomeLLMError` /
  `OutcomeToolError`. A canceled context now wins over a
  `DeadlineExceeded` from the call, and vice versa (the context decides).
- **Behavior change (pre-1.0 minor):** negative `MaxIterations` changes
  meaning from "the default of 8" to "unlimited". Callers that passed a
  negative value by mistake now get an unbounded run.
- `OutcomeKind` grows one value. Exhaustive `switch` statements need an
  `OutcomeDeadline` case. The set stays closed; further kinds still need
  an ADR.
- Neutrality is unchanged: no new `Config` or event fields, and no policy
  on how long a run may take.
- No `MAESTRO_DIVERGENCES.md` row: `toolloop` is new surface with no
  pre-extraction Maestro equivalent.

## References

- ADR-0011: tool loop helper. Amended here for the outcome set and the
  `MaxIterations` default.
- ADR-0017: terminal tools and the executed limit turn. Amended here for
  `MaxIterations` semantics.
- `docs/MAESTRO_DIVERGENCES.md` X5: caller cancel is not converted into a
  `*llms.ProviderError`; `DeadlineExceeded` is a retryable timeout.
- Issues #73, #74, #78; follow-ups #79, #80.
