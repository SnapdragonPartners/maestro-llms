# 0020. Tool loop `OutcomeEmptyResponse`: an empty response is reported, not appended

- **Status:** Accepted; amended by [ADR-0032](0032-outcome-kind-grows.md) (`OutcomeKind` may grow after v1; switches need a `default` case)
- **Date:** 2026-10-16

## Context

A reasoning model can spend its whole output budget on reasoning and
return no visible content and no tool calls. Every adapter drops
reasoning content, so on every provider the result is an assistant
`Message` with no text and no tool calls. `StopReason` is then the
provider's raw length reason: `length` (vLLM, Ollama), `max_tokens`
(Anthropic), `max_output_tokens` (OpenAI), `MAX_TOKENS` (Google).

`toolloop.Run` (ADR-0011) appended that message verbatim and returned
`OutcomeFinalAnswer`. A caller continuing from `Outcome.Messages` was then
rejected by the toolkit's own Validation middleware (`message N
(assistant): empty content`). Providers reject empty assistant messages
too, so the transcript the loop documents as continuable was not sendable
(#85, observed with GLM-5.3 on LunaRoute: 0 output tokens, 32768
reasoning).

## Decision

Amend ADR-0011's outcome set. Everything else in ADR-0011, ADR-0017 and
ADR-0019 stands.

- **An empty response** is one with no tool calls and no non-empty text
  (no content parts, or only empty text parts; some adapters emit an
  empty text part, which providers also reject). The check is the same on
  every provider and does not depend on `StopReason`.
- **It is not appended.** `Outcome.Messages` ends at the last well-formed
  point (the initial messages, or the previous turn's tool results), so
  the caller can append a message, such as a nudge to finish, and send it.
- **It returns the new `OutcomeEmptyResponse`** (`"empty_response"`)
  instead of `OutcomeFinalAnswer`. `Response` is the empty response, so
  `StopReason` and `Usage` show why. Its usage is counted in `TotalUsage`,
  `OnIteration` still fires for it, `Iterations` counts only tool-calling
  turns as before, and `Err` is nil.
- **The Validation middleware's error** for an empty assistant message now
  names the usual cause and says to drop the message. The rule itself is
  unchanged.
- **The `ChatResponse.Message` doc** notes that it can be empty, for plain
  `Complete` loops that append responses themselves.

## Rejected alternatives

- **Keep `OutcomeFinalAnswer` and document that it may be empty.** This is
  smaller, but "the model answered" and "the model produced nothing" call
  for different handling. A caller that prints `out.Response.Text` would
  silently show an empty answer. v1 freezes the outcome set, so the
  distinction belongs in `Kind`.
- **Normalize `StopReason` and add `OutcomeLength`.** This is more precise,
  but it is a core API change across five adapters, and "empty" is the
  condition that breaks the transcript whatever the cause. Normalizing
  `StopReason` is a separate pre-v1 question.

## Consequences

- **Behavior change (pre-1.0 minor):** a response with no text and no tool
  calls now returns `OutcomeEmptyResponse` and is absent from `Messages`.
  Exhaustive `switch out.Kind` statements need a case. Callers that
  stripped a trailing empty assistant message themselves (buggrit's
  `wellFormed`) can drop that workaround.
- `OutcomeKind` grows one value. The set stays closed; further kinds still
  need an ADR.
- Neutrality is unchanged: no new `Config` or event fields.
- No `MAESTRO_DIVERGENCES.md` row: `toolloop` has no pre-extraction
  Maestro equivalent, and the validation change only adds a hint to an
  existing error.

## References

- ADR-0011: tool loop helper. Amended here for the outcome set.
- ADR-0006: Validation middleware, whose empty-content rule is unchanged.
- ADR-0016: `Usage.ReasoningTokens`, which shows where the budget went.
- Issue #85; #86 (reasoning-effort control) is related but separate.
