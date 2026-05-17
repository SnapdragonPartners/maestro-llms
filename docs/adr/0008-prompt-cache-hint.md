# 0008. Provider-neutral prompt-cache hint (`ContentPart.CacheBreakpoint`)

- **Status:** Accepted
- **Date:** 2026-05-17

## Context

Maestro's coder marks the initial prompt and the last cacheable context
message with a `CacheControl`, which its Anthropic client maps to Anthropic
prompt caching. The extracted message model had no equivalent and the
Anthropic adapter emitted plain text blocks, so cutting Maestro over as-is
**silently dropped** prompt caching — a real cost/latency regression. This
was recorded (divergence A5, external-review P1) as an interim, visible gap.

The constraint: a Maestro-shaped `CacheControl` (TTL, policy knobs) must not
enter the app-neutral model — that is exactly the product-specific coupling
the spec forbids. But "cache from here back" is a general capability several
providers expose, so a *neutral* hint is justified (agreed with the external
reviewer).

Provider reality is asymmetric:

- **Anthropic** — explicit inline `cache_control` breakpoints on blocks.
  Direct, faithful mapping.
- **OpenAI** — automatic prefix caching, no caller control. A hint is a
  harmless no-op.
- **Gemini** — explicit caching exists but as a *separate* cached-content
  resource API (create a handle, reference it), **not** inline breakpoints.
  An inline hint cannot drive it.
- **Ollama** — no prompt caching.

## Decision

Add `ContentPart.CacheBreakpoint bool` — a minimal, advisory hint: everything
up to and including this part *may* be prompt-cached. **No TTL, no policy,
no per-provider knobs.** It never changes model output, only cache economics.

- **Anthropic** honors it: a marked text content block gets
  `cache_control: ephemeral`; a breakpoint on any system part marks the
  (flattened, single) system block — covering Maestro's "cache the system
  prompt" and "cache the last context message" cases.
- **OpenAI / Gemini / Ollama** ignore it (documented on the field). Gemini's
  separate cached-content API is explicitly **out of scope** here.

Scope is text content + the system prompt (Maestro's actual usage). Marking
non-text parts (tool_use/tool_result) is currently a no-op.

**Anthropic caps cache_control markers at 4 per request.** Because the hint
is advisory ("never changes behavior"), exceeding the cap must NOT turn a
valid request into a provider 400 — and rejecting locally would equally
violate "advisory." So the adapter caps deterministically: the system block
(if marked) takes one; the remaining budget goes to the **last** content
breakpoints (longest cached prefixes); earlier excess is silently emitted as
plain text. Over-marking degrades cache economics, never correctness.

## Consequences

- Maestro prompt caching is preserved at cut-over via a neutral hint, not a
  ported `CacheControl`. Divergence A5 is **resolved**.
- The neutrality cost is honestly asymmetric: only Anthropic actually caches
  from the hint today. This is documented on the field and here, not hidden.
- **Reopen conditions** (each a future ADR, not this one): honoring
  breakpoints on tool_result blocks (large tool outputs are a common cache
  target); a separate Gemini explicit-caching integration (different API
  shape — cannot be retrofitted onto this inline hint); exposing cache-token
  accounting alignment (`Usage` already carries `CacheRead/WriteTokens`).
- No behavior change for callers that don't set it (zero value = off).

## References

- `llms/message.go` — `ContentPart.CacheBreakpoint`
- `llms/providers/anthropic/convert.go` — system + content block mapping
- `docs/MAESTRO_DIVERGENCES.md` — A5 (resolved)
- `docs/specification.md` — ContentPart
- ADR-0003 (Usage already anticipated cache tokens), ADR-0001 (process)
