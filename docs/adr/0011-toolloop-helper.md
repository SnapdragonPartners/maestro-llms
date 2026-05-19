# 0011. Tool loop helper — scope, API, and non-goals

- **Status:** Accepted
- **Date:** 2026-05-19

## Context

`maestro-llms` ships provider-neutral chat/embedding clients and middleware,
but every multi-turn tool-using application has to write the same loop by
hand: build a request, call `Complete`, append the assistant response,
execute each `ToolCall`, append matching `ToolResultMessage`s, repeat until
no tool calls. That code has structural correctness requirements — every
assistant tool call must receive a matching tool result before the next
turn (cf. validation middleware, ADR-0006); assistant messages must
round-trip verbatim to preserve `ToolCall.ProviderSignature` (ADR-0010) —
which are easy to get subtly wrong. Morris specifically wants a reusable
helper so its chat service can start as a single `Complete` call and evolve
into a tool loop without rewriting the handler, UI contract, or persistence
boundary.

The standing risk for any such helper is drift: a "toolloop" package
acquires agent state, audit taxonomies, persistence, or workflow signals
over time and re-couples the toolkit to consumer assumptions the extraction
exists to remove. Maestro already has its own agent runtime (terminal
tools, story IDs, escalation, persistence); the helper must not absorb it
or grow toward it.

## Decision

Add `llms/toolloop/` as a leaf package depending only on the core `llms`
API and the Go standard library. The full API and execution semantics are
in `docs/toolloop-proposal.md`, accepted by this ADR. The binding scope
guarantees follow.

**In scope (v1):**

- A synchronous `Run(ctx, Config) Outcome` over `llms.ChatClient`.
- App-supplied `Tool{Definition, Execute}`; the loop never executes side
  effects itself — `Execute` is the only place tools run.
- Verbatim round-trip of assistant tool-call messages, preserving
  `ToolCall.ProviderSignature` (ADR-0010).
- Typed `Outcome{Kind, Response, Messages, TotalUsage, Iterations, Err}`
  with five kinds: `FinalAnswer`, `MaxIterations`, `LLMError`, `ToolError`,
  `Canceled`. Cancellation is detected via `errors.Is(err,
  context.Canceled)`, matching the toolkit's contract that caller cancel
  is **not** converted into a `*llms.ProviderError` (cross-cutting X5).
- Fail-closed configuration: `Request.Tools` and `Config.Tools` both set is
  a config error; `Request.ToolChoice` and `Config.ToolChoice` both
  non-zero is a config error; duplicate tool names or nil `Execute`
  functions are config errors; `ToolChoice.RequiresTools()` with no tools
  is a config error (ADR-0007).
- Provider-neutral observation hooks (`OnIteration`, `OnToolCall`) carrying
  only neutral facts.
- Pre-execute `MaxIterations` stopping: the limit-hitting assistant
  message is appended and returned in `Outcome.Messages` as diagnostic
  state; its tool calls are **not** executed and the transcript is **not**
  directly re-feedable into `Complete` without repair.
- Distinction between model-visible tool failure (`ToolResult{IsError:
  true}` → loop continues) and loop-visible tool failure (`Execute` returns
  `error` → `OutcomeToolError`, loop stops).
- Unknown-tool recovery is the default: an unregistered tool name appends
  an error `ToolResult` and continues, so the model can self-correct when
  the provider can't enforce `tool_choice`.

**Out of scope (binding):** every entry in the proposal's Non-Goals list,
notably — no Maestro agent concepts; no terminal/state-transition tools;
no `ProcessEffect`, story IDs, request IDs, or workflow signals; no
built-in persistence, audit taxonomy, or database schema; no built-in
authorization, tenant isolation, or secret resolution; no built-in
filesystem/shell/browser/retrieval/MCP tool adapters; no typed argument
binding, schema-generation helper, or tool registry framework; no human
escalation policy; no built-in moderation or "police output" hook; no
automatic streaming; no prompt management or context compaction.

**Streaming is deferred per ADR-0003.** A streaming-aware tool loop needs
streaming-aware retry/timeout/circuit/transcript semantics first and is a
separate ADR; do not extend this helper to streaming without one.

**Why a leaf package, not core.** Callers who don't need the helper
inherit zero extra API surface; the toolkit's `ChatClient`/`EmbeddingClient`
contracts stay stable; growing `Config` or `Outcome` is not a breaking
change for clients that never import `toolloop`.

**Why events carry only neutral facts.** This is the load-bearing
guardrail. `IterationEvent` and `ToolCallEvent` must never grow story IDs,
tenant IDs, agent state, request IDs, or audit labels — applications
correlate by closing over their own state in the callback. This is what
keeps the helper from drifting into Maestro/Morris-shaped runtime over
time, and is what makes the helper safe for both consumers despite their
deliberately different needs.

## Consequences

- Morris (and similar consumers) can adopt tool calling by swapping
  `client.Complete(ctx, req)` for `toolloop.Run(ctx, Config{Client: client,
  Request: req, Tools: tools})` — the surrounding chat-service boundary
  (build request → orchestrate → apply app policy → persist/return) stays
  stable.
- Maestro's existing agent runtime is **explicitly not absorbed**. Maestro
  may use the helper *inside* its agent layer if that ever becomes useful,
  but agent semantics (terminal tools, suspend/resume, escalation, story
  persistence) stay in Maestro.
- `MaxIterations` semantics, `Outcome.Err` per-kind mapping, the
  pre-execute stop behavior, and the diagnostic-transcript pairing
  invariant are pinned in the proposal and become regression-test targets
  for the implementation.
- Parallel tool execution, typed argument binding, unknown-tool fail-fast,
  and a truncation predicate are deliberately deferred to future ADRs; the
  v1 surface stays small and testable with `llms/testllm`.
- The Non-Goals list is **binding**, not aspirational. PRs that try to
  grow `IterationEvent`/`ToolCallEvent` with app-specific fields, or to
  absorb persistence/audit/agent concepts, require a superseding ADR.
- Additive change: no existing `llms` core type changes; no
  `MAESTRO_DIVERGENCES.md` row is required (the helper is new surface, not
  a behavior change relative to pre-extraction Maestro).

## References

- `docs/toolloop-proposal.md` — full design (accepted by this ADR).
- ADR-0003 — middleware wraps `Complete` only; streaming-aware semantics
  deferred (binds the streaming deferral here too).
- ADR-0006 — validation middleware structural rules (tool-call ↔ result
  pairing the loop must preserve).
- ADR-0007 — `ToolChoiceRequired` / `RequiresTools()` (cf. fail-closed
  config rule).
- ADR-0010 — `ToolCall.ProviderSignature` (the assistant-message
  round-trip the loop preserves verbatim).
- `docs/specification.md` — provider-neutral contracts the helper builds
  on.
