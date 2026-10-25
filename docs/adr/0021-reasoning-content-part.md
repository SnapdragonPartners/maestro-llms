# 0021. Round-trip provider reasoning as a `ContentReasoning` part

- **Status:** Accepted; amended by [ADR-0030](0030-toolloop-compaction.md) (a `Compact` hook edits history, so it must strip reasoning first)
- **Date:** 2026-10-17

## Context

Every adapter dropped reasoning content. For Anthropic that used to be
harmless, because the adapter never turned thinking on. It no longer is:
on current models (Opus 5.5, Sonnet 5.5, Haiku 5.5, Opus 5, Fable 5.x),
thinking runs adaptively when `thinking` is omitted, and on several of
them it cannot be turned off. So the adapter has been receiving
`thinking` / `redacted_thinking` blocks and throwing them away (#92).
In a tool loop on a default model, each turn started without the
reasoning that produced the previous one.

Anthropic's contract is to pass these blocks back unchanged and in order
("required when using tools with extended thinking", per the SDK's
`ThinkingBlock.Signature` doc), and let the API decide which ones the
current model can read. Stripping *every* block is accepted, as a recovery
rather than the steady state, which is why nothing failed.

Since 2026-08 a thinking block's signature also binds the conversation
prefix before it ("preserved thinking"). Editing, reordering or removing an
earlier turn while keeping later blocks invalidates them. On accounts
created on or after 2026-08-31, or on any request that sets
`thinking.block_binding.prefix_mismatch_behavior`, the API answers with a
400:

```text
messages.1.content.2: Invalid `signature` in `thinking` block. The block is
bound to a different conversation. ... Content before this block differs
from when it was created, first at `messages.0.content.0`.
```

(observed live, Opus 5.5, 2026-10-17). Removing *all* blocks remains valid.

ADR-0010 solved the per-tool-call version of this problem for Gemini
(`ToolCall.ProviderSignature`). Reasoning is not per tool call: it precedes
text as well as tool use, so it needs its own part.

## Decision

1. **`ContentReasoning` part with an opaque payload.**

   ```go
   ContentReasoning ContentPartType = "reasoning"

   type Reasoning struct {
       Provider     ProviderID // the adapter that produced it
       Text         string     // readable reasoning/summary; display only, never sent back
       ProviderData []byte     // the provider's own encoding, sent back unchanged
   }
   ```

   The Anthropic adapter captures each `thinking` / `redacted_thinking`
   block, in its original position, as one part. `ProviderData` holds the
   block's fields (`type`, `thinking`, `signature` / `data`), and replay is
   rebuilt from `ProviderData` only, so an app editing `Text` cannot alter
   what is sent. One opaque blob rather than typed signature/redacted fields
   lets other providers' reasoning state (OpenAI encrypted reasoning items,
   Gemini thought parts) reuse the part later without new surface. ADR-0010
   rejected a generic metadata bag on `ContentPart`. This is narrower: one
   part type, one owner, and an explicit `Provider`.

2. **Typed `ProviderID`.** `ModelRef.Provider` (and the `Provider` field of
   `ProviderError`, `LimitError`, `middleware.CircuitOpenError`,
   `middleware.Event` and `ratelimit.ReservationRequest`) changes from
   `string` to `llms.ProviderID`, with constants for this module's adapters.
   The set is open: an app implementing `ChatClient` for another provider
   defines its own value. The core compares it only for equality. A
   provider ID names the wire protocol, not the deployment, so
   `anthropicvertex` is `ProviderAnthropic`.

3. **Other providers' reasoning is dropped at one choke point.**
   `llms.ReasoningFrom(p, msgs)` removes reasoning parts whose `Provider`
   is not `p` (and any with nil `Reasoning`). Every adapter calls it before
   translating messages, so its content switch only ever sees its own
   reasoning, and a transcript can move between providers. This is done in
   the adapters, not in middleware, so an app that composes no middleware
   still can't send one provider's reasoning to another.

