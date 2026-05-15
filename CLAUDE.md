# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Current state

This repository is **pre-implementation**. It currently contains only `LICENSE` (MIT, Snapdragon Partners) and `docs/specification.md`. There is no Go code, `go.mod`, build, or test tooling yet. The first implementation work is the extraction described in the spec's "Extraction Plan".

`docs/specification.md` is the binding contract for v0. Read it before writing code. The "Normative Clarifications From Engineering Review" and "Resolved by review" sections are decisions, not suggestions — do not relitigate them in code (e.g. tool calls/results are `ContentPart`s not side-channel fields; `System` is a dedicated `ChatRequest` field; `Temperature` is `*float32`; streaming is a separate `StreamingChatClient` interface; local limiter rejections use `LimitError`, not `ProviderError`).

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

None yet — no Go toolchain is set up. Once `go.mod` exists, the standard Go workflow applies (`go build ./...`, `go test ./...`, single test via `go test -run TestName ./pkg/...`). Update this section with the real commands when tooling lands.

## Versioning

Pre-1.0; v0.x minor versions may break. v0.1 scope is fixed: core chat+embeddings interfaces, OpenAI embeddings, Anthropic chat, middleware chaining, limiter interfaces, in-memory limiter, fakes. OpenAI chat is explicitly out of scope for v0.1 unless it falls out of the Maestro extraction for free — do not let it expand v0.1.
