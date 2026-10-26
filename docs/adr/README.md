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
| [0004](0004-retry-circuit-reuse-llms-retryable.md) | Retry/circuit middleware reuse `llms.Retryable`, not a ported classifier | Accepted; amended by 0033 |
| [0005](0005-circuit-open-error.md) | Circuit breaker emits a distinct, non-retryable `CircuitOpenError` | Accepted; amended by 0033 |
| [0006](0006-validation-error.md) | Validation middleware: distinct non-retryable `ValidationError`, structural scope only | Accepted |
| [0007](0007-toolchoice-required.md) | `ToolChoiceRequired` — force a tool call without naming one | Accepted |
| [0008](0008-prompt-cache-hint.md) | Provider-neutral prompt-cache hint (`ContentPart.CacheBreakpoint`) | Accepted |
| [0009](0009-vertex-backend-psc-and-gemini-embeddings.md) | Vertex backend (Anthropic + Gemini embeddings), PSC injection, task-typed embeddings — design of record; v0.4 implementation | Accepted; amended by 0027, 0028, 0029 |
| [0010](0010-toolcall-provider-signature.md) | Round-trip opaque provider state via `ToolCall.ProviderSignature` (Gemini 3 thought_signature) | Accepted |
| [0011](0011-toolloop-helper.md) | Tool loop helper — scope, API, and non-goals | Accepted; partially superseded by 0017; amended by 0019, 0020, 0026, 0030 |
| [0012](0012-model-lister.md) | Model listing & per-family upgrade detection (`llms.ModelLister`, per-provider `LatestInFamily`) | Accepted |
| [0013](0013-text-token-estimator.md) | Text-level token estimator (`EstimateTextTokens`) — neutral-biased free function distinct from the high-biased middleware estimator | Accepted |
| [0015](0015-vllm-provider.md) | vLLM provider — leaf package, OpenAI Chat Completions wire shape (no auth allowed, no `LatestInFamily`, `Created` is local load time) | Accepted; amended by 0027 |
| [0016](0016-usage-reasoning-tokens.md) | `Usage.ReasoningTokens` — surface reasoning-model thinking budget alongside visible output | Accepted; partially superseded by 0018 |
| [0017](0017-toolloop-terminal-tools.md) | Tool loop terminal tools (`ToolResult.Stop`, `OutcomeTerminalTool`) and executed limit turn | Accepted; amended by 0019, 0032 |
| [0018](0018-anthropic-thinking-tokens.md) | Anthropic `ReasoningTokens` from `output_tokens_details.thinking_tokens` (retires the ADR-0016 exception) | Accepted |
| [0019](0019-toolloop-deadline-and-no-iteration-limit.md) | Tool loop `OutcomeDeadline` and `NoIterationLimit` (the loop's context decides deadline vs failure; negative `MaxIterations` is unlimited) | Accepted; amended by 0032 |
| [0020](0020-toolloop-empty-response.md) | Tool loop `OutcomeEmptyResponse` (an empty response is reported, not appended) | Accepted; amended by 0032 |
| [0021](0021-reasoning-content-part.md) | Round-trip provider reasoning as a `ContentReasoning` part (Anthropic thinking blocks); typed `ProviderID` | Accepted; amended by 0030 |
| [0022](0022-reasoning-effort.md) | Provider-neutral reasoning control (`ChatRequest.Reasoning`, `ChatResponse.AppliedReasoning`): 1:1 mapping or omitted, no per-model prediction | Accepted; amended by 0032 |
| [0023](0023-usage-input-includes-cache.md) | `Usage.InputTokens` includes cached input on every provider; cache reads/writes are subsets | Accepted |
| [0024](0024-provider-params.md) | Per-request `ProviderParams`: raw provider body fields keyed by `ProviderID`, deep-merged after mapping | Accepted |
| [0025](0025-bucket-distributed-limiter.md) | Generic `bucket` limiter with a Postgres backend (lease rows + renewal for slots; FIFO queue planned, not MVP) | Accepted; amended by 0033 |
| [0026](0026-context-length-error-kind.md) | Context-window overflow: `ErrorKindContextLength` (deterministic provider signals only) and toolloop `OutcomeContextExceeded` | Accepted; amended by 0030, 0032 |
| [0027](0027-pre-v1-api-cleanup.md) | Pre-v1 API cleanup: `openaicompat` package (`vllm` a permanent alias), `*auth.Credentials` for both Vertex entry points, one `google.Option` for chat and embeddings | Accepted; amended by 0028 |
| [0028](0028-gemini-chat-on-vertex.md) | Gemini chat on Vertex AI: `google.New` accepts `WithVertex`/`WithCredentials`, one backend selector for chat and embeddings, Vertex model-listing prefix | Accepted; amended by 0029 |
| [0029](0029-vertex-injected-client-auth.md) | Vertex: `WithCredentials` authenticates an injected `WithHTTPClient` (network-only on every provider; caller's client never mutated) | Accepted |
| [0030](0030-toolloop-compaction.md) | Tool loop compaction: one `Compact` hook (before each turn, and once on overflow), `OutcomeCompactionError`, separate `CompactionUsage`, `KeepLast`/`Summarize` in the `compact` leaf | Accepted; amended by 0032 |
| [0031](0031-attempt-context-observer.md) | Per-attempt correlation: optional `ContextObserver` (each `Event` with its context) and the retry attempt number via `AttemptFromContext`; no chain-order change | Accepted |
| [0032](0032-enum-like-types-v1.md) | Enum-like types at v1: the grow list (switches need a `default` case, no silent reclassification), a new `OutcomeKind` still needs an ADR, `CompactReason` fixed for v1 | Accepted |
| [0033](0033-limit-error-classification.md) | Limiter rejections: `LimitError` is neutral to the circuit; `Permanent` and canceled rejections are not retryable (a deadline still is) | Accepted |
