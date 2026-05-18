# maestro-llms

A small, app-neutral Go toolkit for talking to LLM and embedding providers
behind one stable interface.

`maestro-llms` gives you a single `ChatClient` / `EmbeddingClient` contract,
one conversation model, and one error model that work the same across
Anthropic, OpenAI, Google (Gemini), and Ollama — plus composable middleware
(retry, timeout, circuit breaker, rate limiting, metrics, validation) and
deterministic fakes for testing. It is intentionally **light**: a thin
adapter layer, not a framework. It carries no application policy, no agent
logic, no pricing tables, and the minimum of third-party dependencies.

It is shared by two consumers with deliberately different needs — Maestro
(desktop/local) and Morris (cloud, multi-instance) — so nothing in the
package may import product-specific assumptions from either. When a feature
needs app context, the answer is an interface the app implements, not a
concrete implementation here.

The binding design is [`docs/specification.md`](docs/specification.md).
Rationale for non-obvious decisions lives in [`docs/adr/`](docs/adr/).
Intentional differences from the original Maestro implementation are tracked
in [`docs/MAESTRO_DIVERGENCES.md`](docs/MAESTRO_DIVERGENCES.md).

## Features

- **One conversation model.** `Message` / `ContentPart` is the single
  representation; each provider adapter translates to/from that provider's
  wire shape at the boundary. Tool calls and results are content parts, not
  side-channel fields, so a conversation round-trips unambiguously.
- **Four chat providers**: Anthropic (Messages), OpenAI (Responses API),
  Google (Gemini via genai), Ollama (hand-rolled `/api/chat`, no SDK).
- **Embeddings**: OpenAI and Gemini/Vertex (`gemini-embedding-001`),
  order/ID-preserving, per-request dimension override, task-typed
  (`EmbeddingTask`/`Title`, advisory).
- **Vertex AI backend**: Claude via `anthropicvertex` (separate leaf
  package — base `anthropic` stays Google-dep-free) and Gemini embeddings;
  app-supplied auth + PSC endpoint/transport injection, no ADC discovery.
- **One typed error model.** `*llms.ProviderError` (kind, HTTP status,
  `Retry-After`) and `*llms.LimitError`, both `errors.As`-able, with
  `llms.Retryable` / `llms.RetryAfter` helpers.
- **Prompt-cache hint.** `ContentPart.CacheBreakpoint` — an optional,
  advisory marker (no TTL/policy). Anthropic maps it to `cache_control`;
  OpenAI auto-caches (no-op); Gemini/Ollama ignore it. Output never changes,
  only cache economics.
- **Composable middleware**: validation, retry, per-attempt timeout, circuit
  breaker, rate-limit reservation, metrics/observer — plus `Recommended*`
  helpers that wire the spec's recommended order.
- **Deterministic fakes** (`llms/testllm`) so downstream code tests without
  network.
- **Provider packages are leaf imports.** The core `llms` package pulls no
  provider SDKs; you import only the providers you use.

### Not in scope (by design)

- **Streaming.** `StreamingChatClient` is a fixed forward-declared interface
  but is **not implemented**, and no middleware forwards it
  ([ADR-0003](docs/adr/0003-middleware-complete-only-defer-streaming.md)). All
  clients/middleware are `Complete`/`Embed`-only. Streaming is deferred until
  a real consumer needs it.
- **OpenAI chat is the Responses API**, not Chat Completions.
- **No automatic embedding chunking / batching.** The caller owns chunking
  (it also owns retry policy, progress, source IDs). Providers return a typed
  error when an input batch exceeds the model limit.
- **No cost/pricing, no story/request attribution, no agent policy.** The
  metrics `Observer` emits provider-neutral facts only; pricing and
  attribution belong to the application.
- **Pre-1.0.** v0.x minor versions may break.

## Install

```sh
go get github.com/SnapdragonPartners/maestro-llms@latest
```

Requires Go 1.26+. Core import path: `github.com/SnapdragonPartners/maestro-llms/llms`.

## Usage

### Chat

