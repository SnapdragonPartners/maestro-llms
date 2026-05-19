# 0010. Round-trip opaque provider state via `ToolCall.ProviderSignature`

- **Status:** Accepted
- **Date:** 2026-05-19

## Context

Pre-extraction Maestro's Gemini client kept a per-client `responseCache` to
replay Gemini "thought signatures" across turns. The extraction **dropped**
that cache (divergence G1) because a mutable, turn-indexed per-client cache
violates the stateless + concurrency-safe contract every client in this
package must honor. G1 recorded the open question explicitly: *"Revisit if a
stateless encoding is needed."*

It is now needed, and as a hard failure, not the "quality may differ" G1
predicted. **Gemini 3 Pro (`gemini-3-pro-preview`)** rejects any multi-turn
tool loop with `400 INVALID_ARGUMENT — "Function call is missing a
thought_signature in functionCall parts"` once the assistant turn is sent
back without the signature. genai exposes the blob as
`genai.Part.ThoughtSignature []byte`, but the app-neutral model had nowhere
to carry it: `ToolCall` was `{ID, Name, Parameters}` and `ChatResponse.Raw`
is response-only, not round-trippable through a `ChatRequest`. So a consumer
**cannot** fix this — it is structurally a toolkit concern. Reported by the
Maestro team during cut-over (their PR #220 / migration §5 G1); Gemini is
otherwise unusable for agentic tool loops via the toolkit.

## Decision

Add `ToolCall.ProviderSignature []byte` — an **opaque, provider-owned** blob
the core never interprets. It restores Maestro's behavior the *right* way:
the signature travels in the conversation history the application already
round-trips, so **no per-client cache** is reintroduced and the
stateless/concurrency-safe contract is preserved.

- Google adapter: capture `part.ThoughtSignature` into `ProviderSignature` on
  response parse; set `genai.Part.ThoughtSignature` from it on request build.
- Other adapters leave it nil and ignore it — same neutral, honored-where-
  supported pattern as `ContentPart.CacheBreakpoint` (ADR-0008).

**Why a typed field on `ToolCall`, not a general opaque-metadata bag on
`ContentPart`:** the signature is per-tool-call state; a specific field is
precise, hard to misuse, and mirrors where genai itself attaches it. A
generic metadata map would invite smuggling app context through the neutral
model — exactly what `Metadata`/`Purpose` boundaries exist to prevent.

## Consequences

- Gemini 3 Pro multi-turn tool loops work; parity with pre-extraction Maestro
  restored without the stateless-contract violation that motivated dropping
  the cache. G1 upgraded from "quality may differ" to **resolved**.
- Additive, zero-value-safe core change (`ToolCall` gains a field); existing
  callers and the other three adapters are unaffected (nil → omitted).
- The blob is opaque and provider-version-specific; the core makes no
  guarantees about its content or longevity — it is only ever passed back to
  the same provider unchanged.
- If another provider later needs analogous round-trip state, it reuses this
  field (it is provider-neutral by name and contract); no new surface.

## References

- `llms/tool.go` — `ToolCall.ProviderSignature`
- `llms/providers/google/convert.go` — capture/replay
- `docs/MAESTRO_DIVERGENCES.md` — G1 (resolved)
- ADR-0008 (CacheBreakpoint — same neutral/opaque/honored-where-supported
  pattern), ADR-0001 (process)
- `google.golang.org/genai` `Part.ThoughtSignature`; Maestro PR #220 /
  migration §5 G1
