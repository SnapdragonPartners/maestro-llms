# Architecture Decision Records

This directory holds the **Architecture Decision Records (ADRs)** for
`maestro-llms`: short, immutable documents that capture a single significant
architectural decision, its context, and its consequences.

## Why ADRs here

`docs/specification.md` is the binding *contract* (what the package must do).
`docs/MAESTRO_DIVERGENCES.md` is a per-row *checklist* of where this package
intentionally differs from Maestro, for cut-over acceptance. Neither captures
**why a structural choice was made, what was rejected, and what it costs us
later**. ADRs fill that gap so a future contributor (or a future us) does not
relitigate a settled decision or trip over a deliberate limitation.

## Format

Lightweight Nygard-style. One decision per file, named
`NNNN-kebab-title.md`, numbered in creation order (zero-padded, four digits).
Each ADR has: **Status**, **Date**, **Context**, **Decision**,
**Consequences**, and **References**.

- ADRs are **append-only**. Do not rewrite history: to change a decision,
  add a new ADR and set the old one's status to `Superseded by ADR-NNNN`.
- Status values: `Proposed`, `Accepted`, `Superseded by ADR-NNNN`,
  `Deprecated`.
- Keep them terse and binding, matching the house style of
  `specification.md`. An ADR is a decision, not an essay.
- Retrofitting ADRs for decisions already made is fine and encouraged;
  date them to when the decision was actually taken.

Copy `0001`'s structure as the template.

## Index

| ADR | Title | Status |
|---|---|---|
| [0001](0001-record-architecture-decisions.md) | Record architecture decisions | Accepted |
| [0002](0002-ollama-no-sdk-hand-rolled-client.md) | Ollama provider: no SDK, hand-rolled `/api/chat` client | Accepted |
| [0003](0003-middleware-complete-only-defer-streaming.md) | Middleware wraps `Complete` only; defer streaming semantics | Accepted |
| [0004](0004-retry-circuit-reuse-llms-retryable.md) | Retry/circuit middleware reuse `llms.Retryable`, not a ported classifier | Accepted |
| [0005](0005-circuit-open-error.md) | Circuit breaker emits a distinct, non-retryable `CircuitOpenError` | Accepted |
| [0006](0006-validation-error.md) | Validation middleware: distinct non-retryable `ValidationError`, structural scope only | Accepted |
| [0007](0007-toolchoice-required.md) | `ToolChoiceRequired` — force a tool call without naming one | Accepted |
| [0008](0008-prompt-cache-hint.md) | Provider-neutral prompt-cache hint (`ContentPart.CacheBreakpoint`) | Accepted |
| [0009](0009-vertex-backend-psc-and-gemini-embeddings.md) | Vertex backend (Anthropic + Gemini embeddings), PSC injection, task-typed embeddings — design of record; v0.4 implementation | Accepted |
| [0010](0010-toolcall-provider-signature.md) | Round-trip opaque provider state via `ToolCall.ProviderSignature` (Gemini 3 thought_signature) | Accepted |
