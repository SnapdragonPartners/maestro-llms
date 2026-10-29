# maestro-llms Specification

Status: Accepted, the v1 contract  
Date: 2026-10-29 (first drafted 2026-05-15)  
Module: `github.com/SnapdragonPartners/maestro-llms`

This document specifies what the package does. Three other documents sit
beside it:

- The README section ["Stability contract (v1)"](../README.md#stability-contract-v1)
  is the compatibility promise: which packages and semantics are covered,
  which enum-like types may grow, and what is not covered. The semantics
  specified here are covered by it.
- The [ADR log](adr/README.md) records why each significant decision was
  made and what was rejected. Where an ADR amended or replaced part of the
  original design, the section below states the current rule and links the
  ADR. An accepted ADR is part of the contract; where this document and an
  ADR disagree, the later ADR wins and this document is the one to fix.
- [`MAESTRO_DIVERGENCES.md`](MAESTRO_DIVERGENCES.md) lists every place the
  package intentionally behaves differently from Maestro's original
  implementation.

The v0 drafts of this document, including the original milestones, roadmap
and extraction plan, are in the git history. The README section "Versioning
& license" summarizes what each v0.x release shipped.

## Purpose

`maestro-llms` is a small open source Go toolkit for working with LLM and
embedding providers behind stable application-facing interfaces.

It is shared by applications with deliberately different needs, including:

- **Maestro**, a desktop/local orchestration app with in-process rate limiting.
- **Morris**, a cloud-deployed family-office app with shared/distributed rate
  limiting, operational secrets, audit, and content-classification rules.
- **maestro-cms**, a content service that does budget-aware text chunking for
  embeddings.

The package provides common provider and middleware functionality without
importing product-specific assumptions from any application.

## Goals

1. Provide stable chat/completion and embedding abstractions.
2. Support multiple providers through optional provider packages.
3. Normalize messages, tool calls, tool results, usage metadata, and provider errors.
4. Support middleware composition for retries, timeout, circuit breaking, metrics hooks, validation, and rate limiting.
5. Let applications plug in their own configuration, secret resolution, logging, audit, and rate-limit policy.
6. Keep the core package small enough that it remains useful outside its first consumers.

## Non-Goals

- No Maestro agent concepts. `llms/toolloop` is a tool loop, not an agent
  loop ([ADR-0011](adr/0011-toolloop-helper.md)).
- No Morris tenant/user/content authorization concepts.
- No application database schema. The one schema the package owns is
  `bucket/postgres`'s own limiter state, in a dedicated `bucket` schema
  ([ADR-0025](adr/0025-bucket-distributed-limiter.md)).
- No built-in secret store, and no implicit credential discovery (see
  [Configuration Boundary](#configuration-boundary)).
- No built-in audit taxonomy.
- No application config file format.
- No prompt management system.
- No retrieval/RAG framework.
- No streaming yet. `StreamingChatClient` is a forward declaration, outside
  the v1 contract until streaming is designed
  ([ADR-0003](adr/0003-middleware-complete-only-defer-streaming.md)).

## Normative Clarifications From Engineering Review

The Maestro implementation was the tested reference for tool calls across
Anthropic, OpenAI, Google, and Ollama, and the extraction used it to resolve
provider-specific behavior while removing Maestro-specific orchestration,
config, and logging assumptions.

The following decisions are normative:

- Tool-call round trips must be unambiguous and round-trippable.
- Multi-tool-call assistant turns must be unambiguous and round-trippable.
- System instructions live in a dedicated `ChatRequest.System` field, not arbitrary mid-list messages.
- System instructions are text-only.
- `Temperature` must distinguish unset from intentional `0.0`.
- Streaming support is a separate optional interface, not a method added to `ChatClient`.
- Local limiter rejections are distinct from provider `429` errors.
- Limiter release must work after request context cancellation.
- Retry middleware wraps rate-limit middleware by default so each provider attempt gets its own reservation.
- Provider clients, middleware wrappers, fakes, and in-memory limiters are safe for concurrent use unless explicitly documented otherwise.
- Unknown model names must not fail solely because the package does not know them.

## Package Shape

```text
github.com/SnapdragonPartners/maestro-llms
  llms/                     core interfaces and shared types
  llms/middleware/          provider-neutral middleware, chain helpers, Recommended*
  llms/ratelimit/           Limiter/Reservation interfaces, in-memory limiter, FromBucket
  llms/toolloop/            synchronous tool-loop helper
  llms/toolloop/compact/    ready-made compaction policies for the tool loop
  llms/testllm/             deterministic fakes
  llms/providers/
    anthropic/              Anthropic Messages API (chat)
    anthropic/anthropicvertex/  Anthropic on Vertex AI (chat)
    openai/                 OpenAI Responses API (chat) and embeddings
    google/                 Gemini chat and embeddings, Gemini API or Vertex AI
    ollama/                 Ollama /api/chat (chat), no SDK
    openaicompat/           any OpenAI Chat Completions server (chat)
    vllm/                   deprecated alias of openaicompat
  bucket/                   generic per-key token/concurrency limiter (no llms import)
  bucket/memory/            in-process backend
  bucket/postgres/          shared PostgreSQL backend
```

Provider packages are leaf imports: an application imports only the
providers it uses, and the core `llms` package pulls in no provider SDK. A
Vertex backend that would add a dependency lives in its own leaf package, as
`anthropicvertex` does ([ADR-0009](adr/0009-vertex-backend-psc-and-gemini-embeddings.md)).

## Core Concepts

### Provider

A provider is a wire protocol and API family, identified by
`llms.ProviderID`:

- `anthropic` (including Anthropic on Vertex AI)
- `openai`
- `google` (the Gemini API and Gemini on Vertex AI)
- `ollama`
- `openaicompat` (any server speaking OpenAI Chat Completions, such as vLLM)

`ProviderID` names the adapter, not the deployment
([ADR-0021](adr/0021-reasoning-content-part.md)). It is an open set that may
grow; the core compares it only for equality.

### Model

A model is a provider-specific model identifier, such as:

- `claude-sonnet-...`
- `gpt-...`
- `text-embedding-...`
- `gemini-...`

Applications pass model names through unchanged. Unknown or new model names
are never rejected because the package does not know them.

### Purpose

Applications can label a call purpose without the package needing to
understand app semantics:

```go
type Purpose string

const (
    PurposeChat           Purpose = "chat"
    PurposeEmbedding      Purpose = "embedding"
    PurposeClassification Purpose = "classification"
    PurposeSummarization  Purpose = "summarization"
)
```

Applications may pass custom purposes.

`Purpose` is application intent. It answers "why is this call happening?" Examples: chat, embedding, classification, summarization, title generation, ingestion inspection.

`Operation` is transport shape for middleware/limiting. It answers "what provider API class is being used?" Examples: chat, embedding. `Operation` may be derived by middleware from the client/request type; it remains explicit in reservation requests so application-provided distributed limiters do not need to infer it.

## Chat API

`ChatClient` is synchronous.

```go
type ChatClient interface {
    Complete(ctx context.Context, req ChatRequest) (ChatResponse, error)
    Model() ModelRef
}
```

Streaming, when it is designed, is a separate optional capability:

```go
type StreamingChatClient interface {
    Stream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error)
}
```

Callers and middleware discover streaming with a type assertion, because
adding `Stream` to `ChatClient` would break every provider, fake, and
middleware. No adapter implements it yet, and `StreamingChatClient` and
`StreamChunk` are outside the v1 contract until streaming is designed
([ADR-0003](adr/0003-middleware-complete-only-defer-streaming.md)).

Model listing is an additional optional capability ([ADR-0012](adr/0012-model-lister.md)):

```go
type ModelLister interface {
    ListModels(ctx context.Context) ([]ModelInfo, error)
}

type ModelInfo struct {
    ID      string    // provider's model identifier
    Family  string    // provider-classified family ("" if N/A or unparseable)
    Created time.Time // zero if the SDK doesn't expose it
    Raw     any       // SDK-specific payload, outside the stability contract
}
```

Discovery is by type assertion on a `ChatClient`. Providers without a list API simply don't implement it. The exception is `anthropicvertex`: it returns the base `*anthropic.Client`, which satisfies `ModelLister`, but Vertex AI doesn't serve the models endpoint, so `ListModels` fails with a `bad_request` `ProviderError`. Per-family upgrade detection (`LatestInFamily`) is a provider-package concern — family naming differs per provider (Anthropic `claude-{opus|sonnet|haiku}`, OpenAI dated-snapshot stripping, Gemini `gemini-{pro|flash|nano|ultra}`) — and is offered as a pure helper plus a one-shot convenience method on the provider's chat client. Ollama and `openaicompat` implement `ModelLister` only: their lists are locally available models, and "latest in family" has no consistent meaning for community or HuggingFace-style identifiers. See ADR-0012 for binding non-goals.

### Model Reference

```go
type ModelRef struct {
    Provider ProviderID
    Name     string
}

// ProviderID names a provider adapter (wire protocol, not deployment:
// Anthropic on Vertex is ProviderAnthropic). Open set; the core compares it
// only for equality. See ADR-0021.
type ProviderID string

const (
    ProviderAnthropic    ProviderID = "anthropic"
    ProviderOpenAI       ProviderID = "openai"
    ProviderGoogle       ProviderID = "google"
    ProviderOllama       ProviderID = "ollama"
    ProviderOpenAICompat ProviderID = "openaicompat"
    ProviderVLLM         = ProviderOpenAICompat // Deprecated alias (ADR-0027)
)
```

`ModelRef` is promised to stay comparable, so it can be a map key.

### Chat Request

```go
type ChatRequest struct {
    System      []ContentPart
    Messages    []Message
    Tools       []ToolDefinition
    ToolChoice  ToolChoice
    Purpose     Purpose
    MaxTokens   int
    Temperature *float32
    Metadata    map[string]string
    Reasoning   *ReasoningConfig
    // Raw provider body fields, deep-merged into the wire request after
    // mapping (objects merge, anything else replaces). ADR-0024.
    ProviderParams map[ProviderID]map[string]any
}

type ReasoningEffort string // none, minimal, low, medium, high, xhigh, max

type ReasoningConfig struct {
    Effort       ReasoningEffort // zero: not set
    BudgetTokens int             // <= 0: not set
}
```

`Reasoning` asks a reasoning model to bound its thinking. Each adapter maps
each field 1:1 to its provider's control, or omits it when the provider has
none for that value, so the model default applies. A local failure for an
unsupported value is never returned: it can't be retried and the compiler
can't catch it. Adapters don't predict which values a particular model
accepts, so a value the provider supports but the model rejects comes back as
the provider's error. `ChatResponse.AppliedReasoning` reports what was sent.
See ADR-0022 for the per-provider tables.

`Metadata` is app-supplied, provider-neutral context for middleware and logging. It must not be sent to providers unless a middleware or provider explicitly chooses to use a key.

`ProviderParams` is the per-request escape hatch for provider body fields the neutral request doesn't model. Each adapter reads only its own `ProviderID`'s entry and deep-merges it into the wire request after mapping: objects merge, anything else replaces, so an entry can override a mapped field. Keys and values pass through as the provider defines them and are outside the stability contract. See [ADR-0024](adr/0024-provider-params.md).

`System` carries system/developer instructions and is normalized by each provider adapter. Anthropic receives it as the top-level system parameter; OpenAI and `openaicompat` receive it in the provider-appropriate system/developer slot; Google and Ollama adapters translate it according to their supported request shape.

System instructions are not legal as arbitrary mid-conversation `Messages`. Multiple `System` parts are legal and preserve order; providers may join them when their API requires one string.

`System` parts must be text-only. Validation middleware (with a `ValidationError`) and provider adapters (with a `bad_request` `ProviderError`) reject any other part type in the system field (`tool_call`, `tool_result`, `reasoning`, or a future multimodal type), unless this specification is deliberately extended.

`Temperature == nil` means "use provider/model default." `Temperature != nil && *Temperature == 0` means intentionally deterministic.

### Messages

```go
type Role string

const (
    RoleUser      Role = "user"
    RoleAssistant Role = "assistant"
    RoleTool      Role = "tool"
)

type Message struct {
    Role    Role
    Content []ContentPart
}

type ContentPart struct {
    Type            ContentPartType
    Text            string
    ToolCall        *ToolCall
    ToolResult      *ToolResult
    Reasoning       *Reasoning
    CacheBreakpoint bool // optional prompt-cache hint (see below)
}

type ContentPartType string

const (
    ContentText       ContentPartType = "text"
    ContentToolCall   ContentPartType = "tool_call"
    ContentToolResult ContentPartType = "tool_result"
    ContentReasoning  ContentPartType = "reasoning"
)

type Reasoning struct {
    Provider     ProviderID // adapter that produced it; only that adapter replays it
    Text         string     // readable reasoning/summary; display only, never sent back
    ProviderData []byte     // opaque, provider-owned; sent back unchanged
}
```

Tool calls and tool results are content parts, not side-channel fields on `Message`. This keeps the conversation round-trip explicit:

- assistant messages can contain `tool_call` parts
- tool-result messages use `RoleTool` and contain `tool_result` parts
- providers that require a different wire shape translate at the provider boundary

`CacheBreakpoint` is an optional, provider-neutral prompt-cache hint:
everything up to and including the marked part *may* be prompt-cached. It is
purely advisory — setting or ignoring it never changes model output, only
cache economics — and is not a Maestro-shaped `CacheControl` (no TTL/policy
knobs; see ADR-0008). Anthropic honors it (maps to `cache_control: ephemeral`
on the block); OpenAI prefix-caches automatically (no-op); Gemini's explicit
caching is a separate cached-content API the inline hint does not drive;
Ollama has none. Provider adapters must not error on it.

`reasoning` parts carry provider reasoning state that must go back to the
provider that produced it (Anthropic `thinking` / `redacted_thinking`
blocks). They appear only on assistant messages, in the position the provider
returned them. Every adapter drops reasoning parts from other providers before
translating (`ReasoningFrom`), so a transcript can move between providers. A
reasoning-only assistant turn is not a reply and is rejected like an empty
one. Reasoning is bound to the conversation before it: an app that edits
earlier turns must remove all reasoning first (`WithoutReasoning`). See
ADR-0021.

Provider mapping:

- Anthropic: `RoleTool` / `tool_result` parts are encoded as user-role tool-result content blocks because Anthropic models tool results as user messages.
- OpenAI: `RoleTool` maps to tool-role messages.
- OpenAI Chat Completions (`openaicompat`): `RoleTool` maps to tool-role messages.
- Google/Ollama: provider adapters use the Maestro-tested tool-call mapping for that provider.

The top-level conversation model remains one app-neutral representation. The content shape also allows later multimodal expansion without changing the top-level request contract.

### Tools

```go
type ToolDefinition struct {
    Name        string
    Description string
    InputSchema json.RawMessage
}

type ToolChoice struct {
    Type ToolChoiceType
    Name string // set when Type == ToolChoiceTool
}

type ToolChoiceType string

const (
    ToolChoiceAuto     ToolChoiceType = "auto"
    ToolChoiceNone     ToolChoiceType = "none"
    ToolChoiceRequired ToolChoiceType = "required"
    ToolChoiceTool     ToolChoiceType = "tool"
)
```

`ToolChoiceAuto` lets the model decide; `ToolChoiceNone` forbids tool calls;
`ToolChoiceRequired` forces the model to call at least one of the offered
tools but lets it pick which; `ToolChoiceTool` forces a specific named tool
(`Name` required). `Required` maps to Anthropic `any`, OpenAI `required`,
Gemini ANY-mode ([ADR-0007](adr/0007-toolchoice-required.md)). Ollama has no `tool_choice`, so a `Required` (or `Tool`)
choice is best-effort there — tools are offered, the model decides — and
adapters must not silently lose caller intent (see MAESTRO_DIVERGENCES).
With no tools offered, adapters omit `tool_choice` from the wire request
(divergence X7).

```go
type ToolCall struct {
    ID                string
    Name              string
    Parameters        json.RawMessage
    ProviderSignature []byte // opaque, provider-owned; round-trip unchanged
}

type ToolResult struct {
    ToolCallID string
    Content    string
    IsError    bool
}
```

Tool schemas should remain raw JSON Schema so the package does not force a schema-generation library.

`ToolCall.ProviderSignature` is an opaque, provider-owned blob the core never
interprets. It carries provider-required per-tool-call state that must survive
the stateless response→app→request round-trip (the app already replays the
assistant turn in history) without a per-client cache — e.g. Gemini 3's
mandatory functionCall `thought_signature`. Adapters that don't need it leave
it nil and ignore it, exactly like `ContentPart.CacheBreakpoint`. See
ADR-0010.

`ToolCall.Parameters` is `json.RawMessage` to preserve exact provider-emitted JSON and avoid lossy `map[string]any` round-trips. Applications can unmarshal into typed structs. The one exception is a provider SDK that decodes args itself: the genai SDK rounds Gemini tool-call args through float64 in both directions, so integers beyond 2^53 lose precision on the google adapter (divergence G5).

`ToolResult.Content` is a string. This intentionally limits tool results to textual/JSON-string content. Structured or multimodal tool results, if added, extend `ToolResult` with new fields without changing the core round-trip semantics.

### Worked Tool Exchange

Initial request:

```go
req := llms.ChatRequest{
    System: []llms.ContentPart{llms.Text("You may call lookup_person.")},
    Messages: []llms.Message{
        llms.UserText("How am I related to Daniel Ratner?"),
    },
    Tools: []llms.ToolDefinition{lookupPersonTool},
}
```

Assistant response asks for a tool (`resp.Message` has this shape; `Message`
holds a slice, so it is not comparable with `==`):

```go
// resp.Message:
llms.Message{
    Role: llms.RoleAssistant,
    Content: []llms.ContentPart{
        {
            Type: llms.ContentToolCall,
            ToolCall: &llms.ToolCall{
                ID: "toolu_123",
                Name: "lookup_person",
                Parameters: json.RawMessage(`{"query":"Daniel Ratner"}`),
            },
        },
    },
}
```

Caller executes the tool and sends the result back:

```go
nextReq := llms.ChatRequest{
    System: req.System,
    Messages: append(req.Messages,
        resp.Message,
        llms.Message{
            Role: llms.RoleTool,
            Content: []llms.ContentPart{
                {
                    Type: llms.ContentToolResult,
                    ToolResult: &llms.ToolResult{
                        ToolCallID: "toolu_123",
                        Content: `{"matches":[{"id":"1585","name":"Daniel Ratner"}]}`,
                    },
                },
            },
        },
    ),
    Tools: req.Tools,
}
```

Provider adapters translate this app-neutral sequence to their provider wire format. The final `Complete` call returns the assistant's answer as a normal assistant text message.

### Multi-Tool-Call Turns

Assistant messages may contain multiple `tool_call` parts in one turn. Callers should execute all tool calls requested by that assistant message and then append one `RoleTool` message containing one `tool_result` part per tool call:

```go
toolMsg := llms.Message{
    Role: llms.RoleTool,
    Content: []llms.ContentPart{
        {
            Type: llms.ContentToolResult,
            ToolResult: &llms.ToolResult{
                ToolCallID: "call_a",
                Content: `{"ok":true}`,
            },
        },
        {
            Type: llms.ContentToolResult,
            ToolResult: &llms.ToolResult{
                ToolCallID: "call_b",
                Content: `{"ok":true}`,
            },
        },
    },
}
```

This is the app-neutral representation. Provider adapters split or merge as required:

- Anthropic: encode all tool-result parts as tool-result content blocks in one user-role message.
- OpenAI: split a multi-result `RoleTool` message into one tool-role wire message per `tool_call_id`.
- `openaicompat`: likewise one tool-role wire message per `tool_call_id`.
- Google/Ollama: use the Maestro-tested mapping for that provider.

Every tool call in an assistant turn must receive a corresponding tool result before the next provider call. Validation middleware rejects missing, duplicate, or orphaned tool results ([ADR-0006](adr/0006-validation-error.md)).

### Chat Response

```go
type ChatResponse struct {
    Message    Message
    Text       string
    ToolCalls  []ToolCall
    StopReason StopReason
    Usage      Usage
    // AppliedReasoning is the reasoning control the adapter sent, in neutral
    // terms (only mapped fields set); nil when nothing was sent. ADR-0022.
    AppliedReasoning *ReasoningConfig
    Raw        any
}

type StopReason string
```

`Raw` is optional provider-specific data for advanced callers. Middleware does not depend on it.

`StopReason` values are the provider's own strings; the core package does not normalize them.

`Text` and `ToolCalls` are convenience mirrors derived from `Message`. Providers must populate `Message` as the source of truth. If mirrors are present, tests should assert that they match `Message`. Helper methods may be added later so callers can avoid relying on duplicated fields.

`Raw` is outside the stability contract. Applications that depend on it accept provider-package breakage risk.

## Embeddings API

```go
type EmbeddingClient interface {
    Embed(ctx context.Context, req EmbeddingRequest) (EmbeddingResponse, error)
    Model() ModelRef
    DefaultDimensions() int
}
```

### Embedding Request

```go
type EmbeddingRequest struct {
    Inputs     []EmbeddingInput
    Purpose    Purpose
    Dimensions int
    Task       EmbeddingTask // advisory; honored where supported, else ignored
    Metadata   map[string]string
}

type EmbeddingInput struct {
    ID    string
    Text  string
    Title string // optional; only meaningful with EmbeddingTaskRetrievalDocument
}

// EmbeddingTask is a provider-neutral, advisory embedding-intent hint.
// Constants: Unspecified (zero), RetrievalDocument, RetrievalQuery,
// SemanticSimilarity, Classification, Clustering, QuestionAnswering,
// FactVerification, CodeRetrievalQuery.
type EmbeddingTask string
```

### Embedding Response

```go
type EmbeddingResponse struct {
    Vectors []EmbeddingVector
    Usage   Usage
    Raw     any
}

type EmbeddingVector struct {
    ID     string
    Values []float32
}
```

The response must preserve input ordering. IDs are included so callers can defensively match responses to inputs.

`DefaultDimensions` is advisory. Some providers/models support caller-selected dimensions, so `EmbeddingRequest.Dimensions` may override the model default when the provider supports it. The true dimension is always the length of each returned vector.

Provider clients return a typed validation/config error when `Inputs` exceeds the provider or model batch limit. Automatic chunking is intentionally outside provider clients; applications own chunking because they also own retry policy, progress reporting, source IDs, and ingestion transaction boundaries.

### Task-typed embeddings, Vertex, single-input models

These rules come from [ADR-0009](adr/0009-vertex-backend-psc-and-gemini-embeddings.md),
as amended by ADR-0027, ADR-0028 and ADR-0029.

`Task`/`Title` (shown in the types above) are provider-neutral and advisory,
honored only where the provider supports them (Gemini retrieval-document/-query
etc.) and ignored elsewhere (OpenAI). They are *not* app context smuggled
through `Metadata` — task type materially changes the vectors for retrieval.

`gemini-embedding-001` accepts **one input per call**: this is simply the
"batch limit" rule above with a limit of 1 — the client returns a typed
`bad_request`/validation error when `len(Inputs) > 1`. It must **not**
internally fan out or otherwise make a hidden chunking exception; applications
set their batch size from model metadata/config and keep ownership of
chunking.

Silent truncation is a retrieval-quality and auditability hazard. genai
cannot send `autoTruncate:false` (its field is `bool,omitempty`), so the
Google embeddings client does **not** rely on the Vertex flag for safety: it
enforces no-silent-truncation with a client-side `MaxInputBytes` guard
(literal UTF-8 bytes). Fail-closed — `AutoTruncate=true` opts into Vertex
truncation (Vertex-only); `AutoTruncate=false` requires `MaxInputBytes>0` or
construction fails, so the API can never *look* safe while Vertex silently
truncates (ADR-0009 implementation refinement).

Anthropic access via **Vertex AI** is a separate-package backend
(`anthropic/anthropicvertex`) plus a low-level `anthropic.WithRequestOptions`
escape hatch, preserving leaf imports. Gemini chat and embeddings reach
Vertex AI through the `google` package itself (`WithVertex` +
`WithCredentials`), with one backend selector shared by `New` and
`NewEmbeddings` ([ADR-0028](adr/0028-gemini-chat-on-vertex.md)).
Credentials and the endpoint (for PSC) are **app-supplied** — the package
exposes endpoint/base-URL override and HTTP-client/transport injection; it
performs no ADC discovery. Networking (PSC, DNS, VPC-SC, IAM) is the
application's infrastructure concern, not this package's. See ADR-0009.

Both Vertex entry points take the same credential type,
`*cloud.google.com/go/auth.Credentials` (ADR-0027); `anthropicvertex`
converts it internally. The Google chat and embeddings constructors share
one functional-option type, and an option a constructor does not accept is
a config error, never ignored. On Vertex, `WithCredentials` authenticates an
injected `WithHTTPClient` on a copy; the application's client and
credentials are never mutated ([ADR-0029](adr/0029-vertex-injected-client-auth.md)).

## Usage Metadata

Usage is normalized across chat and embeddings.

```go
type Usage struct {
    InputTokens          int
    OutputTokens         int
    TotalTokens          int
    ReasoningTokens      int
    BillableOutputTokens int
    EmbeddingTokens      int
    CacheReadTokens      int
    CacheWriteTokens     int
    ProviderRequestID    string
}
```

If a provider does not return a field, it is left zero. Middleware can estimate tokens before a call; provider clients fill actual usage when available. `Usage` is promised to stay comparable.

`OutputTokens` is the visible assistant output only. `ReasoningTokens` is the separately metered thinking output of reasoning models, which does not appear in the response; it is zero where the provider does not expose the split. `BillableOutputTokens` is what the provider charges as output, `OutputTokens + ReasoningTokens` on every adapter. See [ADR-0016](adr/0016-usage-reasoning-tokens.md) and, for Anthropic's `output_tokens_details.thinking_tokens`, [ADR-0018](adr/0018-anthropic-thinking-tokens.md).

`CacheReadTokens` and `CacheWriteTokens` cover provider prompt-cache accounting: Anthropic cache reads and creation, OpenAI `cached_tokens` / `cache_write_tokens`, and Gemini `cachedContentTokenCount`. Providers that do not expose cache token accounting should leave these fields zero.

`InputTokens` is all input the provider processed, **including** cached tokens, on every provider. `CacheReadTokens` and `CacheWriteTokens` are subsets of it, so `InputTokens − CacheReadTokens − CacheWriteTokens` is the uncached remainder and is never negative (`llms.CacheSplit` clamps inconsistent provider counts). Anthropic's wire `input_tokens` excludes cache reads and writes, so its adapter adds them. See [ADR-0023](adr/0023-usage-input-includes-cache.md).

## Errors

Provider errors are classified.

```go
type ErrorKind string

const (
    ErrorKindConfig        ErrorKind = "config"
    ErrorKindAuth          ErrorKind = "auth"
    ErrorKindRateLimited   ErrorKind = "rate_limited"
    ErrorKindTimeout       ErrorKind = "timeout"
    ErrorKindUnavailable   ErrorKind = "unavailable"
    ErrorKindBadRequest    ErrorKind = "bad_request"
    ErrorKindContentPolicy ErrorKind = "content_policy"
    ErrorKindContextLength ErrorKind = "context_length"
    ErrorKindUnknown       ErrorKind = "unknown"
)

type ProviderError struct {
    Provider   ProviderID
    Model      string
    Kind       ErrorKind
    StatusCode int
    Message    string
    RetryAfter time.Duration
    Cause      error
}
```

Applications use `errors.As` to inspect `ProviderError`. `Cause` holds the underlying SDK or transport error; its dynamic type is outside the stability contract.

`ErrorKind` may grow in minor releases ([ADR-0032](adr/0032-enum-like-types-v1.md)), so a switch over it needs a `default` case.

Retry guidance:

- `rate_limited`, `timeout`, and `unavailable` are normally retryable.
- `auth`, `config`, `bad_request`, `content_policy`, and `context_length` are normally not retryable.
- `context_length` means the input does not fit the model's context window; the caller must shorten the conversation before retrying. Adapters report it only for a deterministic provider signal and otherwise leave the error `bad_request` (ADR-0026).
- `RetryAfter` captures provider `Retry-After` headers when available.

`llms.Retryable(err)` is the single retry classifier, and `llms.RetryAfter(err)` the matching backoff hint; both understand `*ProviderError` and `*LimitError`, and treat any other error as not retryable with no hint ([ADR-0004](adr/0004-retry-circuit-reuse-llms-retryable.md)).

Local limiter rejections are not represented as provider errors. They use a separate error type:

```go
type LimitError struct {
    Cause      error // context error when the limiter's context ended the wait
    Provider   ProviderID
    Model      string
    Reason     string // diagnostic text, not covered by the contract
    RetryAfter time.Duration
    Permanent  bool   // not admissible under the limits in force
}
```

This lets retry/circuit middleware distinguish "we throttled locally before making a provider call" from "the provider returned 429." A `LimitError` is never evidence that the provider is unhealthy, so the circuit breaker treats it as neutral. It is retryable unless it is `Permanent` or its `Cause` is a cancellation; a deadline `Cause` stays retryable, because the deadline may be a per-attempt one ([ADR-0033](adr/0033-limit-error-classification.md)). `bucket.LimitError` carries the same `Permanent` and `Cause`, and `ratelimit.FromBucket` keeps both.

The `middleware` package adds two more error types, both non-retryable: `middleware.ValidationError` for a structurally invalid request ([ADR-0006](adr/0006-validation-error.md)) and `middleware.CircuitOpenError` for a fast-fail while the circuit is open ([ADR-0005](adr/0005-circuit-open-error.md)).

## Middleware

Middleware composes separately for chat and embeddings.

```go
// package middleware
type ChatMiddleware func(llms.ChatClient) llms.ChatClient
type EmbeddingMiddleware func(llms.EmbeddingClient) llms.EmbeddingClient

func ChainChat(base llms.ChatClient, mws ...ChatMiddleware) llms.ChatClient
func ChainEmbeddings(base llms.EmbeddingClient, mws ...EmbeddingMiddleware) llms.EmbeddingClient
```

The first middleware argument is outermost:

```go
ChainChat(base, A, B, C)
// call order: A -> B -> C -> base
```

The package ships these middleware, each as a chat and (except validation) an embedding variant:

- `ValidationChat`: structural checks only ([ADR-0006](adr/0006-validation-error.md))
- `RetryChat` / `RetryEmbeddings`: classified by `llms.Retryable` ([ADR-0004](adr/0004-retry-circuit-reuse-llms-retryable.md))
- `TimeoutChat` / `TimeoutEmbeddings`
- `CircuitChat` / `CircuitEmbeddings` ([ADR-0005](adr/0005-circuit-open-error.md))
- `RateLimitChat` / `RateLimitEmbeddings`, over a `ratelimit.Limiter` and a `TokenEstimator`
- `MetricsChat` / `MetricsEmbeddings`, over an `Observer`

Every middleware wraps `Complete` and `Embed` only. A wrapper does not forward the optional `StreamingChatClient` capability, so it is undiscoverable through a chain; streaming-aware middleware needs a superseding ADR ([ADR-0003](adr/0003-middleware-complete-only-defer-streaming.md)).

Logging and metrics use narrow callback interfaces, not a specific logging package. `Observer` receives one `Event` per call the metrics middleware wraps, carrying provider-neutral facts and no request or response content. An `Observer` that also implements the optional `ContextObserver` receives each `Event` with that call's context instead, and `AttemptFromContext` reads the retry attempt number the retry middleware puts there ([ADR-0031](adr/0031-attempt-context-observer.md)).

Recommended order for most apps:

1. request validation
2. retry
3. per-attempt timeout
4. circuit breaker
5. local/shared rate limit reservation
6. metrics/logging hooks
7. provider client

With this default, each retry attempt flows through timeout, circuit breaker, and rate-limit reservation independently. That is intentional: retries are real provider traffic and should be gated by the same limiter as first attempts.

`RecommendedChat` and `RecommendedEmbeddings` build exactly this order from a `RecommendedConfig`: validation (chat only; there is no structural embedding validation), retry and circuit always; timeout, rate limiting and metrics only when configured. A nil `RecommendedConfig.Retry` selects the jittered `DefaultRetryConfig()` ([ADR-0036](adr/0036-recommended-retry-pointer.md)). Because metrics is innermost, its `Observer` sees each real provider attempt but not outer rejections; an application that also wants those adds a second, outer `MetricsChat` with `ChainChat`.

Applications may change the order with `ChainChat`/`ChainEmbeddings`. The tradeoffs:

- Retry outside rate limiting means each provider attempt consumes a reservation.
- Rate limiting outside retry means one reservation covers all attempts and can undercount provider traffic.
- Timeout outside retry creates one total deadline for all attempts.
- Timeout inside retry creates one timeout budget per attempt, which is the default recommendation here.

## Rate Limiting

The package defines the rate-limit interfaces, an in-memory implementation for local/desktop apps, and a shared implementation for multi-instance apps. Applications can also plug in their own.

### Design Requirement

The rate limiter must not assume process-local state is authoritative.

Maestro can use an in-memory limiter. Morris needs a PostgreSQL-backed or otherwise shared limiter because Cloud Run can run multiple instances. The same middleware works with both.

### Reservation Interface

```go
type Limiter interface {
    Reserve(ctx context.Context, req ReservationRequest) (Reservation, error)
}

type ReservationRequest struct {
    Provider       llms.ProviderID
    Model          string
    Operation      Operation
    Purpose        llms.Purpose
    Metadata       map[string]string
    Subject        Subject
    EstimatedUnits UsageUnits
}

type Operation string

const (
    OperationChat      Operation = "chat"
    OperationEmbedding Operation = "embedding"
)

type UsageUnits struct {
    InputTokens     int
    OutputTokensMax int
    TotalTokensMax  int
    Requests        int
}

type Subject struct {
    TenantID string
    UserID   string
    JobID    string
}

type Reservation interface {
    Commit(ctx context.Context, usage llms.Usage) error
    Release(ctx context.Context) error
}
```

Semantics:

- `Reserve` is called before the provider request.
- `Commit` records actual usage when the provider call succeeds or returns usage.
- `Release` frees any leased concurrency/request slot.
- Middleware calls `Commit` when actual usage is available (any usage that reports a token field) and always calls `Release`.
- Implementations must make repeated `Release` safe.
- Implementations must make `Release` safe after `Commit`.
- Middleware calls `Release` with a context that survives request cancellation, such as `context.WithoutCancel(ctx)` or a bounded background context.

Reconciliation contract:

- `Reserve` consumes or holds the estimated usage according to the limiter implementation.
- `Commit` records actual usage.
- If actual usage is lower than estimated, implementations may refund unused units when they can do so safely.
- If actual usage is higher than estimated, implementations should charge the delta if the backend supports it.
- A failed delta charge must not erase the fact that the provider call already happened; implementations should return an error so the application can audit/account for the mismatch.
- Concurrency leases are released by `Release`, not by `Commit`.
- The toolkit's limiters charge `max(InputTokens, EmbeddingTokens)` plus `BillableOutputTokens` (or `OutputTokens + ReasoningTokens` when it is unset), with `TotalTokens`, a provider-specific aggregate, as a floor, using saturating integer arithmetic ([ADR-0034](adr/0034-limiter-config-and-charging.md)).

In-memory limiter (`ratelimit.NewInMemoryLimiter`):

- appropriate for Maestro/local tests
- token bucket and concurrency semaphore
- configuration is validated: `NewInMemoryLimiter` returns an error for limits `bucket.Limits.Validate` rejects ([ADR-0034](adr/0034-limiter-config-and-charging.md))
- implements the optional stats interface

Stats is a separate optional interface:

```go
type LimiterStats interface {
    Stats(ctx context.Context) (LimiterSnapshot, error)
}
```

Consumers discover it with a type assertion. Core `Limiter` stays minimal so distributed implementations are not forced to support stats before they need them.

Shared limiter ([ADR-0025](adr/0025-bucket-distributed-limiter.md), which replaced the original plan for Morris to build its own first):

- The generic top-level `bucket` package, which does not import `llms`: `Acquire(ctx, key, tokens)` returns a `Lease` with `Adjust`, `Release` and `Lost`. Backends are `bucket/memory` (in process) and `bucket/postgres` (shared across instances).
- `ratelimit.FromBucket(l, key)` adapts any `bucket.Limiter` to `Limiter`, so the middleware is unchanged. `key` is the application's policy for mapping a request to a bucket key. A bucket lease is exactly one concurrency slot.
- `bucket/postgres` keeps limits in the database (`Ensure`), holds concurrency slots as renewed lease rows, treats `MaxWait` as a hard deadline, and fails closed. Its tables live in a dedicated `bucket` schema with versioned migrations; a database behind `postgres.SchemaVersion` gets a `*postgres.SchemaError` rather than wrong behavior.
- PostgreSQL only for now; other SQL dialects or Redis would be further `bucket/<backend>` packages behind the same interface.

## Token Estimation

The rate-limit middleware sizes each reservation with a `middleware.TokenEstimator`:

```go
type TokenEstimator interface {
    EstimateChat(req llms.ChatRequest) ratelimit.UsageUnits
    EstimateEmbeddings(req llms.EmbeddingRequest) ratelimit.UsageUnits
}
```

`DefaultEstimator` is approximate and character-based. Only its bias toward over-reserving is promised, not its exact numbers: overestimation is safer for rate limiting than underestimation. Provider-specific estimators may be added.

A separate, exported text-level helper is available at the core level for consumers that need to estimate the token count of a standalone string (budget-aware chunking, ad-hoc inspection) without going through the request-shaped interface (ADR-0013):

```go
func EstimateTextTokens(s string) int
```

Bias is intentionally **neutral** (rune count divided by 4, rounded up; the formula is covered by the contract because callers persist chunk boundaries built on it) — distinct from the request-shaped estimator's high bias — because over-estimating during chunking produces smaller-than-necessary chunks and thus more downstream API calls. The two estimators serve different purposes and should not be consolidated.

## Provider Clients

Every provider package exposes `New(opts ...Option)`, which returns its chat
client and an error; an embeddings-capable package also exposes
`NewEmbeddings(opts ...Option)`. Missing or invalid configuration is a
`config` `ProviderError` from the constructor. Where chat and embeddings
share one option type (`openai`, `google`), an option the constructor does
not accept is also a config error, never ignored
([ADR-0027](adr/0027-pre-v1-api-cleanup.md), [ADR-0035](adr/0035-provider-naming.md)).
Every chat client supports tool calls, usage metadata, error
classification, and an injectable `*http.Client`.

### Anthropic

`llms/providers/anthropic`: chat over the Anthropic Messages API.
`WithRequestOptions` is the low-level escape hatch that takes anthropic-sdk-go
request options. Thinking blocks round-trip as `reasoning` parts
([ADR-0021](adr/0021-reasoning-content-part.md)), and `CacheBreakpoint` maps to
`cache_control: ephemeral` ([ADR-0008](adr/0008-prompt-cache-hint.md)).
No embeddings.

`llms/providers/anthropic/anthropicvertex`: the same chat client on Vertex AI.
`New` requires `WithVertex(project, location)`, because both appear in the
Vertex request path, and app-supplied `WithCredentials`. `WithBaseURL` is an
optional endpoint override (for example a Private Service Connect endpoint),
not an alternative to `WithVertex`. `New` returns `*anthropic.Client`
([ADR-0009](adr/0009-vertex-backend-psc-and-gemini-embeddings.md),
[ADR-0035](adr/0035-provider-naming.md)).

### OpenAI

`llms/providers/openai`: `New` is chat via the OpenAI **Responses API**, not
Chat Completions (OpenAI is deprecating Chat Completions, and Maestro's
tested client already used Responses). `NewEmbeddings` is the embeddings
client; `WithDimensions` is embeddings-only, and `New` rejects it
([ADR-0035](adr/0035-provider-naming.md)).

### Google

`llms/providers/google`: Gemini chat (`New`) and embeddings
(`NewEmbeddings`), extracted from Maestro's tested `google.golang.org/genai`
implementation. Both run on the Gemini API with an API key or on Vertex AI
with `WithVertex` + `WithCredentials`, through one shared backend selector
([ADR-0028](adr/0028-gemini-chat-on-vertex.md)). Gemini 3's
`thought_signature` round-trips through `ToolCall.ProviderSignature`
([ADR-0010](adr/0010-toolcall-provider-signature.md)).

### Ollama

`llms/providers/ollama`: chat for local models over Ollama's `/api/chat`
JSON endpoint. It is a hand-rolled `net/http` client rather than the
`github.com/ollama/ollama` SDK, whose server-side advisories would otherwise
be attributed to every consumer ([ADR-0002](adr/0002-ollama-no-sdk-hand-rolled-client.md)).
No API key.

### OpenAI-compatible

`llms/providers/openaicompat`: chat for any server or gateway that speaks the
OpenAI Chat Completions endpoint (`/v1/chat/completions`), such as self-hosted
vLLM or a hosted router, NOT the Responses API used by the sibling `openai`
package. It uses the openai-go SDK pointed at a required base URL that
includes the API version path (for example `http://localhost:8000/v1`). The API key is optional (vLLM's default deployment
has no auth). Implements `ModelLister` but not `LatestInFamily`; on vLLM,
`ModelInfo.Created` is the model load time on the instance, not the upstream
release date ([ADR-0015](adr/0015-vllm-provider.md)).

`llms/providers/vllm` is a deprecated alias of `openaicompat`, kept for the
life of v1 and in export parity with it ([ADR-0027](adr/0027-pre-v1-api-cleanup.md)).

## Configuration Boundary

The package accepts already-resolved configuration. There is no shared
config struct: each provider package takes functional options.

```go
client, err := anthropic.New(
    anthropic.WithAPIKey(apiKey),
    anthropic.WithModel(model),
    anthropic.WithHTTPClient(httpClient),
)
```

The package does not know:

- where the API key came from
- whether it was read from env, Secret Manager, KMS, a desktop config file, or a database
- whether the active model is admin-managed or hardcoded

Applications own config resolution. Provider clients never discover
credentials on their own: every key or credential comes from an option, and
the Vertex backends perform no Application Default Credentials discovery
([ADR-0009](adr/0009-vertex-backend-psc-and-gemini-embeddings.md)). The
Anthropic and OpenAI-family clients (`anthropic`, `anthropicvertex`,
`openai`, `openaicompat`) also ignore their SDKs' environment defaults
(`ANTHROPIC_*`, `OPENAI_*`), endpoints included (divergences A6 and X6).
The `google` package does not yet extend that to endpoints: when
`WithBaseURL` is unset, the genai SDK still honors `GOOGLE_GEMINI_BASE_URL`
or `GOOGLE_VERTEX_BASE_URL`, so an application that must pin the Google
endpoint passes `WithBaseURL`.

HTTP injection is needed for tests, proxies, observability, and app-managed timeout policy.

## Metadata And Context

`Metadata map[string]string` is for app-provided labels, not middleware-private coordination.

Key convention:

- package-owned keys use `llms.*`
- provider-owned keys use `provider.<name>.*`
- application-owned keys should use an application prefix, such as `maestro.*` or `morris.*`

Middleware that needs private typed state uses context values with unexported key types rather than mutating shared metadata.

## Concurrency

All core clients, middleware wrappers, fakes, and in-memory limiters are safe for concurrent use unless explicitly documented otherwise.

Provider clients avoid mutable request-scoped state on the client struct. Middleware that tracks metrics or limiter state synchronizes access.

## Tool Loop

`llms/toolloop` is a small synchronous helper over a `ChatClient`: send the
request, execute every tool call the model emits with application-supplied
executors, append the matching tool results, and repeat. `Run` returns an
`Outcome` whose `Kind` says why it stopped: a final answer, an empty response,
a terminal tool, the iteration limit, a provider error, a context-window
overflow it could not recover from, an executor or compaction error, or the
loop's context being canceled or reaching its deadline. Configuration is
validated before any provider call. The decisions of record are
[ADR-0011](adr/0011-toolloop-helper.md) (scope and non-goals; the full
proposal is [`toolloop-proposal.md`](toolloop-proposal.md)),
[ADR-0017](adr/0017-toolloop-terminal-tools.md) (terminal tools, executed limit turn),
[ADR-0019](adr/0019-toolloop-deadline-and-no-iteration-limit.md) (deadline, `NoIterationLimit`),
[ADR-0020](adr/0020-toolloop-empty-response.md) (empty response),
[ADR-0026](adr/0026-context-length-error-kind.md) (context exceeded), and
[ADR-0030](adr/0030-toolloop-compaction.md) (the `Compact` hook and the
`llms/toolloop/compact` policies). `OutcomeKind` may grow, each new kind by
its own ADR, so a switch over it needs a `default` case
([ADR-0032](adr/0032-enum-like-types-v1.md)).

## Model Metadata

The package ships no model registry. If one is added, it is informational
only (context window, maximum output tokens, default embedding dimensions,
rough pricing, token-estimation hints).

Hard invariant: unknown model names must not fail solely because the package does not know them. Any registry is additive and advisory.

## Test Support

`llms/testllm` provides deterministic, concurrency-safe fakes,
`FakeChatClient` and `FakeEmbeddingClient`, plus `TextResponse` and
`ToolCallResponse` helpers.

They support:

- fixed response text
- scripted responses, including tool calls
- scripted errors, or a `Func` that takes full control
- usage metadata injection
- request recording for assertions (`ProviderParams` recorded in their JSON form)
- deterministic embeddings by input text or a configured vector map

Most consumer tests run without real provider calls, so the fakes' correctness gates everything downstream.

## Security And Privacy

The package is careful by default:

- never log API keys
- never log request content in middleware by default
- expose hooks so applications can redact or audit according to their own rules
- keep raw provider responses optional
- avoid global mutable config

Morris applies stricter content classification and audit rules outside the package.

## Versioning

Semantic versioning. From v1.0.0, anything the README
["Stability contract (v1)"](../README.md#stability-contract-v1) covers,
including the semantics in this document and the accepted ADRs, changes
incompatibly only in a new major version with a `/v2` import path. New
capabilities arrive as optional interfaces, new struct fields, new options,
and new values of the enum-like types the contract lists.

A significant structural decision lands an ADR in the same change. A change
to a section of this document either updates the section or, for a decision
that replaces one recorded here, adds an ADR and links it from the section.

## Open Questions

1. Which provider-specific raw response fields, if any, should get normalized instead of left in `Raw`? Any answer is additive: a new normalized field.

Resolved by review:

- Module path is `github.com/SnapdragonPartners/maestro-llms`.
- Core package import name is `llms`.
- OpenAI chat uses the **Responses API** (not Chat Completions — OpenAI is
  deprecating it and Maestro already used Responses). Chat Completions
  servers are served by `openaicompat`.
- Provider constructor shape is a functional options pattern
  (`New(WithAPIKey, WithModel, WithBaseURL, WithHTTPClient, ...)`, returning
  the client and an error).
- Provider error classification goes through one shared internal helper,
  not a classifier per adapter.
- Model metadata is strictly optional/advisory.
- `Raw any` is opt-in and outside the stability contract.
- Limiter stats are a separate optional interface.
