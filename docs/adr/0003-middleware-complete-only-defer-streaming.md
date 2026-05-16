# 0003. Middleware wraps `Complete` only; defer streaming semantics

- **Status:** Accepted
- **Date:** 2026-05-16

## Context

`ChatClient` is the closed core interface (`Complete`, `Model`).
`StreamingChatClient` is a **separate optional capability** discovered by type
assertion (`llms/chat.go`; `docs/specification.md` §Streaming, line 146). This
indirection exists precisely so streaming can land later without breaking
every provider, fake, and middleware.

Facts as of this decision:

- **No provider implements `StreamingChatClient`.** It is a fixed
  forward-declared contract only; `StreamChunk` is a placeholder.
- The existing `ratelimit` middleware (`rlChat`) is a plain `ChatClient`
  wrapper. It already does **not** forward `StreamingChatClient`: a
  type-assertion for streaming fails once a client is wrapped. So this
  property is pre-existing, not introduced by v0.3.
- The v0.3 middleware set (validation, retry, per-attempt timeout, circuit
  breaker, metrics) all wrap clients the same way.
- A wrapper struct that implements only `ChatClient` makes any underlying
  streaming capability **invisible through the chain** — it does not corrupt
  streaming, it hides it.
- Consumer demand: Maestro never needs streaming. Morris may need it
  *post-MVP*, not now.

The real cost of supporting streaming in middleware is **not** the
type-assertion forwarding boilerplate. It is that retry, per-attempt timeout,
and circuit-breaking have **no obvious correct semantics over a stream**:
you cannot transparently retry a stream that has already emitted tokens to
the caller; "per-attempt timeout" vs. "idle-gap timeout between chunks" is a
different design; circuit accounting on a partially-consumed stream is
ambiguous. Building streaming-aware middleware *correctly* means designing
those semantics — for a feature no consumer needs yet.

(Note: a comment in `llms/chat.go` optimistically parenthesizes streaming as
"v0.3". That is superseded here: v0.3 ships the **middleware**; streaming
itself remains deferred.)

## Decision

**Option A.** All v0.3 middleware are plain `ChatClient` (and
`EmbeddingClient`) wrappers, consistent with `ratelimit`. They wrap
`Complete`/`Embed` only. Streaming-aware middleware semantics are **not**
designed or implemented now.

The limitation is made explicit rather than left silent:

- This ADR is the decision of record.
- A short "Limitations" note in middleware package docs states that wrappers
  are `Complete`-only and composing them around a future streaming client
  drops `StreamingChatClient` until streaming semantics are designed.
- `ratelimit` gets the same note retroactively (it shares the property).

We explicitly **reject** Option B (build capability-preserving wrappers now)
because it forces speculative design of streaming retry/timeout/circuit
semantics with no consumer to validate against — a classic premature, hard to
get right, and likely-to-be-redone investment.

## Consequences

- **Today: zero functional impact.** No provider streams; nothing is lost.
  v0.3 stays focused on the `Complete`/`Embed` path both consumers use.
- **When streaming lands** (Morris, post-MVP): composing existing middleware
  around a streaming-capable client will not expose `StreamingChatClient`.
  That is acceptable and now documented, not a surprise.
- **The deferred work is bounded and explicit.** A future ADR + work item
  must, with a real streaming consumer in hand: (a) finalize `StreamChunk`,
  (b) define retry/timeout/circuit semantics over a stream, (c) introduce a
  capability-preserving wrapper pattern, (d) retrofit `ratelimit` and the
  v0.3 middleware. None of that is implied to be free; it is simply not v0.3.
- No core interface changes — `StreamingChatClient` staying optional is what
  makes this deferral non-breaking, exactly as the spec intended.

## References

- `llms/chat.go` — `ChatClient` / `StreamingChatClient` / `StreamChunk`
- `docs/specification.md` — §Streaming (line 146), recommended middleware order
- `llms/middleware/ratelimit.go` — pre-existing `Complete`-only precedent
- ADR-0001 (this log's process)
- Supersedes the "(v0.3)" streaming parenthetical in `llms/chat.go` doc comment
