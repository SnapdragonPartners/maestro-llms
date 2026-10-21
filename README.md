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

It is shared by three consumers with deliberately different needs — Maestro
(desktop/local), Morris (cloud, multi-instance), and maestro-cms (content
service, budget-aware chunking) — so nothing in the package may import
product-specific assumptions from any of them. When a feature needs app
context, the answer is an interface the app implements, not a concrete
implementation here.

The binding design is [`docs/specification.md`](docs/specification.md).
Rationale for non-obvious decisions lives in [`docs/adr/`](docs/adr/).
Intentional differences from the original Maestro implementation are tracked
in [`docs/MAESTRO_DIVERGENCES.md`](docs/MAESTRO_DIVERGENCES.md).

## Features

- **One conversation model.** `Message` / `ContentPart` is the single
  representation; each provider adapter translates to/from that provider's
  wire shape at the boundary. Tool calls and results are content parts, not
  side-channel fields, so a conversation round-trips unambiguously.
- **Reasoning round-trip.** Anthropic `thinking` blocks come back as
  `ContentReasoning` parts and are replayed unchanged when you resend the
  transcript; other providers drop parts that aren't theirs. See
  [ADR-0021](docs/adr/0021-reasoning-content-part.md).
- **Reasoning control.** `ChatRequest.Reasoning` (effort level and/or
  thinking budget) maps 1:1 to each provider's control or is omitted, and
  `ChatResponse.AppliedReasoning` reports what was sent. See
  [ADR-0022](docs/adr/0022-reasoning-effort.md).
- **Per-request provider fields.** `ChatRequest.ProviderParams` deep-merges
  raw body fields into a specific provider's wire request when you know
  more about a model than the neutral API covers. See
  [ADR-0024](docs/adr/0024-provider-params.md).
- **Five chat providers**: Anthropic (Messages), OpenAI (Responses API),
  Google (Gemini via genai), Ollama (hand-rolled `/api/chat`, no SDK),
  OpenAI-compatible servers and gateways such as vLLM and LunaRoute
  (`openaicompat`, OpenAI Chat Completions via openai-go; ADR-0015,
  ADR-0027; `vllm` remains as an alias).
- **Embeddings**: OpenAI and Gemini/Vertex (`gemini-embedding-001`),
  order/ID-preserving, per-request dimension override, task-typed
  (`EmbeddingTask`/`Title`, advisory).
- **Vertex AI backend**: Claude via `anthropicvertex` (separate leaf
  package — base `anthropic` stays Google-dep-free), Gemini chat, and
  Gemini embeddings;
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
- **Tool loop helper** (`llms/toolloop`): synchronous, app-neutral helper
  for multi-turn tool round trips over a `ChatClient`. Executes
  app-supplied tools, preserves provider signatures, returns a typed
  `Outcome`. Deliberately a tool loop, not an agent loop — see
  [ADR-0011](docs/adr/0011-toolloop-helper.md).
- **Model listing & upgrade detection** (`llms.ModelLister`): optional
  capability for surfacing "newer model available in your family"
  prompts to users — applications decide whether to upgrade, the
  toolkit does not. Per-provider `LatestInFamily` helpers know each
  provider's family-naming convention; Ollama implements list-only
  (no canonical family). See [ADR-0012](docs/adr/0012-model-lister.md).
- **Text-level token estimator** (`llms.EstimateTextTokens`): exported
  `func(string) int` for budget-aware chunking, with documented
  **neutral** bias (distinct from the high-biased middleware
  estimator). See
  [ADR-0013](docs/adr/0013-text-token-estimator.md).
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
| Google | `google.New(...)` | `WithAPIKey`, `WithModel`, `WithMaxRetries`, `WithBaseURL`, `WithHTTPClient` |
| Ollama | `ollama.New(...)` | `WithBaseURL`, `WithModel`, `WithHTTPClient` |
| OpenAI-compatible (vLLM, LunaRoute, …) | `openaicompat.New(...)` | `WithBaseURL` (required), `WithModel` (required), `WithAPIKey` (optional — vLLM defaults to no auth), `WithHTTPClient` |

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

#### Reasoning parts

