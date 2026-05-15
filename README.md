# maestro-llms

A small, app-neutral Go toolkit for working with LLM and embedding providers
behind stable interfaces, shared by the Maestro and Morris applications.

The binding design lives in [`docs/specification.md`](docs/specification.md).
Its "Normative Clarifications" and "Resolved by review" sections record
settled decisions — read them before implementing.

## Status

Pre-implementation. This commit establishes the module, package layout, and
tooling only. v0.1 scope: core chat and embedding interfaces, OpenAI
embeddings, Anthropic chat, middleware chaining, limiter interfaces, an
in-memory limiter, and deterministic fakes.

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

Work lands via pull request; `main` is branch-protected and CI must pass.