```go
import (
	"context"
	"fmt"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/anthropic"
)

func run(ctx context.Context, apiKey string) error {
	client, err := anthropic.New(
		anthropic.WithAPIKey(apiKey),
		anthropic.WithModel("claude-haiku-4-5-20251001"),
	)
	if err != nil {
		return err
	}

	temp := float32(0)
	resp, err := client.Complete(ctx, llms.ChatRequest{
		Purpose:     llms.PurposeChat,
		System:      []llms.ContentPart{llms.Text("Answer in one sentence.")},
		Messages:    []llms.Message{llms.UserText("What is a goroutine?")},
		MaxTokens:   256,
		Temperature: &temp, // *float32: nil means "provider default"
	})
	if err != nil {
		return err
	}
	fmt.Println(resp.Text)                 // assistant text
	fmt.Println(resp.StopReason)           // why generation stopped
	fmt.Println(resp.Usage.InputTokens)    // accounting (zero if unknown)
	return nil
}
```

Every provider is constructed the same way and returns an `llms.ChatClient`:

| Provider | Constructor | Notable options |
|---|---|---|
| Anthropic | `anthropic.New(...)` | `WithAPIKey`, `WithModel`, `WithMaxRetries`, `WithHTTPClient` |
| OpenAI (chat) | `openai.NewChat(...)` | `WithAPIKey`, `WithModel`, `WithMaxRetries`, `WithHTTPClient` |
| Google | `google.New(...)` | `WithAPIKey`, `WithModel`, `WithMaxRetries` |
| Ollama | `ollama.New(...)` | `WithBaseURL`, `WithModel`, `WithHTTPClient` |

`WithMaxRetries` controls *SDK-level* retries; the toolkit defaults provider
SDK retries to 0 and expects you to use the retry middleware (below) for one
consistent policy.

### Tool use

Tool calls and results are content parts; a round trip is: request with
tools → inspect `resp.ToolCalls` → send results back as a tool message.

```go
weather := llms.ToolDefinition{
	Name:        "get_weather",
	Description: "Get the current weather for a city.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
}

first, err := client.Complete(ctx, llms.ChatRequest{
	Purpose:    llms.PurposeChat,
	Messages:   []llms.Message{llms.UserText("Weather in Paris?")},
	Tools:      []llms.ToolDefinition{weather},
	ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceTool, Name: "get_weather"},
	MaxTokens:  512,
})
// ... handle err ...

tc := first.ToolCalls[0] // tc.ID, tc.Name, tc.Parameters (json.RawMessage)

final, err := client.Complete(ctx, llms.ChatRequest{
	Purpose: llms.PurposeChat,
	Messages: []llms.Message{
		llms.UserText("Weather in Paris?"),
		first.Message, // the assistant turn that requested the tool
		llms.ToolResultMessage(llms.ToolResult{
			ToolCallID: tc.ID,
			Content:    `{"city":"Paris","temp_c":18,"summary":"clear"}`,
		}),
	},
	Tools:     []llms.ToolDefinition{weather},
	MaxTokens: 512,
})
// final.Text is the model's answer using the tool result
```

`ToolChoice.Type` is `ToolChoiceAuto` (default), `ToolChoiceNone`,
`ToolChoiceRequired` (model must call one of the offered tools, its pick),
or `ToolChoiceTool` (force a specific named tool). `Required` maps to
Anthropic `any` / OpenAI `required` / Gemini ANY-mode; on Ollama both
`Required` and `Tool` are best-effort (it has no `tool_choice`). A
`Required`/`Tool` choice with no tools offered is rejected up front.
Provider differences are documented in
[`docs/MAESTRO_DIVERGENCES.md`](docs/MAESTRO_DIVERGENCES.md).

### Embeddings

```go
import "github.com/SnapdragonPartners/maestro-llms/llms/providers/openai"

emb, err := openai.New(
	openai.WithAPIKey(apiKey),
	openai.WithModel("text-embedding-3-small"),
)
// ... handle err ...

out, err := emb.Embed(ctx, llms.EmbeddingRequest{
	Purpose:    llms.PurposeEmbedding,
	Dimensions: 256, // optional per-request override
	Inputs: []llms.EmbeddingInput{
		{ID: "a", Text: "the quick brown fox"},
		{ID: "b", Text: "jumps over the lazy dog"},
	},
})
// out.Vectors preserves input order/IDs; out.Vectors[i].Values is []float32
// out.Usage.EmbeddingTokens for accounting
```

