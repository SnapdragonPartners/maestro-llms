# 0016. `Usage.ReasoningTokens` + `BillableOutputTokens` — normalize the reasoning split

- **Status:** Accepted
- **Date:** 2026-05-31

## Context

Reasoning-class models (Gemini 3 series, OpenAI o-series, Anthropic
Claude 4 with extended thinking) emit a class of output tokens that are
not part of the visible response: the model's internal "thoughts" that
the provider returns as a separately-metered count. These tokens
**count against the request's output cap**
(`MaxOutputTokens` / `MaxTokens`), but they do not appear in the
assistant message a consumer sees.

Prior to this ADR `llms.Usage` carried only `InputTokens` and
`OutputTokens`. Each adapter mapped its provider's "output_tokens"
field into `Usage.OutputTokens`. **That meaning silently varied** with
the provider's wire convention:

| Provider | Wire `output_tokens` meaning | Reasoning field exposed by SDK |
|---|---|---|
| **Gemini (genai)** | Visible only (`CandidatesTokenCount`) | `UsageMetadata.ThoughtsTokenCount` — additive, separate |
| **OpenAI (Responses + Chat Completions)** | Total (visible + reasoning) | `OutputTokensDetails.ReasoningTokens` / `CompletionTokensDetails.ReasoningTokens` — subset of `output_tokens` |
| **Anthropic** | Total (visible + thinking when extended-thinking on) | none — no field separates the two |
| **Ollama** | Total | none (no thinking concept) |

So a chat to `gemini-3.1-pro-preview-customtools` with `MaxTokens=1024`
returned `OutputTokens=36, StopReason=MAX_TOKENS` — confusing because
the toolkit had silently dropped 988 thinking tokens. Meanwhile an
o-series call returning 250 output tokens (200 reasoning + 50 visible)
also reported `OutputTokens=250`, conflating the two semantics for any
cross-provider consumer.

`llms.Usage` is the place a consumer goes to reconcile what happened
on the wire. The toolkit's job is to make the meaning of its fields
the same across providers.

## Decision

Add two fields to `llms.Usage` and normalize cross-provider semantics:

```go
type Usage struct {
    InputTokens          int
    OutputTokens         int  // VISIBLE response output only — cross-provider normalized
    TotalTokens          int
    ReasoningTokens      int  // separately-metered thinking; ADDITIVE to OutputTokens
    BillableOutputTokens int  // what the provider bills as "output"; usually Output + Reasoning
    EmbeddingTokens      int
    CacheReadTokens      int
    CacheWriteTokens     int
    ProviderRequestID    string
}
```

**Semantic contract:**

- `OutputTokens` = **visible assistant response only**. This is the
  number a consumer counting the rendered message would count.
- `ReasoningTokens` = the **separately-metered thinking subset** where
  the provider exposes it (additive to `OutputTokens`).
- `BillableOutputTokens` = **what the provider charges you as
  "output."** For most models this equals
  `OutputTokens + ReasoningTokens`. It exists so consumers doing cost
  math read one field and don't have to track per-provider semantics.
- `TotalTokens` continues to be the provider's authoritative grand
  total (input + output + reasoning + cache + tool-use).

**Budget math:** a length-truncation fires when
`InputTokens + OutputTokens + ReasoningTokens` approaches the cap, not
when `OutputTokens` alone does. Callers seeing a small `OutputTokens`
paired with a length stop reason should read `ReasoningTokens` to
understand where the budget went.

**Per-adapter mapping:**

| Provider | OutputTokens | ReasoningTokens | BillableOutputTokens |
|---|---|---|---|
| Gemini | `CandidatesTokenCount` | `ThoughtsTokenCount` | `Candidates + Thoughts` |
| OpenAI Responses | `wire.OutputTokens − OutputTokensDetails.ReasoningTokens` | `OutputTokensDetails.ReasoningTokens` | `wire.OutputTokens` |
| OpenAI Chat Completions (vLLM) | `wire.CompletionTokens − CompletionTokensDetails.ReasoningTokens` | `CompletionTokensDetails.ReasoningTokens` (zero for most models) | `wire.CompletionTokens` |
| Anthropic | `wire.OutputTokens` (includes thinking when on; not separable) | 0 (not exposed) | `wire.OutputTokens` |
| Ollama | `eval_count` | 0 | `eval_count` |

