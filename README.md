# maestro-llms

A small, app-neutral Go toolkit for working with LLM and embedding providers
behind stable interfaces, shared by the Maestro and Morris applications.

The binding design lives in [`docs/specification.md`](docs/specification.md).
Its "Normative Clarifications" and "Resolved by review" sections record
settled decisions — read them before implementing.

## Status

- **v0.1.0** — core chat/embedding interfaces, middleware chaining, limiter
  + in-memory limiter, deterministic fakes, Anthropic chat, OpenAI embeddings.
- **v0.2.0** — OpenAI chat (Responses API), Google chat, Ollama chat (no
  SDK), shared provider error classifier; full live integration suite.
- **v0.3.0** — the remaining provider-neutral middleware: validation, retry,
  per-attempt timeout, circuit breaker, metrics/observer, plus the
  `Recommended*` chain helper (see [Middleware](#middleware)).

Design rationale for non-obvious decisions lives in
[`docs/adr/`](docs/adr/). See also [`docs/specification.md`](docs/specification.md)
and [`docs/MAESTRO_DIVERGENCES.md`](docs/MAESTRO_DIVERGENCES.md).

## Layout

```
llms/                core interfaces and shared types
llms/middleware/     provider-neutral middleware + chain helpers
llms/ratelimit/      Limiter/Reservation interfaces + in-memory limiter
llms/providers/      one package per provider (leaf imports)
llms/testllm/        deterministic fakes for tests
```

## Middleware

Middleware is `func(Client) Client`, composed with `ChainChat` /
`ChainEmbeddings`. **The first argument is the outermost wrapper**, and
composition order is semantically significant — it changes correctness, not
just performance.

Provider-neutral middleware (`llms/middleware/`):

| Middleware | Purpose |
|---|---|
| `ValidationChat` | Reject structurally invalid requests (text-only `System`, tool-call↔result pairing, roles). Chat only. |
| `RetryChat` / `RetryEmbeddings` | Retry while `llms.Retryable`, honoring `RetryAfter`; exponential backoff + optional jitter. |
| `TimeoutChat` / `TimeoutEmbeddings` | Per-attempt `context` deadline. |
| `CircuitChat` / `CircuitEmbeddings` | Three-state breaker; fast-fails with a non-retryable `*CircuitOpenError`; single-flight HalfOpen probe. |
| `RateLimitChat` / `RateLimitEmbeddings` | Reservation protocol against a `ratelimit.Limiter`. |
| `MetricsChat` / `MetricsEmbeddings` | One app-neutral `Event` per call to a narrow `Observer` (success and failure). |

### Recommended order

`RecommendedChat` / `RecommendedEmbeddings` compose the spec's recommended
order:

```
validation -> retry -> per-attempt timeout -> circuit -> rate limit -> metrics -> provider
```

Each retry attempt independently flows through timeout, circuit, and the
rate-limit reservation (retries are real provider traffic and are gated like
first attempts); a malformed request is rejected before any of that work.
This is an opinionated convenience — `ChainChat` remains the primitive for
custom orders/subsets. Tradeoffs of changing the order (retry vs. reservation,
total vs. per-attempt timeout) are documented in `docs/specification.md`
("Recommended order").

Retry/circuit classify failures **only** via `llms.Retryable` (one error
model, no second classifier — [ADR-0004](docs/adr/0004-retry-circuit-reuse-llms-retryable.md));
`*CircuitOpenError` and `*ValidationError` are deliberately non-retryable
([ADR-0005](docs/adr/0005-circuit-open-error.md),
[ADR-0006](docs/adr/0006-validation-error.md)). All wrappers are
`Complete`/`Embed`-only — streaming is deferred
([ADR-0003](docs/adr/0003-middleware-complete-only-defer-streaming.md)).

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
- **Locally:** `make test-integration` is the one correct command on every
  OS — it is OS-aware. On **macOS**, AMFI/Gatekeeper (often plus
  endpoint-security agents) blocks freshly built *unsigned* test binaries (a
  plain `go test` wedges in `dyld` before any Go code runs), so the target
  automatically routes through a compile + ad-hoc codesign step; on Linux it
  runs `go test` directly. (`make test-integration-local` forces the
  codesign path explicitly and is safe on any OS — rarely needed.)
- **Anthropic key:** the Anthropic test reads `ANTHROPIC_API_KEY` first
  (the CI secret), then falls back to `MAESTRO_ANTHROPIC_API_KEY`. Use the
  prefixed var locally to keep `ANTHROPIC_API_KEY` unset so Claude Code's
  OAuth subscription auth keeps working in the same shell.

#### Running them

Each provider runs **simple chat** and a **tool-use round trip**; OpenAI also
runs **embeddings**. A provider whose key/host is absent is skipped (not
failed), so you can exercise any subset by setting only those vars.

| Provider | Key / host (skips if unset) | Model override (default) |
|---|---|---|
| Anthropic | `ANTHROPIC_API_KEY`, else `MAESTRO_ANTHROPIC_API_KEY` | `ANTHROPIC_MODEL` (`claude-haiku-4-5-20251001`) |
| OpenAI | `OPENAI_API_KEY` | `OPENAI_CHAT_MODEL` (`gpt-4o-mini`), `OPENAI_EMBED_MODEL` (`text-embedding-3-small`) |
| Google | `GEMINI_API_KEY`, else `GOOGLE_GENAI_API_KEY` / `GOOGLE_API_KEY` | `GOOGLE_MODEL` (`gemini-2.5-flash`) |
| Ollama | local daemon at `OLLAMA_HOST` (`http://localhost:11434`) | `OLLAMA_MODEL` — the Makefile defaults this to `llama3.2:1b`; raw `go test` falls back to `ministral-3:14b-instruct-2512-fp16` |

Full run, all four providers (same command on macOS and Linux):

```sh
MAESTRO_ANTHROPIC_API_KEY=sk-ant-… \
OPENAI_API_KEY=sk-… \
GEMINI_API_KEY=… \
make test-integration
```

The Makefile defaults `OLLAMA_MODEL=llama3.2:1b` (a small non-reasoning model)
unless you set it. To exercise just one provider, set only its key/host and
leave the rest unset — the others skip, e.g.
`OPENAI_API_KEY=… make test-integration`.

Ollama caveat: point it at a **non-reasoning** model (e.g. `llama3.2:1b`).
Reasoning models (e.g. `qwen3`) emit a separate `thinking` field that this
client does not surface, so `content` is empty under small token budgets and
unbounded otherwise; a `think` control is not exposed in v0.2.

Work lands via pull request; `main` is branch-protected and CI must pass.