Task-typed embeddings are provider-neutral and advisory: `EmbeddingRequest.Task`
(e.g. `llms.EmbeddingTaskRetrievalDocument` / `…RetrievalQuery`) and an
optional `EmbeddingInput.Title` are honored where supported (Gemini) and
ignored where not (OpenAI).

### Vertex AI (Anthropic & Gemini embeddings)

For Google Vertex AI (e.g. behind Private Service Connect), auth and the
endpoint are **app-supplied** — the toolkit does no ADC discovery. Anthropic
on Vertex lives in a separate leaf package so the base `anthropic` package
stays Google-dependency-free:

```go
import (
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/anthropic/anthropicvertex"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/google"
)

// Claude via Vertex. creds is *google.Credentials YOU built (service
// account / Workload Identity); for PSC also pass WithEndpoint + an
// WithHTTPClient whose transport reaches the PSC endpoint AND carries
// Google auth (overriding the SDK's client discards its auth — ADR-0009).
chat, err := anthropicvertex.New(
	anthropicvertex.WithRegion("us-central1"),
	anthropicvertex.WithProjectID("my-proj"),
	anthropicvertex.WithModel("claude-sonnet-4@20250514"),
	anthropicvertex.WithCredentials(creds),
	// anthropicvertex.WithEndpoint(pscURL), anthropicvertex.WithHTTPClient(pscClient),
) // returns the same *anthropic.Client (all translation/middleware reused)

// gemini-embedding-001 via Vertex. AutoTruncate=false is fail-closed:
// MaxInputBytes is REQUIRED (genai cannot send autoTruncate:false, so an
// oversized input is rejected client-side rather than silently truncated).
emb, err := google.NewEmbeddings(google.EmbeddingConfig{
	Model:         "gemini-embedding-001",
	Project:       "my-proj",
	Location:      "us-central1",
	Credentials:   creds,
	MaxInputBytes: 8000,        // required unless AutoTruncate=true
	// Endpoint: pscURL, HTTPClient: pscClient,
})
```

`gemini-embedding-001` is single-input: a multi-input request is rejected with
a typed `bad_request` (the toolkit never fans out — the app owns chunking). A
plain `WithAPIKey` selects the direct Gemini API instead of Vertex; mixing API
key and Vertex fields fails closed. PSC/DNS/VPC-SC/IAM is your
infrastructure's concern, not the toolkit's.

### Middleware

Middleware is `func(Client) Client`, composed with `ChainChat` /
`ChainEmbeddings`. **The first argument is the outermost wrapper, and
composition order is semantically significant — it changes correctness, not
just performance.**

| Middleware | Purpose |
|---|---|
| `ValidationChat` | Reject structurally invalid requests (text-only `System`, tool-call↔result pairing, valid roles). Chat only. |
| `RetryChat` / `RetryEmbeddings` | Retry while `llms.Retryable`, honoring `RetryAfter`; exponential backoff + optional jitter. |
| `TimeoutChat` / `TimeoutEmbeddings` | Per-attempt `context` deadline. |
| `CircuitChat` / `CircuitEmbeddings` | Three-state breaker; fast-fails with a non-retryable `*CircuitOpenError`; single-flight HalfOpen probe. |
| `RateLimitChat` / `RateLimitEmbeddings` | Reservation protocol against a `ratelimit.Limiter`. |
| `MetricsChat` / `MetricsEmbeddings` | One app-neutral `Event` per call to a narrow `Observer` (success and failure). |

The easy path is `RecommendedChat`, which composes the spec's recommended
order — `validation → retry → per-attempt timeout → circuit → rate limit →
metrics → provider`:

```go
import (
	"time"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
)

c := middleware.RecommendedChat(client, middleware.RecommendedConfig{
	Retry:    middleware.DefaultRetryConfig(), // 5 attempts, 1s→30s, ×2, ±10% jitter
	Timeout:  30 * time.Second,                // per attempt; 0 omits it
	Circuit:  middleware.DefaultCircuitConfig(),
	Observer: myObserver,                      // optional; nil omits metrics
	// Limiter: someLimiter,                   // optional; nil omits rate limiting
})
resp, err := c.Complete(ctx, req) // same llms.ChatClient interface
```