Current Anthropic models think by default. Each `thinking` /
`redacted_thinking` block comes back in `resp.Message` as a
`ContentReasoning` part. Append `resp.Message` as-is, and the blocks go back
on the next request, which a tool loop needs. `Reasoning.Text` is for display
only and is empty unless the app asks for summarized thinking. Other providers
drop reasoning parts that aren't theirs, so a transcript can switch providers.

Anthropic binds each thinking block to the conversation before it. **If you
edit history** (drop or rewrite earlier turns, compact client-side), remove
all reasoning first with `llms.WithoutReasoning(msgs)`. Otherwise newer
Anthropic accounts reject the request with `400 ... bound to a different
conversation`. Appending is always safe, and `toolloop` only appends.

#### Reasoning control

```go
resp, err := client.Complete(ctx, llms.ChatRequest{
	Messages:  msgs,
	Reasoning: &llms.ReasoningConfig{Effort: llms.ReasoningEffortLow},
})
// resp.AppliedReasoning is what the adapter actually sent (nil: nothing,
// so the model default applied).
```

Each adapter maps a value to exactly one provider setting, or omits it when
the provider has no equivalent. An unsupported value never fails locally; it
runs at the model default, and `AppliedReasoning` shows that. Adapters don't
guess which values a particular model accepts, so a value the provider
supports but the model rejects comes back as the provider's error. For
example, Opus 5.5 can't disable thinking, so `ReasoningEffortNone` (sent as
`thinking: disabled`) gets a 400.

| Neutral | Anthropic | OpenAI / openaicompat | Google | Ollama |
|---|---|---|---|---|
| `None` | `thinking: disabled` | `none` | — | `think: false` |
| `Minimal` | — | `minimal` | `MINIMAL` | — |
| `Low`/`Medium`/`High` | `output_config.effort` | same | `LOW`/`MEDIUM`/`HIGH` | `think: "low"`… |
| `XHigh`/`Max` | `output_config.effort` | same | — | — |
| `BudgetTokens` | `thinking: enabled, budget_tokens` | — | `thinkingBudget` | — |

Gemini takes `thinkingLevel` on Gemini 3 and `thinkingBudget` on 2.5. Set the
field that fits your model. OpenAI-compatible servers vary: on LunaRoute,
GLM-5.3 treats effort as a soft bound and DeepSeek turns reasoning off at
`None`.

#### Provider-specific fields (`ProviderParams`)

```go
resp, err := client.Complete(ctx, llms.ChatRequest{
	Messages: msgs,
	ProviderParams: map[llms.ProviderID]map[string]any{
		llms.ProviderVLLM:   {"chat_template_kwargs": map[string]any{"enable_thinking": false}},
		llms.ProviderGoogle: {"generationConfig": map[string]any{"candidateCount": 1}},
	},
})
```

Each adapter merges only its own entry, after mapping everything else, so a
request can carry fields for several providers. Objects merge recursively,
so the Gemini entry above keeps the adapter's temperature and max tokens.
Any other value replaces what the adapter mapped. Values must marshal to
JSON, or the call fails as `bad_request`. Overrides are outside the
stability contract, and estimation and rate-limiting middleware don't see
them. If an override replaces a mapped reasoning field, that field drops out
of `AppliedReasoning`.

### Tool loop (`llms/toolloop`)

For multi-turn tool use, `llms/toolloop` wraps the round trip shown above
over a `ChatClient`: it sends the request, executes every tool call the
model emits, appends the matching tool results, and repeats until the
model returns a final answer or the loop hits a stop condition.

```go
import "github.com/SnapdragonPartners/maestro-llms/llms/toolloop"

weather := toolloop.Tool{
	Definition: llms.ToolDefinition{Name: "get_weather", InputSchema: schemaJSON},
	Execute: func(ctx context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
		// Unmarshal call.Parameters yourself. Return ToolResult{IsError: true}
		// for model-visible failures the loop should let the model recover
		// from; return a non-nil error for loop-fatal ones.
		return toolloop.ToolResult{Content: `{"city":"Paris","temp_c":18}`}, nil
	},
}

out := toolloop.Run(ctx, toolloop.Config{
	Client:  client,
	Request: llms.ChatRequest{Messages: []llms.Message{llms.UserText("Weather in Paris?")}},
	Tools:   []toolloop.Tool{weather},
})

switch out.Kind {
case toolloop.OutcomeFinalAnswer:
	fmt.Println(out.Response.Text)
case toolloop.OutcomeTerminalTool, toolloop.OutcomeEmptyResponse,
	toolloop.OutcomeMaxIterations, toolloop.OutcomeLLMError,
	toolloop.OutcomeContextExceeded, toolloop.OutcomeCompactionError,
	toolloop.OutcomeToolError, toolloop.OutcomeCanceled, toolloop.OutcomeDeadline:
	// inspect out.Err, out.Messages, out.TotalUsage
}
```