The `Output + Reasoning == Billable` identity holds for every adapter
**except Anthropic with extended thinking on**, where the wire
`output_tokens` includes thinking and the SDK exposes no split. There,
`OutputTokens` carries the combined number (documented limitation) and
`BillableOutputTokens` mirrors it.

## Why not just leave each adapter as-is

Considered and rejected. That would make `Usage.OutputTokens` mean
different things on different providers — visible-only on Gemini,
visible+reasoning on OpenAI — and require every consumer to read
godoc per-provider. The toolkit's whole point is cross-provider
neutrality at the contract layer.

## Why the breaking change for OpenAI o-series consumers

This is a pre-1.0 contract refinement. The previous `Usage.OutputTokens`
was the OpenAI wire `output_tokens`, which for o-series silently
included reasoning. Code that read `Usage.OutputTokens` as the
billing-relevant number now under-counts; the migration is to read
`Usage.BillableOutputTokens` (which carries the exact same value the
old `OutputTokens` did). Non-reasoning OpenAI calls are unaffected
because `reasoning_tokens=0` makes the subtraction a no-op. ADR-0016
is the right time to make this consistent rather than carrying the
inconsistency into v1.0.

## Consequences

- The reported Gemini case ("32/36 tokens · MAX_TOKENS at
  MaxTokens=1024") becomes self-explanatory: `OutputTokens=36`,
  `ReasoningTokens=988`, `BillableOutputTokens=1024`. The
  `examples/chat` demo surfaces `ReasoningTokens` in the bubble
  footer when > 0; non-reasoning models keep the terse footer.
- Cross-provider cost math: read `BillableOutputTokens`. One field,
  one semantic, no per-provider branching.
- Cross-provider response-size math: read `OutputTokens`. One field,
  visible-only, the same everywhere except Anthropic-with-thinking
  (documented).
- OpenAI o-series breaking semantic on `OutputTokens`. Migration is a
  read-from-different-field, not a math change.
- Anthropic and Ollama require no behavioral change beyond mirroring
  `OutputTokens` into `BillableOutputTokens` so cross-provider cost
  math doesn't have to special-case them.
- Future reasoning-capable models served via vLLM populate
  `ReasoningTokens` automatically through the Chat Completions
  `completion_tokens_details` path; no adapter change needed when one
  arrives.
- No `MAESTRO_DIVERGENCES.md` row needed (the original Maestro
  implementation did not surface either of these fields; nothing to
  diverge from).

## References

- `llms/usage.go` — `ReasoningTokens`, `BillableOutputTokens`.
- `llms/providers/google/convert.go` — Gemini mapping.
- `llms/providers/openai/chatconvert.go` — Responses normalization.
- `llms/providers/vllm/convert.go` — Chat Completions normalization
  via `buildUsage`.
- `llms/providers/anthropic/convert.go` — `Billable = Output`,
  documented thinking limitation.
- `llms/providers/ollama/wire.go` — `Billable = Output`, no reasoning.
- `examples/chat/server.go` + `ui/app.js` — bubble footer surfaces
  reasoning when > 0.
- genai docs: `ThoughtsTokenCount`.
- openai-go: `responses.ResponseUsageOutputTokensDetails.ReasoningTokens`,
  `CompletionUsageCompletionTokensDetails.ReasoningTokens`.
- ADR-0001 (process). Cross-provider precedent: ADR-0010
  (`ToolCall.ProviderSignature` was similarly a per-provider concept
  surfaced through a cross-provider neutral field).