`RecommendedConfig`'s zero value is usable (validation/retry/circuit on with
defaults; timeout/rate-limit/metrics opt-in). For a custom order or subset,
`ChainChat` is the primitive:

```go
c := middleware.ChainChat(client,
	middleware.ValidationChat(),
	middleware.RetryChat(middleware.DefaultRetryConfig()),
	middleware.TimeoutChat(30*time.Second),
)
```

Each retry attempt independently flows through timeout, circuit, and the
rate-limit reservation (retries are real provider traffic, gated like first
attempts). The tradeoffs of reordering (retry vs. reservation, total vs.
per-attempt timeout) are in `docs/specification.md` ("Recommended order").

### Rate limiting

`llms/ratelimit` ships a process-local **token-bucket + concurrency-semaphore**
limiter. You construct it and hand it to the rate-limit middleware (directly
or via `RecommendedConfig.Limiter`); the middleware does the bookkeeping.

```go
import (
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/ratelimit"
)

lim := ratelimit.NewInMemoryLimiter(ratelimit.Config{
	TokensPerMinute: 100_000, // 0 = token-unlimited
	MaxConcurrency:  8,        // 0 = concurrency-unlimited
	// MaxWait: 0 => block until ctx is done; BufferFactor defaults to 0.9
})

c := middleware.RecommendedChat(client, middleware.RecommendedConfig{
	Limiter: lim, // nil => rate-limit middleware omitted
})
// or compose just the rate-limit middleware yourself (it returns a
// ChatMiddleware, so apply it via ChainChat):
//   c := middleware.ChainChat(client,
//           middleware.RateLimitChat(lim, middleware.DefaultEstimator{}))
```

`ratelimit.Config`'s zero value is usable (a zero dimension is "unlimited").
The protocol the middleware runs per call is a **reservation**: `Reserve`
(estimate units up front) → run the request → `Commit` (actual `Usage`) →
`Release`. Over-use drives the bucket negative (debt repaid by future
refills) so traffic is never undercounted; `Release` always runs, even if
the request's context was canceled.

A limiter that exposes the optional `LimiterStats` capability (the in-memory
one does) can be type-asserted for a point-in-time snapshot:

```go
if s, ok := any(lim).(ratelimit.LimiterStats); ok {
	snap, _ := s.Stats(ctx) // available tokens, in-flight count, ...
}
```

**Distributed limiting:** the in-memory limiter is for single-process/local
use. For a multi-instance deployment (e.g. Cloud Run) implement
`ratelimit.Limiter` (`Reserve` returning a `Reservation` with `Commit`/
`Release`) backed by shared storage and pass *that* to the same middleware —
nothing else changes. The package intentionally ships only the in-memory
implementation; the shared backend is the application's to provide.

### Errors

One typed model, resolvable through wrapping with `errors.As`:

```go
resp, err := c.Complete(ctx, req)
switch {
case err == nil:
	// ok
case llms.Retryable(err):
	// transient: rate_limited / timeout / unavailable / local limiter.
	// llms.RetryAfter(err) gives a backoff hint (0 if none).
default:
	var pe *llms.ProviderError
	if errors.As(err, &pe) {
		// pe.Kind (auth, config, bad_request, content_policy, ...),
		// pe.StatusCode, pe.RetryAfter
	}
}
```

`llms.Retryable` is the single classifier — retry and circuit middleware use
it, no second policy ([ADR-0004](docs/adr/0004-retry-circuit-reuse-llms-retryable.md)).
`*CircuitOpenError` and `*ValidationError` (from middleware) are deliberately
**non-retryable** ([ADR-0005](docs/adr/0005-circuit-open-error.md),
[ADR-0006](docs/adr/0006-validation-error.md)).

### Testing your code (fakes)

Downstream tests should not hit the network. `llms/testllm` provides
deterministic fakes implementing `llms.ChatClient` / `llms.EmbeddingClient`:

