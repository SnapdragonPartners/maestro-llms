# maestro-llms

A small, app-neutral Go toolkit for working with LLM and embedding providers
behind stable interfaces, shared by the Maestro and Morris applications.

The binding design lives in [`docs/specification.md`](docs/specification.md).
Its "Normative Clarifications" and "Resolved by review" sections record
settled decisions — read them before implementing.

## Status

v0.1 shipped (tagged `v0.1.0`): core chat/embedding interfaces, middleware
chaining, limiter + in-memory limiter, deterministic fakes, Anthropic chat,
OpenAI embeddings. v0.2 in progress: OpenAI chat (Responses API), Google
chat, Ollama chat, and a shared provider error classifier. See
[`docs/specification.md`](docs/specification.md) ("Roadmap Update") and
[`docs/MAESTRO_DIVERGENCES.md`](docs/MAESTRO_DIVERGENCES.md).

## Layout

```
llms/                core interfaces and shared types
llms/middleware/     provider-neutral middleware + chain helpers
llms/ratelimit/      Limiter/Reservation interfaces + in-memory limiter
llms/providers/      one package per provider (leaf imports)
llms/testllm/        deterministic fakes for tests
```

## Development

```
make lint     # gofmt + golangci-lint
make test     # unit tests with coverage
make build    # lint + go build ./...
make fix      # auto-fix import grouping
make install-hooks   # install the pre-push lint+test hook
```

Single test: `make test TESTARGS='-run TestName ./llms/...'`.

### Live integration tests

Build-tagged (`//go:build integration`) tests exercise the real provider
APIs. They never run in normal `make test`/CI for unit work, and each skips
unless its credentials/host are present.

- **Canonical path = CI.** The *Integration (live providers)* workflow
  (Actions tab, manual `workflow_dispatch`) runs them on Linux against the
  live APIs plus an Ollama started in the runner. This is the source of
  truth.
- **Locally:** `make test-integration` works on Linux. On **macOS**,
  AMFI/Gatekeeper (often plus endpoint-security agents) blocks freshly built
  *unsigned* test binaries — a plain `go test` wedges in `dyld` before any
  Go code runs. Use `make test-integration-local`, which compiles, ad-hoc
  codesigns, then runs each integration binary. Set the keys/host you want
  exercised, e.g. `OPENAI_API_KEY=… OLLAMA_MODEL=llama3.2:1b make test-integration-local`.

Ollama caveat: point it at a **non-reasoning** model (e.g. `llama3.2:1b`).
Reasoning models (e.g. `qwen3`) emit a separate `thinking` field that this
client does not surface, so `content` is empty under small token budgets and
unbounded otherwise; a `think` control is not exposed in v0.2.

Work lands via pull request; `main` is branch-protected and CI must pass.
