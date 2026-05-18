# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Current state

**v0.4.0** is the current line. Shipped: core chat/embedding interfaces, errors, fakes, the reservation limiter + in-memory limiter; all four provider chat adapters (Anthropic, OpenAI Responses, Google, Ollama-no-SDK) + OpenAI and Gemini/Vertex embeddings; the full provider-neutral middleware set (`Validation`, `Retry`, `Timeout`, `Circuit`, `RateLimit`, `Metrics`) with `ChainChat`/`ChainEmbeddings` and the `Recommended*` helper; **v0.4 (ADR-0009)**: Anthropic-on-Vertex via the `anthropicvertex` leaf package + base `anthropic.WithRequestOptions`, Gemini/Vertex embeddings (`google.NewEmbeddings`), task-typed embeddings (`EmbeddingTask`/`EmbeddingInput.Title`), app-supplied auth + PSC endpoint/transport injection (no ADC), fail-closed `MaxInputBytes` truncation guard, `gemini-embedding-001` single-input rejection. Standard Go toolchain (`go.mod`; `make build|test|lint`). The Maestro cut-over (extraction-plan step 11) and Morris wiring are the next milestones; streaming and vLLM remain deliberately deferred.

`docs/specification.md` is the binding contract for v0. Read it before writing code. The "Normative Clarifications From Engineering Review" and "Resolved by review" sections are decisions, not suggestions — do not relitigate them in code (e.g. tool calls/results are `ContentPart`s not side-channel fields; `System` is a dedicated `ChatRequest` field; `Temperature` is `*float32`; streaming is a separate `StreamingChatClient` interface; local limiter rejections use `LimitError`, not `ProviderError`).

`docs/adr/` is the **Architecture Decision Record** log (see `docs/adr/README.md`): the rationale of record for significant structural choices, with the spec as the contract and `docs/MAESTRO_DIVERGENCES.md` as the cut-over checklist. Read relevant ADRs before changing a documented decision; do not "fix" a deliberate limitation an ADR explains. Significant structural changes should land an ADR in the same PR. Notably ADR-0003: middleware wraps `Complete`/`Embed` only — streaming-aware middleware semantics are deliberately deferred until a real consumer exists, so do not add streaming forwarding to middleware without a superseding ADR.

## What this package is

`maestro-llms` is an app-neutral Go toolkit wrapping LLM/embedding providers behind stable interfaces, shared by two consumers with deliberately different needs:

- **Maestro** — desktop/local, in-process rate limiting.
- **Morris** — cloud (Cloud Run, multi-instance), needs shared/distributed rate limiting, audit, content classification.

The core design tension to keep in mind on every change: **nothing in this package may import product-specific assumptions from either app.** Config resolution, secret sourcing, audit taxonomy, distributed limiter storage, and content rules all belong to the applications, not here. When a feature seems to need app context, the answer is almost always an interface the app implements or a callback hook — not a concrete implementation in this package.

## Architecture (planned, per spec)

Planned module `github.com/SnapdragonPartners/maestro-llms`, core import name `llms`.

```
llms/                core interfaces and shared types (ChatClient, EmbeddingClient, Message, errors)
llms/middleware/     provider-neutral middleware (timeout, retry, circuit, ratelimit, metrics, validation)
llms/ratelimit/      Limiter/Reservation interfaces + optional in-memory limiter
llms/providers/{anthropic,openai,google,ollama}/   one package per provider, imported only if used
llms/testllm/        deterministic FakeChatClient / FakeEmbeddingClient
```

Load-bearing structural decisions:

- **Provider packages are leaf imports.** The core `llms` package must not pull provider SDKs. Apps import only the provider packages they use.
- **One app-neutral conversation model.** `Message`/`ContentPart` is the single representation; each provider adapter translates it to/from that provider's wire shape at the provider boundary (e.g. Anthropic encodes `RoleTool` results as user-role content blocks; OpenAI uses tool-role messages). The Maestro implementation is the tested reference for this mapping across all four providers — adapt it, don't reinvent provider behavior.
- **Middleware is `func(Client) Client`, composed via `ChainChat`/`ChainEmbeddings`; first argument is outermost.** Composition order is semantically significant (see the spec's recommended order and the retry-vs-reservation tradeoff). Changing order changes correctness, not just performance.
- **Rate limiting is a reservation protocol** (`Reserve` → `Commit` actuals → always `Release`), not a concrete limiter. The in-memory limiter ships here; the distributed (Postgres) one is implemented in Morris first and only promoted here if it stays app-neutral. `Release` must run on a context that survives request cancellation (`context.WithoutCancel`).
- **Capability growth via optional interfaces + type assertion**, never by widening core interfaces. This applies to `StreamingChatClient` and `LimiterStats`. Adding a method to `ChatClient` or `Limiter` is a breaking change for every provider, fake, and middleware — treat it as such.
- **Deterministic fakes are first-class**, not an afterthought: both consumers run most tests without real provider calls, so `testllm` correctness gates everything downstream.

## Build/test commands

`make build` (lint + `go build ./...`), `make test` (unit tests w/ coverage; single: `make test TESTARGS='-run TestName ./llms/...'`), `make lint` (gofmt + golangci-lint — the lint gate is strict: `fieldalignment`, `gocritic rangeValCopy`, `revive` unused-params, modernize `min`/`max`/`WaitGroup.Go` all fail CI). Live provider tests: `make test-integration` (OS-aware; see README). CI requires `build-lint-test` + `CodeQL` (aggregate only — see memory).

## Versioning

Pre-1.0; v0.x minor versions may break. Shipped lines: v0.1.0 (core + Anthropic chat + OpenAI embeddings), v0.2.0 (OpenAI/Google/Ollama chat + error classifier), v0.3.0 (full middleware set + `Recommended*`), v0.4.0 (Vertex backend + Gemini embeddings + task-typed embeddings, ADR-0009). Each PR that intentionally differs from Maestro appends a `docs/MAESTRO_DIVERGENCES.md` row; significant structural decisions land an ADR (`docs/adr/`) in the same PR. Don't expand a release's scope mid-stream — scope is decided up front and tracked in the session plan.