```go
import "github.com/SnapdragonPartners/maestro-llms/llms/testllm"

fake := &testllm.FakeChatClient{Text: "canned reply"}              // fixed text
fake := &testllm.FakeChatClient{Responses: []llms.ChatResponse{…}} // scripted, in order
fake := &testllm.FakeChatClient{Err: someErr}                      // always errors
fake := &testllm.FakeChatClient{Func: func(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
	// full control: assert on req, return anything, simulate latency/errors
}}
```

The fakes are concurrency-safe and record calls, so they compose under the
same middleware as real clients.

## Development

```
make build    # lint + go build ./...
make test     # unit tests with coverage   (single: make test TESTARGS='-run TestName ./llms/...')
make lint     # gofmt + golangci-lint  (strict gate — see below)
make fix      # auto-fix import grouping
make install-hooks   # pre-push lint+test hook
```

The lint gate is strict and enforced in CI (`build-lint-test` + `CodeQL`):
`fieldalignment`, `gocritic` (`rangeValCopy`), `revive` (unused params), and
modernizers (`min`/`max`, `WaitGroup.Go`) all fail the build. Run
`make lint` before pushing.

### Live integration tests

Build-tagged (`//go:build integration`) tests exercise the real provider
APIs. They never run in normal `make test`/CI, and each **skips** unless its
credentials/host are present (so any subset works).

- **`make test-integration`** is the one correct command on every OS — it is
  OS-aware. On **macOS**, AMFI/Gatekeeper blocks freshly built *unsigned*
  test binaries (a plain `go test` wedges in `dyld`), so the target routes
  through a compile + ad-hoc codesign step; on Linux it runs `go test`
  directly. The canonical run is the *Integration (live providers)* CI
  workflow (manual `workflow_dispatch`, Linux + a runner Ollama).

| Provider | Key / host (skipped if unset) | Model override (default) |
|---|---|---|
| Anthropic | `ANTHROPIC_API_KEY`, else `MAESTRO_ANTHROPIC_API_KEY` | `ANTHROPIC_MODEL` (`claude-haiku-4-5-20251001`) |
| OpenAI | `OPENAI_API_KEY` | `OPENAI_CHAT_MODEL` (`gpt-4o-mini`), `OPENAI_EMBED_MODEL` (`text-embedding-3-small`) |
| Google | `GEMINI_API_KEY`, else `GOOGLE_GENAI_API_KEY` / `GOOGLE_API_KEY` | `GOOGLE_MODEL` (`gemini-2.5-flash`) |
| Ollama | local daemon at `OLLAMA_HOST` (`http://localhost:11434`) | `OLLAMA_MODEL` (Makefile defaults `llama3.2:1b`) |

```sh
MAESTRO_ANTHROPIC_API_KEY=sk-ant-… OPENAI_API_KEY=sk-… GEMINI_API_KEY=… make test-integration
```

The `MAESTRO_ANTHROPIC_API_KEY` fallback lets you keep `ANTHROPIC_API_KEY`
unset locally so Claude Code's OAuth auth keeps working in the same shell.
Point Ollama at a **non-reasoning** model (e.g. `llama3.2:1b`): reasoning
models emit a separate `thinking` field this client does not surface.

## Contributing

Work lands via pull request; `main` is branch-protected and CI must pass.
Conventions: a PR that intentionally diverges from Maestro appends a row to
`docs/MAESTRO_DIVERGENCES.md`; a significant structural decision lands an ADR
in `docs/adr/` in the same PR. Don't relitigate decisions the spec or an ADR
already settled, and don't "fix" a deliberate limitation an ADR explains.

## Versioning & license

Pre-1.0; v0.x minor versions may break. Shipped lines: **v0.1.0** (core +
Anthropic chat + OpenAI embeddings), **v0.2.0** (OpenAI/Google/Ollama chat +
error classifier), **v0.3.0** (full middleware set + `Recommended*`),
**v0.4.0** (Anthropic-on-Vertex + Gemini/Vertex embeddings, PSC
endpoint/transport injection, task-typed embeddings — see
[ADR-0009](docs/adr/0009-vertex-backend-psc-and-gemini-embeddings.md)).

MIT — see [`LICENSE`](LICENSE).