`MaxIterations` bounds the number of tool-calling turns. Zero means the
default of 8. The limit-hitting turn's tools are executed and their results
appended, then the loop stops with `OutcomeMaxIterations` without another
provider call. `toolloop.NoIterationLimit` (any negative value) removes the
ceiling ([ADR-0019](docs/adr/0019-toolloop-deadline-and-no-iteration-limit.md)).

When the loop's own context reaches its deadline during a model call or a
tool, the outcome is `OutcomeDeadline` ("ran out of time"), not an LLM or
tool error; `errors.Is(out.Err, context.DeadlineExceeded)` holds. A
per-attempt deadline from the `Timeout` middleware leaves the loop's context
live, so it is still `OutcomeLLMError`. An explicit cancel is
`OutcomeCanceled`.

If the model returns no text and no tool calls, typically a reasoning
model that spent its whole output budget on reasoning, the outcome is
`OutcomeEmptyResponse` ([ADR-0020](docs/adr/0020-toolloop-empty-response.md)).
`out.Response.StopReason` and `out.TotalUsage.ReasoningTokens` show why. The
empty message is not appended, so `out.Messages` can be continued directly,
for example by appending a user message asking the model to finish, or
retried with a larger `MaxTokens`.

If the transcript outgrows the model's context window, the outcome is
`OutcomeContextExceeded` ([ADR-0026](docs/adr/0026-context-length-error-kind.md)).
`out.Messages` is the transcript that was sent, unchanged, so the app can
shorten it (`llms.WithoutReasoning`, then trim or compact the history) and
run again with it as `Request.Messages`. It relies on the provider signalling
overflow, which Ollama does not (it silently truncates to `num_ctx`); an
overflow without a recognizable signal stays `OutcomeLLMError`.

#### Compaction

