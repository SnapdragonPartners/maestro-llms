# 0044. `MaxTokens` caps billable output, reasoning included

- **Status:** Accepted
- **Date:** 2026-11-10

## Context

A hard per-run budget (#149) admits an attempt only when its worst case
fits. ADR-0043 bounds the input; the output side is `ChatRequest.MaxTokens`,
which maestro-agents sets on every budgeted call, lowered to what the budget
leaves. `BoundUnits.OutputTokensMax` is `MaxTokens`, and
`BoundUnits.TotalTokensMax` adds it to the input bound.

That is a bound only if `MaxTokens` caps what the provider bills as output.
Reasoning models bill thinking tokens as output (`Usage.BillableOutputTokens`
= `OutputTokens` + `ReasoningTokens`, ADR-0016). If a provider capped only
visible text, a model could think past the cap and overshoot the budget.
Providers' documentation is uneven on this point, so #149 committed to
verifying it live, per provider.

## Decision

1. **On `anthropic` (direct or on Vertex, via `anthropicvertex`), `openai`
   and `google` (Gemini API or Vertex), a positive `MaxTokens` caps
   `Usage.BillableOutputTokens`, reasoning included.** The adapters send it
   as `max_tokens`,
   `max_output_tokens` and `maxOutputTokens`, and each provider counts
   thinking against that limit. When the cap is hit, the response ends with
   the provider's length stop reason (`max_tokens`, `max_output_tokens`,
   `MAX_TOKENS`), and its `Message` can be empty if the model spent the
   whole cap reasoning.
2. **Nothing is promised elsewhere.**
   - `ollama` (`num_predict`) and `openaicompat` (`max_tokens`) leave it to
     the backend: a server or chat template may count reasoning differently,
     or not report it.
   - With `MaxTokens` unset, the cap is the adapter's or model's default
     (4,096 on Anthropic), so a budgeted call sets it. `BoundChat` gives no
     bound then (ADR-0043).
   - `ProviderParams` or a client body override can change the wire cap.
     They are outside the promise, as they are outside the input bound.
3. **Live tests are the tripwire.** For each backend in decision 1:
   - `livetest.OutputCapText` truncates a long answer at 64 tokens with the
     default reasoning.
   - `livetest.OutputCapReasoning` truncates a hard problem at 512 tokens
     with reasoning effort high. The run must report reasoning tokens.
   - Each run must end with the provider's length stop reason, so the cap
     was actually hit, and report positive billable output at most
     `MaxTokens`. A run that finishes under the cap, or doesn't reason when
     the scenario requires it, is retried.

   The reasoning scenario runs on each provider's reasoning-effort test
   model (the same `*_REASONING_MODEL` / `*_LEVEL_MODEL` overrides). First
   run (2026-11-10), billable output (visible + reasoning) against the cap,
   text model / reasoning model:

   | Backend, model | Text, cap 64 | Reasoning, cap 512 |
   | --- | --- | --- |
   | Anthropic, Haiku 4.5 / Opus 5.5 | 64 (64 + 0) | 512 (0 + 512) |
   | Anthropic on Vertex, Opus 4.6 / Opus 5.5 | 64 (64 + 0) | 512 (0 + 512) |
   | OpenAI, gpt-5.1 / gpt-5-mini | 64 (64 + 0) | 512 (0 + 512) |
   | Gemini API, gemini-3.8-flash / gemini-3-flash-preview | 60 (0 + 60) | 508 (21 + 487) |
   | Gemini on Vertex, gemini-3.8-flash / gemini-3-flash-preview | 60 (1 + 59) | 508 (20 + 488) |

   Gemini stops a few tokens under the cap and spends its default thinking
   even on the text request; both stay within the cap.

## Consequences

- No API change. This records a guarantee, and the live tests that guard
  it.
- A budget can treat `BoundUnits.TotalTokensMax` as the attempt's worst
  case on these backends. Elsewhere it needs its own output policy.
- A cap hit mid-reasoning returns an empty reply with a length stop reason.
  The budget has paid for it, so the app decides whether to retry with a
  larger cap or stop. `toolloop` reports it as `OutcomeEmptyResponse`
  (ADR-0020).
- A provider that started billing reasoning beyond the cap would break hard
  budgets. The live tests would catch it, and the fix would be a
  per-provider exclusion, as for the input bound.

## References

- #149 and its engineering-review thread ("Output side")
- [ADR-0016](0016-usage-reasoning-tokens.md) (reasoning tokens in `Usage`)
- [ADR-0043](0043-input-bound.md) (the input bound)
- `llms/internal/livetest/outputcap.go`
