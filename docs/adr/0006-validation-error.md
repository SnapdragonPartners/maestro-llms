# 0006. Validation middleware emits a distinct, non-retryable `ValidationError`; structural scope only

- **Status:** Accepted
- **Date:** 2026-05-16

## Context

The v0.3 validation middleware (PR 5) is the outermost middleware in the
spec's recommended order. Two questions, parallel to ADR-0005:

1. **What error does it return when a request is malformed?** Reusing
   `*llms.ProviderError{Kind: bad_request}` would be misleading — no provider
   was contacted; the rejection is local, like `*LimitError` and
   `*CircuitOpenError`.
2. **Should that error be retryable?** A structurally invalid request fails
   identically on every attempt; retrying wastes work.

Separately, Maestro's "validation" bundles agent-type empty-response retry
logic (architect vs coder, guidance injection, `pause_turn` resume). That is
application policy, not provider neutrality.

## Decision

Introduce `middleware.ValidationError{Reason}`:

- A **distinct typed error**, neither `*llms.ProviderError` nor
  `*llms.LimitError`, so `llms.Retryable` returns **false** — a malformed
  request is never retried (consistent with ADR-0004/0005's rationale).
- Lives in the `middleware` package (produced only here).

Scope is **structural and app-neutral only** (divergence M3):

- `System` parts must be text-only (spec §System, line 178).
- At least one message; no empty message content.
- Message `Role` must be exactly user/assistant/tool (no system role —
  system is `ChatRequest.System`); unknown roles are rejected here rather
  than reaching a provider adapter.
- Each `ContentPart` must match its discriminant **and be legal for the
  message role**: `tool_call` only on assistant, `tool_result` only on
  tool, text on any role (rejected at this outer layer rather than letting
  a provider adapter serialize a malformed transcript).
- Tool-call ↔ tool-result pairing (spec line 359): reject missing,
  duplicate, or orphaned tool results across the conversation.

Explicitly **out of scope**: empty-response/agent-guidance/`pause_turn`
policy (Maestro keeps it app-side), and any embedding validation —
there is **no `ValidationEmbeddings`**, because the embedding batch limit is
the provider client's responsibility (the app owns chunking, per the spec),
leaving no app-neutral structural rule to enforce.

## Consequences

- Malformed requests fail fast at the outermost layer, never consuming
  retry/timeout/circuit/limiter work; callers match with
  `errors.As(err, *ValidationError)`.
- Behavioral divergence from Maestro (M3): the toolkit rejects only
  structurally invalid requests; Maestro's empty-response/agent handling
  must stay in Maestro.
- The pairing checker is a small explicit state machine (pending/resolved
  ID sets) so "missing/duplicate/orphaned" each produce a specific reason.
- If an app ever needs custom structural rules, that is a new ADR adding a
  predicate option — not a reason to widen scope into app policy here.

## References

- `llms/middleware/validation.go` — `ValidationError`, `pairingState`
- `docs/specification.md` — §System (178), tool-result pairing (359),
  embeddings/chunking (429)
- `docs/MAESTRO_DIVERGENCES.md` — M3
- ADR-0005 (distinct non-retryable middleware error, same shape), ADR-0004,
  ADR-0001