Set `Config.Compact` to let the loop shorten its own transcript and keep
going ([ADR-0030](docs/adr/0030-toolloop-compaction.md)). The hook runs
before every model call, with the last response's `Usage` so it can compare
the transcript's size to the app's budget (the toolkit doesn't know the
model's window), and again when the provider reports an overflow, after
which the loop retries once with the compacted transcript. Returning nil
`Messages` means "no change". `llms/toolloop/compact` ships two policies:

```go
// Drop all but the last 4 turns once the transcript reaches 150k tokens,
// or whenever the provider reports an overflow.
keep, _ := compact.KeepLast(compact.KeepLastConfig{Turns: 4, AtTokens: 150_000})

// Or replace older turns with a summary written by a (possibly cheaper) model.
sum, _ := compact.Summarize(compact.SummarizeConfig{Client: cheap, KeepTurns: 4, AtTokens: 150_000})

out := toolloop.Run(ctx, toolloop.Config{Client: client, Request: req, Tools: tools, Compact: sum})
// out.Compactions counts compactions; out.CompactionUsage is the
// summarizer's usage, apart from out.TotalUsage.
```

Both keep the first user message (the task), cut only between turns so a
tool call is never separated from its result, and strip reasoning
(`llms.WithoutReasoning`). A hook error or an invalid compacted transcript
ends the run with `OutcomeCompactionError`.

#### Unbounded runs

`NoIterationLimit` needs no context deadline. Two patterns suit a
long-running agent:

- **One endless `Run`.** A tool blocks until the next event arrives and
  returns it as the tool result. Each turn is appended to the transcript,
  so it grows until it reaches the model's context limit; set
  `Config.Compact` (see Compaction above) so the run can shorten it and
  continue.
- **One `Run` per event.** The app owns memory between events and builds
  each `Request` from it. Every run is bounded by its own work, and the
  transcript never outgrows what the app chooses to carry.

#### Structured output via a terminal tool

To get a schema-validated result, give the model a terminal tool and set
`ToolResult.Stop` on a valid submission ([ADR-0017](docs/adr/0017-toolloop-terminal-tools.md)).
The loop ends with `OutcomeTerminalTool` and no extra model call:

```go
submit := toolloop.Tool{
	Definition: llms.ToolDefinition{Name: "submit_findings", InputSchema: findingsSchema},
	Execute: func(ctx context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
		var f Findings
		if err := json.Unmarshal(call.Parameters, &f); err != nil || f.Validate() != nil {
			// Model-visible: the model fixes its arguments and resubmits.
			return toolloop.ToolResult{Content: "invalid findings: ...", IsError: true}, nil
		}
		return toolloop.ToolResult{Content: "accepted", Stop: true}, nil
	},
}

out := toolloop.Run(ctx, toolloop.Config{
	Client:  client,
	Request: req,
	Tools:   []toolloop.Tool{readFile, grep, submit},
	// Optional: llms.ToolChoice{Type: llms.ToolChoiceRequired} keeps the model
	// from ending with plain text instead of submitting. Not every model
	// accepts forced choice (Claude Opus 5.5 rejects "tool"/"any" with
	// bad_request); for those, rely on the system prompt instead.
})
if out.Kind == toolloop.OutcomeTerminalTool {
	var f Findings
	_ = json.Unmarshal(out.TerminalCall.Parameters, &f) // already validated
}
```

`Stop` acts like a `return` within the assistant turn. Earlier calls in the
turn have already run; later calls are not executed and receive a
synthesized "not executed" error result. `out.Messages` stays well-formed
and can be fed back into `Complete`. A terminal call on the turn that hits
`MaxIterations` is still honored. Setting `Stop` and `IsError` together is
rejected as `OutcomeToolError`.

The helper is deliberately a tool loop, not an agent loop: no agent state,
persistence, audit taxonomy, authorization, tool registries, or built-in
tool adapters. See [`docs/toolloop-proposal.md`](docs/toolloop-proposal.md)
and [ADR-0011](docs/adr/0011-toolloop-helper.md) for the full design and
binding non-goals.

### Model listing & upgrade detection

Long-running projects pin a model ID at config time; months later a newer
snapshot in the same family may have shipped. The toolkit exposes an
optional `ModelLister` capability (v0.6 / [ADR-0012](docs/adr/0012-model-lister.md))
and a per-provider `LatestInFamily` helper so applications can surface
"Opus 4.7 is available, upgrade from 4.5?" prompts to users — the toolkit
does not auto-update.

```go
// Discover via type assertion (optional capability — see ADR-0012).
lister, ok := client.(llms.ModelLister)
if !ok {
    // Provider doesn't expose a model list (e.g. future vLLM).
    return
}
models, err := lister.ListModels(ctx)
// ...

// Per-provider helper: returns (newer, true) only if a strictly newer
// model exists in the same family as currentID.
newer, found := anthropic.LatestInFamily(currentID, models)
if found {
    fmt.Printf("Newer model in family: %s (released %s)\n",
        newer.ID, newer.Created.Format("2006-01-02"))
}

// Or one-shot, when you don't want to cache the list:
newer, found, err = client.LatestInFamily(ctx, currentID)
```

Per-provider notes:

- **Anthropic** (`anthropic.LatestInFamily`): family = `claude-{opus|sonnet|haiku}`. Crosses generations on purpose — `claude-3-5-sonnet-…` and `claude-sonnet-4-5-…` are both `claude-sonnet`. Ordered by `CreatedAt`.
- **OpenAI** (`openai.LatestInFamily`): family = ID with trailing `-YYYY-MM-DD` stripped (`gpt-5-2026-03-15` → `gpt-5`; `gpt-5-mini-2025-12-01` → `gpt-5-mini`). Self-filtering by family means embedding/image IDs in the catalog never collide with `gpt-*` queries. Ordered by `Created` (Unix seconds).
- **Google** (`google.LatestInFamily`): family = `gemini-{pro|flash|nano|ultra}`. The genai list exposes no created date, so ordering uses parsed numeric version from the ID (`gemini-3-pro` > `gemini-2.5-pro` > `gemini-1.5-pro`). `ModelInfo.Created` is zero.
- **Ollama** (`ListModels` only — **no `LatestInFamily`**): the list is *locally pulled* models via `/api/tags`, not a provider catalog. `ModelInfo.Created` is the local pull time (`modified_at`), not provider release time. `Family` is empty: Ollama tags are community-uploaded under arbitrary names and have no canonical family convention.

Family parsing is intentionally permissive: major-version bumps stay in
the same family. Callers that want stricter pinning (e.g. "stay within
the same major version") filter the `ListModels` result themselves.

Non-goals (binding, ADR-0012): not auto-update, not toolkit-side caching,
not a stable-vs-preview filter, not a cross-provider abstraction over
model identity. Apps build those on top.

### OpenAI-compatible servers (vLLM, LunaRoute)

The `openaicompat` package speaks the OpenAI **Chat Completions** surface,
not the Responses API that the `openai` package uses. It is the client for
self-hosted GPU inference via [vLLM](https://docs.vllm.ai) and for hosted
OpenAI-compatible gateways ([ADR-0015](docs/adr/0015-vllm-provider.md)). It
was originally called `vllm`. That package remains as a permanent
deprecated alias, and the provider ID is now `"openaicompat"`
([ADR-0027](docs/adr/0027-pre-v1-api-cleanup.md)).

```go
import "github.com/SnapdragonPartners/maestro-llms/llms/providers/openaicompat"

client, err := openaicompat.New(
    openaicompat.WithBaseURL("http://my-vllm.internal:8000/v1"), // include /v1
    openaicompat.WithModel("mistralai/Ministral-3-14B-Instruct-2512"),
    // WithAPIKey is OPTIONAL: vLLM's default deployment has no auth.
    // Set it only if your operator configured VLLM_API_KEY.
)
```

Notes:

- **No-auth by default** is the distinguishing feature vs hosted
  providers — an empty `WithAPIKey` is a valid configuration, not a
  config error.
- **`ModelLister` is implemented** (`/v1/models`); **`LatestInFamily` is
  not** — HuggingFace-style names have no canonical family. Same shape
  as Ollama.
- **`ModelInfo.Created` is the load time** on the vLLM instance, not
  the upstream release date. Don't surface it as "released N days ago."
- **Tool calling** works through the standard `tools` / `tool_choice`
  request fields, but actual emission depends on the vLLM server's
  per-model `--tool-call-parser` configuration (Mistral / Hermes /
  Llama / Pythonic / etc.). The toolkit forwards; the server decides.
- **Streaming is deferred** per ADR-0003. vLLM supports SSE; consumers
  needing it should follow the streaming ADR when it lands.

#### Other OpenAI-compatible gateways (LunaRoute)

Hosted routers that speak the Chat Completions wire shape work through
`openaicompat` unchanged. Verified
live against LunaRoute with `glm-5.3` and
`deepseek-4.1-flash`, covering forced and required tool choice, parallel
tool calls, multi-turn `toolloop` runs, and reasoning usage (#54):

```go
client, err := openaicompat.New(
    openaicompat.WithBaseURL("https://gw.lunaroute.com/v1"),
    openaicompat.WithAPIKey(lunaRouteKey), // sent as Authorization: Bearer
    openaicompat.WithModel("glm-5.3"),     // IDs as listed by ListModels
)
```

- Pass the key with `WithAPIKey`. The client ignores `OPENAI_API_KEY` /
  `OPENAI_BASE_URL` from the environment (divergence X6), so the
  `export OPENAI_*` setup from LunaRoute's docs does not configure it.
- Reasoning models return reasoning in a separate `reasoning_content`
  field. `Text` is the answer only, and the reasoning is counted in
  `Usage.ReasoningTokens` / `BillableOutputTokens`.
- LunaRoute limits concurrent requests per key. Set the limiter's
  `MaxConcurrency` (`RateLimit` middleware) to match, so excess calls
  queue locally instead of being rejected upstream.

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

### Vertex AI (Anthropic, Gemini chat & Gemini embeddings)

For Google Vertex AI (e.g. behind Private Service Connect), auth and the
endpoint are **app-supplied** — the toolkit does no ADC discovery. Anthropic
on Vertex lives in a separate leaf package so the base `anthropic` package
stays Google-dependency-free.

Both Vertex entry points take the same credential type,
`*cloud.google.com/go/auth.Credentials`. Your app builds it once from its GCP
identity (service account / Workload Identity), for example with
`credentials.DetectDefault` ([ADR-0027](docs/adr/0027-pre-v1-api-cleanup.md)).

```go
import (
	"cloud.google.com/go/auth/credentials"

	"github.com/SnapdragonPartners/maestro-llms/llms/providers/anthropic/anthropicvertex"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/google"
)

creds, err := credentials.DetectDefault(&credentials.DetectOptions{
	Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
}) // or any *auth.Credentials your app builds; the toolkit never discovers them

// Claude via Vertex. For PSC also pass WithEndpoint + a WithHTTPClient whose
// transport reaches the PSC endpoint; WithCredentials authenticates it
// (ADR-0029).
chat, err := anthropicvertex.New(
	anthropicvertex.WithRegion("us-central1"),
	anthropicvertex.WithProjectID("my-proj"),
	anthropicvertex.WithModel("claude-sonnet-4@20250514"),
	anthropicvertex.WithCredentials(creds),
	// anthropicvertex.WithEndpoint(pscURL), anthropicvertex.WithHTTPClient(pscClient),
) // returns the same *anthropic.Client (all translation/middleware reused)

// gemini-embedding-001 via Vertex, same creds. Truncation is fail-closed:
// WithMaxInputBytes is REQUIRED unless WithAutoTruncate(true) (genai cannot
// send autoTruncate:false, so an oversized input is rejected client-side
// rather than silently truncated).
emb, err := google.NewEmbeddings(
	google.WithModel("gemini-embedding-001"),
	google.WithVertex("my-proj", "us-central1"),
	google.WithCredentials(creds),
	google.WithMaxInputBytes(8000),
	// google.WithBaseURL(pscURL), google.WithHTTPClient(pscClient),
)

// Gemini chat via Vertex, same creds and the same options (ADR-0028).
gemini, err := google.New(
	google.WithModel("gemini-2.5-flash"),
	google.WithVertex("my-proj", "global"),
	google.WithCredentials(creds),
	// google.WithBaseURL(pscURL), google.WithHTTPClient(pscClient),
)
```

For PSC, `WithHTTPClient` supplies only the network layer (dialer, CA,
proxy) on all three Vertex constructors: the toolkit authenticates a copy of
it with `WithCredentials` and never mutates your client
([ADR-0029](docs/adr/0029-vertex-injected-client-auth.md)). That matches every
direct provider, where an injected client never removes auth.

`gemini-embedding-001` is single-input: a multi-input request is rejected with
a typed `bad_request` (the toolkit never fans out — the app owns chunking).
`google.WithAPIKey` instead selects the direct Gemini API rather than Vertex;
mixing the API key with `WithVertex`/`WithCredentials` fails closed.
`google.New` (chat) and `NewEmbeddings` share one option type; an option a
constructor does not accept (`WithDimensions` on `New`, for example) is a
config error, never ignored.
PSC/DNS/VPC-SC/IAM is your infrastructure's concern, not the toolkit's.

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

#### Shared limits across instances (`bucket`, Postgres)

For a multi-instance deployment (e.g. Cloud Run, or a GitHub app whose every
use shares one upstream concurrency limit), use the generic **`bucket`**
package ([ADR-0025](docs/adr/0025-bucket-distributed-limiter.md)). It is not
LLM-specific: `bucket.Limiter.Acquire(ctx, key, tokens)` blocks until the key
has room for the tokens and one concurrency slot, and returns a `Lease` you
`Adjust` and always `Release`. Backends: `bucket/memory` (process-local) and
`bucket/postgres` (shared, PostgreSQL 13+). `ratelimit.FromBucket` adapts any
of them to the same middleware:

```go
import (
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
	"github.com/SnapdragonPartners/maestro-llms/bucket/postgres"
	"github.com/SnapdragonPartners/maestro-llms/llms/ratelimit"
)

db := stdlib.OpenDBFromPool(pool) // any *sql.DB; the package imports no driver
bl, err := postgres.New(ctx, db, postgres.Config{}) // fails with *postgres.SchemaError if migrations are missing
// Create the bucket if missing (overwrite=false: the database stays the source of truth).
err = bl.Ensure(ctx, "lunaroute", bucket.Limits{MaxConcurrency: 2}, false)

lim := ratelimit.FromBucket(bl, func(ratelimit.ReservationRequest) string { return "lunaroute" })
c := middleware.RecommendedChat(client, middleware.RecommendedConfig{Limiter: lim})
```

- **Limits live in the database** (`bucket.bucket`), read on every acquire, so
  every instance agrees and a change applies at once. Change them only with
  `Ensure(..., overwrite=true)`, which settles refill under the old rate
  first; a raw `UPDATE` of the limit columns applies the new rate
  retroactively. Deleting a bucket row cascades to its leases.
- **Rates are tokens per minute.** Converting a per-second quota is the app's
  job, and the bucket holds a full minute's burst, so lower the rate or
  `BufferFactor` for per-second providers.
- **Concurrency slots are lease rows renewed in the background.** A crashed
  holder's slot frees within `LeaseTTL` (30s); `MaxLeaseDuration` (15m) caps
  a lease that is never released. A lease found expired closes `Lost()` and
  makes `Release` return `bucket.ErrLeaseLost`. Every database operation is
  bounded by `OpTimeout` (5s). If Postgres is unreachable, nothing is
  admitted (fail closed).
- **A lease is one slot:** the adapter rejects reservations with
  `EstimatedUnits.Requests > 1`.
- **Waiting is polling** with backoff and jitter (no FIFO queue yet), bounded
  by `MaxWait` or ctx.

**Database migrations.** The package never runs DDL. Apply the files from
`postgres.Migrations()` (golang-migrate naming, in a dedicated `bucket`
Postgres schema) with your own migration tool, **before** deploying a release
that needs them. `postgres.SchemaVersion` is the version a release requires;
`postgres.New` and `postgres.CheckSchema` (for readiness probes and
integration tests) return `*postgres.SchemaError` until it is applied. Unit
tests with fakes pass without the migration, so watch the release notes: any
release that raises `SchemaVersion` opens them with **"Database migration
required"**.

### Text-level token estimation

For consumers that need to estimate the token count of a standalone string
— e.g. budget-aware text chunking before embedding calls — the core
package exposes a free function:

```go
n := llms.EstimateTextTokens(s) // approx token count, char-based
```

The bias is intentionally **neutral** (~4 chars/token, rune-counted), which
is different from the request-shaped middleware estimator's high bias.
Over-estimating during chunking produces smaller-than-necessary chunks and
thus more downstream API calls (waste, not safety), so neutral is the
right default for splitting; consumers add their own safety margin if they
want. The middleware estimator stays high-biased because over-reservation
is the safe error at a rate limiter. See
[ADR-0013](docs/adr/0013-text-token-estimator.md) for why the two
estimators stay separate.

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
		// pe.Kind (auth, config, bad_request, content_policy,
		// context_length, ...), pe.StatusCode, pe.RetryAfter
	}
}
```

`llms.Retryable` is the single classifier — retry and circuit middleware use
it, no second policy ([ADR-0004](docs/adr/0004-retry-circuit-reuse-llms-retryable.md)).
`*CircuitOpenError` and `*ValidationError` (from middleware) are deliberately
**non-retryable** ([ADR-0005](docs/adr/0005-circuit-open-error.md),
[ADR-0006](docs/adr/0006-validation-error.md)).

`context_length` means the input does not fit the model's context window:
shorten the conversation before sending it again. Adapters report it only
for a deterministic provider signal (a structured error code, or a
documented message backed by a captured fixture) and otherwise leave the
error `bad_request`; Ollama truncates instead of erroring, so it never
reports it ([ADR-0026](docs/adr/0026-context-length-error-kind.md)).

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
| vLLM | `MAESTRO_VLLM` (full base URL incl. `/v1`, e.g. `http://my-vllm:8000/v1`); optional `MAESTRO_VLLM_API_KEY` | `MAESTRO_VLLM_MODEL` (defaults to first model `/v1/models` reports) |
| LunaRoute (via `openaicompat`) | `LUNAROUTE_API_KEY` | `LUNAROUTE_MODELS` (`glm-5.3,deepseek-4.1-flash`), `LUNAROUTE_BASE_URL` (`https://gw.lunaroute.com/v1`) |

```sh
MAESTRO_ANTHROPIC_API_KEY=sk-ant-… OPENAI_API_KEY=sk-… GEMINI_API_KEY=… make test-integration
MAESTRO_VLLM=http://100.x.x.x:8000/v1 make test-integration   # vLLM live test against your own instance
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
[ADR-0009](docs/adr/0009-vertex-backend-psc-and-gemini-embeddings.md)),
**v0.4.1** (OpenAI `incomplete_details.reason` surfaced as `StopReason`),
**v0.4.2** (Gemini `thought_signature` round-trip via
`ToolCall.ProviderSignature` — see
[ADR-0010](docs/adr/0010-toolcall-provider-signature.md)),
**v0.5.0** (`llms/toolloop` synchronous tool-loop helper — see
[ADR-0011](docs/adr/0011-toolloop-helper.md)),
**v0.6.0** (model listing + per-family upgrade detection, text token
estimator — see [ADR-0012](docs/adr/0012-model-lister.md),
[ADR-0013](docs/adr/0013-text-token-estimator.md)),
**v0.7.0** (self-hosted vLLM provider — see
[ADR-0015](docs/adr/0015-vllm-provider.md)),
**v0.7.1** (`Usage.ReasoningTokens` / `BillableOutputTokens` normalization —
see [ADR-0016](docs/adr/0016-usage-reasoning-tokens.md)),
**v0.8.0** (toolloop terminal tools + executed `MaxIterations` limit turn —
see [ADR-0017](docs/adr/0017-toolloop-terminal-tools.md); anthropic-sdk-go
v1.82 / genai v1.75 with the Anthropic client no longer reading SDK
environment defaults; live toolloop and Vertex integration tests),
**v0.8.1** (`openai`/`vllm` ignore `OPENAI_*` environment variables;
`vllm` verified against the LunaRoute gateway; `WithBaseURL` includes
`/v1`), **v0.9.0** (openai-go v3; Anthropic thinking tokens reported
as `ReasoningTokens` — see
[ADR-0018](docs/adr/0018-anthropic-thinking-tokens.md); toolloop
preallocation no longer scales with `MaxIterations`), **v0.10.0**
(toolloop `OutcomeDeadline` and `NoIterationLimit` — see
[ADR-0019](docs/adr/0019-toolloop-deadline-and-no-iteration-limit.md);
`OutcomeEmptyResponse` — see
[ADR-0020](docs/adr/0020-toolloop-empty-response.md); adapters omit
`tool_choice` when no tools are offered), **v0.11.0** (Anthropic
thinking blocks round-trip as `ContentReasoning` parts, plus a typed
`llms.ProviderID` — see
[ADR-0021](docs/adr/0021-reasoning-content-part.md); reasoning control via
`ChatRequest.Reasoning` and `ChatResponse.AppliedReasoning` — see
[ADR-0022](docs/adr/0022-reasoning-effort.md); `InputTokens` includes
cached input on every provider — see
[ADR-0023](docs/adr/0023-usage-input-includes-cache.md); per-request
`ProviderParams` — see [ADR-0024](docs/adr/0024-provider-params.md)), and
**v0.12.0** (the generic `bucket` token/concurrency limiter with
`bucket/memory` and shared `bucket/postgres` backends, adapted to the
middleware by `ratelimit.FromBucket` — see
[ADR-0025](docs/adr/0025-bucket-distributed-limiter.md); `bucket/postgres`
needs its schema v1 migration applied before deploying), and **v0.13.0**
(`ErrorKindContextLength` for context-window overflow, plus toolloop
`OutcomeContextExceeded` — see
[ADR-0026](docs/adr/0026-context-length-error-kind.md); pre-v1 API cleanup:
the `vllm` package is renamed `openaicompat` (with `vllm` kept as a
permanent alias), `*auth.Credentials` on both Vertex entry points, and one
`google.Option` for chat and embeddings — see
[ADR-0027](docs/adr/0027-pre-v1-api-cleanup.md); Gemini chat on Vertex AI
— see [ADR-0028](docs/adr/0028-gemini-chat-on-vertex.md); on Vertex,
`WithCredentials` authenticates an injected `WithHTTPClient` — see
[ADR-0029](docs/adr/0029-vertex-injected-client-auth.md)).

MIT — see [`LICENSE`](LICENSE).