4. **`llms.WithoutReasoning(msgs)`** removes every reasoning part. It is the
   documented step before editing history (snipping turns, client-side
   compaction, #80), since removing all reasoning is always valid.

5. **Reasoning alone is not a reply.** A reasoning-only assistant turn is
   treated as empty everywhere an empty turn already is:
   `middleware.ValidationChat` rejects it with the existing hint, the
   Anthropic adapter rejects it as `bad_request`, and toolloop's
   `isEmptyResponse` ignores reasoning parts, so a response that spent its
   budget on thinking is still `OutcomeEmptyResponse` (ADR-0020 unchanged).
   Validation also accepts reasoning parts only on assistant messages, with
   a non-nil payload and non-empty `Provider`.

6. **On by default; no request-side thinking change.** The adapter does not
   start sending `thinking` or any beta header. It only stops dropping what
   the API already returns.

## Rejected alternatives

- **Middleware to strip foreign reasoning.** Opt-in by composition, so an
  app that forgets it gets a provider 400.
- **Default `prefix_mismatch_behavior: "drop_block"`,** so an edited
  history degrades instead of failing. This needs an explicit `thinking`
  type on every request plus a beta header. On models where omitting
  `thinking` means *no* thinking (Opus 4.7/4.8, 4.6), sending `adaptive`
  turns thinking on, and Haiku 4.5 does not take `adaptive`. It may be
  revisited with #86 (reasoning-effort control), which is where the
  adapter starts choosing a thinking configuration.
- **A client option to disable replay.** `WithoutReasoning` covers the
  same need per request without new client surface.
- **Typed signature/redacted fields in core.** These are Anthropic wire
  details. `ProviderData` keeps them in the adapter.

## Consequences

- Tool loops on default Anthropic models keep their reasoning across turns.
  Replayed blocks are billed as input like any other history.
- Every consumer of `ContentPart` payloads must handle the new pointer.
  `middleware.DefaultEstimator` counts `Reasoning.ProviderData` (and, fixing
  the same gap from ADR-0010, `ToolCall.ProviderSignature`), so replayed
  payloads can't slip past a limiter. `testllm` deep-copies both in its
  recordings.
- **New failure mode for apps that edit history:** editing an earlier turn
  and resending later thinking blocks is a 400 on enforced accounts. Call
  `llms.WithoutReasoning` first. `toolloop` only appends and keeps `System`
  and `Tools` fixed, so it is unaffected. (Amended by ADR-0030: with
  `Config.Compact` set, the hook edits history and must strip reasoning
  first; the shipped compactors do.)
- Consumers that switch exhaustively on `ContentPart.Type` will see a new
  value. Consumers that persist transcripts should store reasoning parts
  (including `ProviderData`) or drop all of them.
- `ProviderID` is a small breaking change: assigning or comparing a plain
  `string` variable needs a conversion. Untyped constants and literals
  still work.
- `Text` is empty under Anthropic's default `"omitted"` display, and for
  redacted blocks. Only `display: "summarized"` (set by the app, e.g.
  through `anthropic.WithRequestOptions`) fills it.

## References

- Issue #92; related #86 (reasoning effort), #93 (`ProviderParams`), #80
  (compaction).
- `llms/message.go` (`ContentReasoning`, `Reasoning`), `llms/model.go`
  (`ProviderID`), `llms/reasoning.go` (`ReasoningFrom`,
  `WithoutReasoning`), `llms/providers/anthropic/convert.go`.
- Live: `TestIntegrationAnthropicReasoningReplay` and
  `TestIntegrationVertexAnthropicReasoningReplay` (shared
  `livetest.ReasoningReplay`), run with `prefix_mismatch_behavior: "error"`.
- ADR-0010 (`ProviderSignature`, same opaque round-trip pattern), ADR-0020
  (empty responses), ADR-0018 (Anthropic thinking tokens).
- `docs/MAESTRO_DIVERGENCES.md` A8.
