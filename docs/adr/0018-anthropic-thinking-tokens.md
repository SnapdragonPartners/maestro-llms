# 0018. Anthropic `ReasoningTokens` from `output_tokens_details.thinking_tokens`

- **Status:** Accepted
- **Date:** 2026-10-16
- **Partially supersedes:** ADR-0016 (the Anthropic row of the per-adapter mapping and its "not separable" exception)

## Context

ADR-0016 normalized `Usage` so that `OutputTokens` is visible output,
`ReasoningTokens` is separately metered thinking, and
`BillableOutputTokens` is what the provider bills. Anthropic was the one
documented exception. The wire `output_tokens` includes thinking when it is
on, and nothing separated the two, so the adapter reported the combined count
in `OutputTokens` with `ReasoningTokens = 0`. That kept
`Output + Reasoning == Billable` arithmetically true, but `OutputTokens`
was not visible-only, which is the one meaning ADR-0016 gives it
everywhere else. (ADR-0016 describes this as the identity "not holding";
precisely, the identity held while the visible/thinking split was
missing.) For Anthropic with thinking on, a consumer could not tell how
much of the output was thinking (#66).

The Messages API now returns `usage.output_tokens_details.thinking_tokens`,
and anthropic-sdk-go v1.82 (#61) exposes it as
`Usage.OutputTokensDetails.ThinkingTokens`. The SDK documents it as the raw
reasoning count, "always ≤ `output_tokens`", computed by re-tokenizing the
reasoning, so it may differ from the exact generation count by a few
tokens.

Observed live on 2026-10-16:

| Endpoint / model | Thinking | `output_tokens` | `thinking_tokens` |
|---|---|---|---|
| First-party, Haiku 4.5 (`enabled`, budget 1024) | on | 825 | 461–503 |
| First-party, Opus 5.5 (`adaptive`) | on | 587 | 200 |
| First-party, Opus 5.5 | off | 17 | 0 |
| Vertex `global`, Opus 5.5 (`adaptive`) | on | 529 | 194 |
| Vertex `global`, Opus 5.5 | off | 13 | 0 |
| Vertex `global`, Opus 4.6 (`adaptive`) | on | 780 | 418 |
| Vertex `global`, Opus 4.6 | off | 25 | **field absent** |

In every thinking-on case, the remainder (`output_tokens - thinking_tokens`)
matches the size of the visible text, which confirms that `thinking_tokens`
is a **subset** of `output_tokens`, the same as OpenAI's
`reasoning_tokens`, and is not billed on top of it.

## Decision

The Anthropic mapping becomes the OpenAI shape:

| Provider | OutputTokens | ReasoningTokens | BillableOutputTokens |
|---|---|---|---|
| Anthropic, details present | `wire.output_tokens − thinking_tokens` | `thinking_tokens` (clamped to `[0, output_tokens]`) | `wire.output_tokens` |
| Anthropic, details absent | `wire.output_tokens` | 0 | `wire.output_tokens` |

- **Presence is checked, not assumed.** The SDK marks the field required,
  but Opus 4.6 on Vertex omits it when thinking is off. A zero-valued
  struct must not be read as a measured `0`, so the adapter checks
  `JSON.ThinkingTokens.Valid()`. Absent details mean the previous behavior.
  That behavior is also exactly right when thinking was off, since there is
  nothing to split.
- **The clamp** keeps a re-tokenization overshoot from producing a
  negative `OutputTokens`.
- `TotalTokens` stays `input_tokens + output_tokens` (the wire total).

With this, `OutputTokens` means visible output for every adapter, and
`ReasoningTokens` reports the thinking split wherever the provider exposes
it.

## Consequences

- **Behavior change for Anthropic consumers with thinking on:**
  `OutputTokens` shrinks to visible-only, and `ReasoningTokens` becomes
  non-zero. Per ADR-0016, cost math already belongs on
  `BillableOutputTokens`, which is unchanged. Recorded as divergence A7
  and called out in the v0.9.0 release notes.
- Responses without thinking are unchanged. They report either
  `thinking_tokens: 0` or no details.
- The toolkit still has no provider-neutral "enable thinking" option.
  Callers turn it on with `anthropic.WithRequestOptions` (the live test
  does this). Adding such an option is out of scope here.

## References

- #66; ADR-0016; `llms/usage.go`.
- `llms/providers/anthropic/convert.go` (`buildUsage`), with unit tests
  `TestUsageThinkingTokens` and the live `TestIntegrationAnthropicThinkingUsage`.
- anthropic-sdk-go v1.82.0 `OutputTokensDetails.ThinkingTokens` godoc.
