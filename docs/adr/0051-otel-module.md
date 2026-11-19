# 0051. OpenTelemetry module: GenAI spans and metrics as a nested v0 module

- **Status:** Proposed
- **Date:** 2026-11-18

## Context

#174 asks us to show the toolkit's data flowing into OpenTelemetry. Every
consumer that wants traces or metrics (Morris on Cloud Run, maestro-agents,
buggrit's latency work) would otherwise write the same adapter against the
same seams. So the deliverable is a reusable adapter plus an example, not
just an example (scope recorded on #174, 2026-11-18).

No change to the root module is needed. The existing seams are enough:

- A `ChatClient`/`EmbeddingClient` decorator sees the request (model,
  `MaxTokens`, `Temperature`) before the call and the response
  (`StopReason`, `Usage`, `Usage.ProviderRequestID`) after it. It runs on
  the call's context, so it can open a span *before* the call, parented on
  the caller's span. The provider's HTTP span (an app's `otelhttp`
  transport) then nests under it.
- The retry layer tags each attempt's context, and
  `middleware.AttemptFromContext` reads the attempt number (ADR-0031).
- `toolloop.Run` takes the caller's context and passes it to every
  `Complete` and every `Tool.Execute`. A span around `Run` is therefore the
  parent of each turn's chat span and each tool span, with no loop hook.

The OpenTelemetry GenAI semantic conventions are unstable, and they are
moving right now:

- The last released set is core semantic conventions **v1.41.0** (Go:
  `go.opentelemetry.io/otel/semconv/v1.41.0`, schema URL
  `https://opentelemetry.io/schemas/1.41.0`).
- Core v1.42.0 deprecated every `gen_ai.*` attribute, metric, event and
  span and moved them to `open-telemetry/semantic-conventions-genai`. The Go
  `semconv/v1.42.0` and later packages have no GenAI names.
- At the time of writing, the new repository has **no release, no tag and no
  schema URL**. Its `main` has already renamed or replaced most of what we
  would emit. `gen_ai.client.token.usage` is now per-direction histograms
  plus `gen_ai.client.inference.usage.*` counters; inference duration is
  `gen_ai.client.inference.duration`; embeddings have their own instruments;
  `gen_ai.usage.cache_creation.input_tokens` is now `...cache_write...`.

Two facts about the toolkit's data shape the mapping:

- `llms.ProviderID` names a wire protocol, not a vendor or deployment.
  Anthropic on Vertex is still `anthropic`, and Gemini on Vertex is still
  `google`. The adapter cannot tell Vertex from the direct API.
- `llms.Usage` leaves unreported fields at zero, so zero means "unknown or
  zero". `ChatResponse` has no response-model field.

## Decision

### 1. Packaging: a nested v0 module

A separate module, `github.com/SnapdragonPartners/maestro-llms/otel`, in
`otel/` with its own `go.mod` and its own tags (`otel/vX.Y.Z`). It is
outside the v1 stability contract and set up like `mcp/` (ADR-0049 §1):
`NESTED_MODULES`, CI tidy check, the CI build against root HEAD through a
temporary `go.work`, Dependabot, and scheduled govulncheck. It requires a
released root version, with no `replace`. It has one package,
`otel/llmotel` (named so it doesn't collide with
`go.opentelemetry.io/otel`).

Why a separate module and why v0: the conventions above will break, and a
v0 module can follow them in a minor release without a `/v2`. It also keeps
OpenTelemetry out of the root module's dependency graph.

The package imports only OpenTelemetry **API** modules
(`go.opentelemetry.io/otel`, `.../trace`, `.../metric`, and `semconv`,
which lives in the `otel` module). The SDK and the exporters appear only in
tests and the example. The app owns the SDK, exporters, resource,
sampling, and endpoint configuration, so the package reads no environment
variables.

### 2. Conventions: pinned to released v1.41.0

The package emits the GenAI conventions as released in semconv **v1.41.0**.
It uses the Go `semconv/v1.41.0` constants and sets that schema URL on its
tracer and meter. It does not track the unreleased GenAI repository.

When that repository cuts a release with a schema URL, the module moves to
it in one minor version: renames, new instruments, and release notes that
list every changed name. In v0 it never emits both the old and new names,
and it has no `OTEL_SEMCONV_STABILITY_OPT_IN`-style switch.

Rejected: tracking the GenAI repository's `main`. It has no version to cite,
backends don't recognize it yet, and it could change between our releases.

Our own attributes, where the conventions have nothing, use the prefix
`maestro_llms.` (§4).

### 3. API

```go
type Options struct {
	TracerProvider trace.TracerProvider // nil: otel.GetTracerProvider()
	MeterProvider  metric.MeterProvider // nil: otel.GetMeterProvider()
	// ProviderName returns gen_ai.provider.name for a model. nil: the
	// default mapping (§4). Apps that know the deployment (Vertex, a named
	// OpenAI-compatible vendor) override it.
	ProviderName func(llms.ModelRef) string
}

func New(opts Options) (*Telemetry, error) // creates the instruments once

func (t *Telemetry) Chat() middleware.ChatMiddleware           // attempt layer: span + metrics
func (t *Telemetry) Embeddings() middleware.EmbeddingMiddleware
func (t *Telemetry) ChatCall() middleware.ChatMiddleware       // logical-call layer: span only
func (t *Telemetry) EmbeddingsCall() middleware.EmbeddingMiddleware
func (t *Telemetry) Tools(tools []toolloop.Tool) []toolloop.Tool
func (t *Telemetry) Run(ctx context.Context, cfg toolloop.Config, ro RunOptions) toolloop.Outcome

type RunOptions struct {
	AgentName string // gen_ai.agent.name and the span name suffix; optional
}
```

A nil provider falls back to the global one, which the package reads and
never sets (the `otelhttp` convention). `New` returns an error only if
instrument creation fails.

**The seam is a decorator, not an `Observer`.** `Chat()` and `Embeddings()`
are ordinary middleware. To get one span per provider attempt, wrap the
base client: `RecommendedChat(t.Chat()(base), cfg)`. That puts the span
innermost, inside retry, timeout, circuit and the reservation, like
`MetricsChat`. To get one span per logical call as well, wrap the chain from
outside with `t.ChatCall()`. The attempt spans then nest under the logical
span, and the gaps between them are backoff and limiter wait.

**Metrics come from exactly one layer.** `Chat()`/`Embeddings()` record the
§4 metrics. `ChatCall()`/`EmbeddingsCall()` record none, and their span
carries no `gen_ai.usage.*` attributes (§4). Without that split, nesting
the same decorator would double every token sum and operation count, and
retries would mix logical-call latency into the attempt-duration
histogram.

An app that wants metrics per logical call rather than per attempt uses
`Chat()` outside the chain *instead of* inside it. Using `Chat()` in both
places is a misuse the package can't detect, and the GoDoc says so.

Rejected: an `Observer`/`ContextObserver` adapter. An `Event` arrives only
after the call. The span would have to be rebuilt with start = end −
latency, so nothing the provider call does could nest under it. And `Event`
has no request fields and no stop reason. `RecommendedConfig` needs no new
slot, because `RecommendedChat` already takes a base client to wrap.

`Tools` wraps each tool's `Execute`. The loop-generated `get_toolbox`
(ADR-0047) is not one of `Config.Tools`, so it gets no span. `Run` calls
`toolloop.Run` inside a span. It does not wrap `cfg.Tools` or `cfg.Client`
itself, because doing so could double-wrap; the example shows all three
used together.

### 4. Mapping

**`gen_ai.provider.name` default:** `openai` → `openai`, `anthropic` →
`anthropic`, `google` → `gcp.gen_ai` (the listed value for "backend
unknown"). Any other `ProviderID` (`ollama`, `openaicompat`, app-defined)
→ its string as-is, which the conventions allow for unlisted providers.
`ProviderName` overrides the mapping. For example, an app on Vertex returns
`gcp.vertex_ai`.

**Chat span:** kind `CLIENT`, name `chat {model}`. Attributes:

| Attribute | Source | When |
|---|---|---|
| `gen_ai.operation.name` | `chat` | always |
| `gen_ai.provider.name` | as above | always |
| `gen_ai.request.model` | `Model().Name` | always |
| `gen_ai.request.max_tokens` | `MaxTokens` | > 0 |
| `gen_ai.request.temperature` | `*Temperature` | non-nil |
| `gen_ai.response.id` | `Usage.ProviderRequestID` | non-empty |
| `gen_ai.response.finish_reasons` | `[StopReason]` | non-empty |
| `gen_ai.usage.input_tokens` | `InputTokens` (includes cache, ADR-0023) | > 0, success |
| `gen_ai.usage.output_tokens` | `BillableOutputTokens` (includes reasoning) | > 0, success |
| `gen_ai.usage.cache_read.input_tokens` | `CacheReadTokens` | > 0, success |
| `gen_ai.usage.cache_creation.input_tokens` | `CacheWriteTokens` | > 0, success |
| `gen_ai.usage.reasoning.output_tokens` | `ReasoningTokens` | > 0, success |
| `maestro_llms.attempt` | `AttemptFromContext` | when present |
| `maestro_llms.purpose` | `Purpose` | non-empty |
| `error.type` | §5 | failure |

The two output fields follow the conventions' definitions, which differ
from ours: `gen_ai.usage.output_tokens` "SHOULD include" reasoning, so it
is `BillableOutputTokens`, not our visible-only `OutputTokens`. Cache
counts are subsets of input on both sides. A zero `Usage` field is never
emitted, because zero may mean "not reported". `gen_ai.response.model` is
not emitted because the toolkit doesn't carry it.

**Logical-call span** (`ChatCall`/`EmbeddingsCall`): kind `INTERNAL`, name
`maestro_llms.call chat {model}` or `maestro_llms.call embeddings {model}`.
It has the operation, provider and request-model attributes,
`maestro_llms.purpose`, and `error.type` on failure. It has no
`gen_ai.usage.*`, no `gen_ai.response.*` and no metrics. A different name
and kind keep it from being read as a second inference call, both in trace
views and in span-derived metrics (Tempo's metrics-generator, for example).

**Embeddings span:** kind `CLIENT`, name `embeddings {model}`.
`gen_ai.operation.name` = `embeddings`, provider, request model,
`gen_ai.embeddings.dimension.count` from `Dimensions` when > 0,
`gen_ai.usage.input_tokens` from `EmbeddingTokens` when > 0 on success,
`maestro_llms.attempt`, `maestro_llms.purpose`, and `error.type`.

**Tool span:** kind `INTERNAL`, name `execute_tool {tool}`.
`gen_ai.operation.name` = `execute_tool`, `gen_ai.tool.name`,
`gen_ai.tool.call.id`, `gen_ai.tool.type` = `function`. A non-nil `Execute`
error sets status Error with `error.type` = `_OTHER`. A result with
`IsError` sets status Error with `error.type` = `tool_error`: it is a tool
failure the model sees, and a trace view should show it.

**Run span:** kind `INTERNAL`, name `invoke_agent` or `invoke_agent
{AgentName}`. `gen_ai.operation.name` = `invoke_agent`, `gen_ai.agent.name`
when set, `gen_ai.request.model` from `cfg.Client.Model()`, and on return:

- `maestro_llms.toolloop.outcome` = `Outcome.Kind`
- `maestro_llms.toolloop.iterations` = the number of turns
- `gen_ai.usage.input_tokens` and `gen_ai.usage.output_tokens` from the run
  total, when > 0

`OutcomeFinalAnswer` and `OutcomeTerminalTool` leave the status unset.
Every other kind sets status Error with `error.type` = the outcome kind.
That includes kinds added later, because `OutcomeKind` grows (ADR-0032).

**Metrics** (v1.41 instruments, with the conventions' advisory bucket
boundaries set explicitly):

- `gen_ai.client.operation.duration` (histogram, `s`): one record per chat
  or embeddings call, on success and failure. Attributes:
  `gen_ai.operation.name`, `gen_ai.provider.name`,
  `gen_ai.request.model`, and `error.type` on failure.
- `gen_ai.client.token.usage` (histogram, `{token}`): on success, one
  record with `gen_ai.token.type` = `input` and one with `output` (from the
  same fields as the span), each only when > 0, with the same attributes.

Tool and run spans record no metrics in v1.41. `maestro_llms.attempt` and
`maestro_llms.purpose` go on spans only, never on metrics: `Purpose` is an
open set, and both would multiply series. Metric attributes stay limited to
the low-cardinality convention set.

### 5. Errors and content

`error.type` is the `llms.ErrorKind` of a `*llms.ProviderError`, found with
`errors.As`. A `*llms.LimitError` gives `rate_limited`, which matters only
when the decorator sits outside the limiter. Anything else gives `_OTHER`.
The span status description is the same fixed string.

The package **never** calls `span.RecordError` and never writes
`err.Error()` anywhere. Provider error text can echo request content, and
Morris must be able to export metadata without content reaching a
telemetry buffer (#112).

No prompt, completion, system prompt, tool definition, tool argument or
tool result is emitted, and there is no option to turn content on. Content
capture stays with #112. Tool and agent names are emitted: the app chose
them, and a model-invented name never reaches a wrapped tool.

### 6. Verification

- Unit tests use the SDK's in-memory span recorder and a manual metric
  reader against `testllm` fakes. They cover every attribute row above,
  zero-usage omission, the error mapping, span nesting (run → attempt and
  tool, logical → attempt under `RecommendedChat` with retry), and metric
  counts with `ChatCall()` outside and `Chat()` inside a retrying chain:
  exactly one duration record per attempt, and token sums equal to the
  successful attempt's usage. They also check that a
  canary error string never appears in any exported span or metric.
- `examples/otel` exports over OTLP/HTTP to the endpoint in the standard
  `OTEL_EXPORTER_OTLP_ENDPOINT` (the example reads it, the package doesn't).
  It runs a tool loop against a `testllm` fake by default, or against a live
  provider by flag.
- A live test, gated on `LLMOTEL_LIVE_OTLP_ENDPOINT` and
  `LLMOTEL_LIVE_TEMPO_URL`, exports one traced run and reads the trace back
  by ID from Tempo. We run it against a Grafana LGTM stack.

## Consequences

- Consumers get GenAI-convention spans and metrics with one line of
  wiring per client, and the root module doesn't change.
- Released-v1.41 names are what today's backends recognize. The cost is a
  breaking minor release of this module when the GenAI repository ships.
  The release notes for that move must list every renamed instrument and
  attribute, because dashboards and alerts depend on them.
- `gen_ai.provider.name` is a best guess unless the app overrides it.
  Google traffic defaults to `gcp.gen_ai` rather than a specific backend.
- Response model and time-to-first-chunk are absent: the first because the
  toolkit doesn't carry it, the second because there is no streaming
  (ADR-0003).

## Not in this ADR

Logs (Loki, or the GenAI events), a possible follow-up. Content capture
(#112). Limiter and bucket gauges. A `RecommendedConfig` slot. Server-side
`mcpserver` instrumentation.

## References

- #174 (scope comment, 2026-11-18); #112
- ADR-0003, ADR-0011, ADR-0016, ADR-0023, ADR-0031, ADR-0032, ADR-0047,
  ADR-0049 §1
- OpenTelemetry semantic conventions v1.41.0 (GenAI);
  semantic-conventions v1.42.0 release notes (the move);
  `open-telemetry/semantic-conventions-genai` at `76e0c19` (unreleased)
