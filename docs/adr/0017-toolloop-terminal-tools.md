# 0017. Tool loop terminal tools and executed limit turn

- **Status:** Accepted; amended by [ADR-0019](0019-toolloop-deadline-and-no-iteration-limit.md) (negative `MaxIterations` is unlimited), and by [ADR-0032](0032-enum-like-types-v1.md) (`OutcomeKind` may grow after v1; switches need a `default` case)
- **Date:** 2026-10-15

## Context

`toolloop.Run` (ADR-0011) ends successfully only when the model returns a
turn with no tool calls. A successful tool execution always triggers another
provider call, and the only way an executor can stop the loop is to return
an error, which yields `OutcomeToolError`, a failure.

That rules out the most portable structured-output pattern: the model
investigates with ordinary tools, then calls a terminal tool
(`submit_findings`, `finish`) whose schema-validated arguments *are* the
result. Callers either pay at least one extra model call and hope the model
stops (risking duplicate or contradictory submissions), or abuse a sentinel
`Execute` error and treat `OutcomeToolError` as success (issue #53, raised
by buggrit).

ADR-0011 listed "no terminal/state-transition tools" as a binding non-goal.
That bullet guarded against absorbing **Maestro's agent runtime**, where
terminal tools drive state transitions, escalation, and persistence. A
stop flag on a tool result carries none of that. It is a stop condition
like `MaxIterations`, decided by app code, with no state the loop
interprets.

ADR-0011 also pinned a **pre-execute** `MaxIterations` stop: the
limit-hitting turn's tool calls were appended but never executed. With
terminal tools that makes the final permitted turn useless. A submission
on that turn is dropped and reported as `MaxIterations`.

## Decision

Supersede ADR-0011 on exactly two points. Everything else in ADR-0011,
including the rest of the non-goals list, stands.

**1. Result-level terminal marker.** `toolloop.ToolResult` gains
`Stop bool`. When an executor returns `Stop: true`, the loop appends the
turn's tool results and returns the new `OutcomeTerminalTool`, with
`Outcome.TerminalCall` (the terminating `llms.ToolCall`) and
`Outcome.TerminalResult` alongside the usual `Response`, `Messages`,
`TotalUsage`, and `Iterations`. `Err` is nil.

- The flag lives on the **result**, not on `Tool`, so the executor
  validates first. An invalid submission returns `IsError: true` and the
  model resubmits in the same loop; only a valid one sets `Stop`.
- **Return-statement semantics within a turn.** Tool calls in a turn are
  an ordered list (`ChatResponse.ToolCalls`, provider order) and execute
  in order. A `Stop` result ends the turn at that call. Earlier calls have
  already run; later calls are **not executed**. Each skipped call gets a
  synthesized `IsError` result (`not executed: tool loop ended by "<name>"`)
  so every tool call stays paired and no side effects run after the model
  said it was done. No `OnToolCall` event is emitted for skipped calls,
  because they were never attempted. The terminating call's event is
  emitted normally and shows `Result.Stop`.
- **Well-formed transcript.** `Messages` ends with the turn's tool-result
  message, so it passes validation middleware (ADR-0006) and can be fed
  back into `Complete` to continue the conversation.
- `Stop` together with `IsError` contradicts itself and is rejected as
  `OutcomeToolError`. Unknown-tool recovery results never set `Stop`.
- The first `Stop` in a turn wins. Later calls are skipped, so no second
  terminal call can execute.

**2. The limit-hitting turn is executed.** When the response that reaches
`MaxIterations` contains tool calls, the loop now executes them and appends
their results. If one sets `Stop`, the outcome is `OutcomeTerminalTool`:
the terminal check runs before the limit check. Otherwise the outcome is
`OutcomeMaxIterations`, and no further provider call is made.
`MaxIterations = N` therefore means "at most N tool-calling turns are
executed" rather than "N-1 executed, the Nth dropped".

## Consequences

- Structured output through a terminal tool is first-class and costs no
  extra model call. Sentinel-error workarounds can be removed.
- **Behavior change (pre-1.0 minor):** callers relying on the limit turn's
  tools *not* executing will see one more batch of executions. The
  `OutcomeMaxIterations` transcript is now well-formed instead of ending
  with unresolved tool calls. Ship in a minor version bump.
- `OutcomeKind` grows one value. Exhaustive `switch` statements in callers
  should add a case. The set stays closed; further kinds still need an ADR.
- Neutrality is unchanged: `Stop` is a boolean the app decides,
  `TerminalCall`/`TerminalResult` are the toolkit's own types, and the
  event structs gain no fields. The ADR-0011 guardrails against agent
  state, persistence, and audit concepts remain binding.
- No `MAESTRO_DIVERGENCES.md` row: `toolloop` is new surface with no
  pre-extraction Maestro equivalent.

## References

- ADR-0011: tool loop helper. Superseded here only for the terminal-tool
  non-goal and the pre-execute `MaxIterations` stop.
- ADR-0006: validation middleware pairing rules the terminal transcript
  satisfies.
- `docs/toolloop-proposal.md`: original design; see its ADR-0017 note.
- Issue #53.

## Amendment 2026-10-16 — amended by ADR-0019

[ADR-0019](0019-toolloop-deadline-and-no-iteration-limit.md) makes a
negative `MaxIterations` mean no iteration limit. "At most N tool-calling
turns are executed" applies to positive N; with `NoIterationLimit` the loop
stops only on a final answer, a terminal tool, an error, or its context.
