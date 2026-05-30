# maestro-llms Specification

Status: Draft  
Date: 2026-05-15  
Intended repo/package name: `maestro-llms`

## Purpose

`maestro-llms` is a small open source Go toolkit for working with LLM and embedding providers behind stable application-facing interfaces.

It is intended to be shared by:

- **Maestro**, a desktop/local orchestration app with in-process rate limiting.
- **Morris**, a cloud-deployed family-office app with shared/distributed rate limiting, operational secrets, audit, and content-classification rules.

The package should extract common provider and middleware functionality without importing product-specific assumptions from either application.

## Goals

1. Provide stable chat/completion and embedding abstractions.
2. Support multiple providers through optional provider packages.
3. Normalize messages, tool calls, tool results, usage metadata, and provider errors.
4. Support middleware composition for retries, timeout, logging hooks, metrics hooks, validation, and rate limiting.
5. Let applications plug in their own configuration, secret resolution, logging, audit, and rate-limit storage.
6. Keep the core package small enough that it remains useful outside Maestro and Morris.

## Non-Goals

- No Maestro agent concepts.
- No Morris tenant/user/content authorization concepts.
- No built-in database schema.
- No built-in secret store.
- No built-in audit taxonomy.
- No application config file format.
- No prompt management system.
- No retrieval/RAG framework.
- No required streaming support in v0.

Streaming can be added later, but the initial API should not block on it.

## Normative Clarifications From Engineering Review

The current Maestro implementation is a working, tested reference for tool calls across Anthropic, OpenAI, Google, and Ollama. The extraction should use that implementation to resolve provider-specific behavior, while still removing Maestro-specific orchestration, config, and logging assumptions.

The following decisions are normative for v0.1:

- Tool-call round trips must be unambiguous and round-trippable.
- Multi-tool-call assistant turns must be unambiguous and round-trippable.
- System instructions live in a dedicated `ChatRequest.System` field, not arbitrary mid-list messages.
- v0 system instructions are text-only.
- `Temperature` must distinguish unset from intentional `0.0`.
- Future streaming support is a separate optional interface, not a future method added to `ChatClient`.
- Local limiter rejections are distinct from provider `429` errors.
- Limiter release must work after request context cancellation.
- Retry middleware should wrap rate-limit middleware by default so each provider attempt gets its own reservation.
- Provider clients, middleware wrappers, fakes, and in-memory limiters are safe for concurrent use unless explicitly documented otherwise.
- Unknown model names must not fail solely because the optional model registry does not know them.

## Package Shape

Possible module:

```text
github.com/SnapdragonPartners/maestro-llms
```

Suggested package layout:

```text
maestro-llms/
  llms/                 core interfaces and shared types
  llms/middleware/      provider-neutral middleware
  llms/ratelimit/       limiter interfaces and optional in-memory limiter
  llms/providers/
    anthropic/
    openai/
    google/
    ollama/
  llms/testllm/         deterministic fakes and test helpers
```

Provider packages should be imported only by applications that need them. The core package should not force every downstream app to pull every provider SDK unless that is unavoidable.

## Core Concepts

### Provider

A provider is an external service family, such as:

- `anthropic`
- `openai`
- `google`
- `ollama`

### Model

A model is a provider-specific model identifier, such as:

- `claude-sonnet-...`
- `gpt-...`
- `text-embedding-...`
- `gemini-...`

The package may include a lightweight model registry for known defaults and token limits, but applications must be able to pass unknown/new model names without waiting for package releases.

### Purpose

Applications should be able to label a call purpose without the package needing to understand app semantics:

```go
type Purpose string

const (
    PurposeChat          Purpose = "chat"
    PurposeEmbedding     Purpose = "embedding"
    PurposeClassification Purpose = "classification"
    PurposeSummarization Purpose = "summarization"
)
```

Applications may pass custom purposes.

`Purpose` is application intent. It answers "why is this call happening?" Examples: chat, embedding, classification, summarization, title generation, ingestion inspection.

`Operation` is transport shape for middleware/limiting. It answers "what provider API class is being used?" Examples: chat, embedding. `Operation` may be derived by middleware from the client/request type; it remains explicit in reservation requests so application-provided distributed limiters do not need to infer it.

## Chat API

Initial v0 should support synchronous completion only.

```go
type ChatClient interface {
    Complete(ctx context.Context, req ChatRequest) (ChatResponse, error)
    Model() ModelRef
}
```

Future streaming support should be a separate optional capability:

```go
type StreamingChatClient interface {
    Stream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error)
}
```

Callers and middleware should discover streaming with a type assertion. Adding `Stream` to `ChatClient` later would be a breaking change for every provider, fake, and middleware.

Model listing is an additional optional capability (v0.6 / ADR-0012):

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

Discovery is by type assertion on a `ChatClient`. Providers without a list API simply don't implement it. Per-family upgrade detection (`LatestInFamily`) is a provider-package concern — family naming differs per provider (Anthropic `claude-{opus|sonnet|haiku}`, OpenAI dated-snapshot stripping, Gemini `gemini-{pro|flash|nano|ultra}`) — and is offered as a pure helper plus a one-shot convenience method on the provider's chat client. Ollama implements `ModelLister` only; its list is locally pulled models and "latest in family" has no consistent meaning for community-uploaded tags. See ADR-0012 for binding non-goals.

### Model Reference

```go
type ModelRef struct {
    Provider string
    Name     string
}
```

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
}
```

`Metadata` is app-supplied, provider-neutral context for middleware and logging. It must not be sent to providers unless a middleware or provider explicitly chooses to use a key.

`System` carries system/developer instructions and is normalized by each provider adapter. Anthropic receives it as the top-level system parameter; OpenAI-compatible providers receive it in the provider-appropriate system/developer slot; Google and Ollama adapters translate it according to their supported request shape.

System instructions are not legal as arbitrary mid-conversation `Messages`. Multiple `System` parts are legal and preserve order; providers may join them when their API requires one string.

In v0, `System` parts must be text-only. Validation middleware and provider adapters should reject `tool_call`, `tool_result`, or future multimodal parts in the system field unless the spec is deliberately extended.

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
    CacheBreakpoint bool // optional prompt-cache hint (see below)
}

type ContentPartType string

const (
    ContentText       ContentPartType = "text"
    ContentToolCall   ContentPartType = "tool_call"
    ContentToolResult ContentPartType = "tool_result"
)
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

Provider mapping:

- Anthropic: `RoleTool` / `tool_result` parts are encoded as user-role tool-result content blocks because Anthropic models tool results as user messages.
- OpenAI: `RoleTool` maps to tool-role messages.
- Google/Ollama: provider adapters use the existing Maestro-tested tool-call mapping for that provider.

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
Gemini ANY-mode. Ollama has no `tool_choice`, so a `Required` (or `Tool`)
choice is best-effort there — tools are offered, the model decides — and
adapters must not silently lose caller intent (see MAESTRO_DIVERGENCES).

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

`ToolCall.Parameters` is `json.RawMessage` to preserve exact provider-emitted JSON and avoid lossy `map[string]any` round-trips. Applications can unmarshal into typed structs.

`ToolResult.Content` is a string in v0. This intentionally limits tool results to textual/JSON-string content. Structured or multimodal tool results can be added later by extending `ToolResult` without changing the core round-trip semantics.

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

Assistant response asks for a tool:

```go
resp.Message == llms.Message{
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
- Google/Ollama: use the existing Maestro-tested mapping for that provider.

Every tool call in an assistant turn must receive a corresponding tool result before the next provider call. Validation middleware should reject missing, duplicate, or orphaned tool results.

### Chat Response

```go
type ChatResponse struct {
    Message    Message
    Text       string
    ToolCalls  []ToolCall
    StopReason StopReason
    Usage      Usage
    Raw        any
}

type StopReason string
```

`Raw` is optional provider-specific data for advanced callers. Middleware should not depend on it.

`Text` and `ToolCalls` are convenience mirrors derived from `Message`. Providers must populate `Message` as the source of truth. If mirrors are present, tests should assert that they match `Message`. Helper methods may be added later so callers can avoid relying on duplicated fields.

`Raw` is outside the stability contract. Applications that depend on it accept provider-package breakage risk.

## Embeddings API

Embeddings are required in v0.

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

Provider clients should return a typed validation/config error when `Inputs` exceeds the provider or model batch limit. Automatic chunking is intentionally outside v0 provider clients; applications own chunking because they also own retry policy, progress reporting, source IDs, and ingestion transaction boundaries.

### v0.4 (ADR-0009): task-typed embeddings, Vertex, single-input models

`Task`/`Title` (shown in the types above) are **implemented in v0.4 core**:
provider-neutral, advisory, honored only where the provider supports them
(Gemini retrieval-document/-query etc.) and ignored elsewhere (OpenAI). They
are *not* app context smuggled through `Metadata` — task type materially
changes the vectors for retrieval.

The remaining v0.4 behavior below is **implemented** (Anthropic-on-Vertex via
the `anthropicvertex` package, and the Gemini/Vertex embeddings client):

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

Anthropic and Gemini-embedding access via **Vertex AI** is a separate-package
backend (`anthropic/anthropicvertex`) plus a low-level
`anthropic.WithRequestOptions` escape hatch, preserving leaf imports.
Credentials and the endpoint (for PSC) are **app-supplied** — the package
exposes endpoint/base-URL override and HTTP-client/transport injection; it
performs no ADC discovery. Networking (PSC, DNS, VPC-SC, IAM) is the
application's infrastructure concern, not this package's. See ADR-0009.

## Usage Metadata

Usage should be normalized across chat and embeddings.

```go
type Usage struct {
    InputTokens       int
    OutputTokens      int
    TotalTokens       int
    EmbeddingTokens   int
    CacheReadTokens   int
    CacheWriteTokens  int
    ProviderRequestID string
}
```

If a provider does not return a field, leave it zero. Middleware can estimate tokens before a call; provider clients should fill actual usage when available.

`CacheReadTokens` and `CacheWriteTokens` cover provider prompt-cache accounting such as Anthropic cache reads and cache creation. Providers that do not expose cache token accounting should leave these fields zero.

## Errors

Provider errors should be classified.

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
    ErrorKindUnknown       ErrorKind = "unknown"
)

type ProviderError struct {
    Provider   string
    Model      string
    Kind       ErrorKind
    StatusCode int
    Message    string
    RetryAfter time.Duration
    Cause      error
}
```

Applications should be able to use `errors.As` to inspect `ProviderError`.

Retry guidance:

- `rate_limited`, `timeout`, and `unavailable` are normally retryable.
- `auth`, `config`, `bad_request`, and `content_policy` are normally not retryable.
- `RetryAfter` should capture provider `Retry-After` headers when available.

Local limiter rejections should not be represented as provider errors. Use a separate error type:

```go
type LimitError struct {
    Provider string
    Model    string
    Reason   string
    RetryAfter time.Duration
}
```

This lets retry/circuit middleware distinguish "we throttled locally before making a provider call" from "the provider returned 429."

## Middleware

The package should support middleware composition separately for chat and embeddings.

```go
type ChatMiddleware func(ChatClient) ChatClient
type EmbeddingMiddleware func(EmbeddingClient) EmbeddingClient

func ChainChat(base ChatClient, mws ...ChatMiddleware) ChatClient
func ChainEmbeddings(base EmbeddingClient, mws ...EmbeddingMiddleware) EmbeddingClient
```

The first middleware argument is outermost:

```go
ChainChat(base, A, B, C)
// call order: A -> B -> C -> base
```

Initial middleware candidates:

- timeout
- retry
- circuit breaker
- rate limit
- metrics hooks
- request/response validation
- token estimation

Logging and metrics should use narrow callback interfaces, not a specific logging package.

Recommended order for most apps:

1. request validation
2. retry
3. per-attempt timeout
4. circuit breaker
5. local/shared rate limit reservation
6. metrics/logging hooks
7. provider client

With this default, each retry attempt flows through timeout, circuit breaker, and rate-limit reservation independently. That is intentional: retries are real provider traffic and should be gated by the same limiter as first attempts.

Applications may change the order, but the package documentation should explain tradeoffs. In particular:

- Retry outside rate limiting means each provider attempt consumes a reservation.
- Rate limiting outside retry means one reservation covers all attempts and can undercount provider traffic.
- Timeout outside retry creates one total deadline for all attempts.
- Timeout inside retry creates one timeout budget per attempt, which is the default recommendation here.

## Rate Limiting

The package should define rate-limit interfaces and provide an optional in-memory implementation for local/desktop apps. Cloud apps must be able to plug in distributed implementations.

### Design Requirement

The rate limiter must not assume process-local state is authoritative.

Maestro can use an in-memory limiter. Morris needs a PostgreSQL-backed or otherwise shared limiter because Cloud Run can run multiple instances. The same middleware should work with both.

### Reservation Interface

```go
type Limiter interface {
    Reserve(ctx context.Context, req ReservationRequest) (Reservation, error)
}

type ReservationRequest struct {
    Provider       string
    Model          string
    Operation      Operation
    Purpose        Purpose
    EstimatedUnits UsageUnits
    Subject        Subject
    Metadata       map[string]string
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
    Commit(ctx context.Context, usage Usage) error
    Release(ctx context.Context) error
}
```

Semantics:

- `Reserve` is called before the provider request.
- `Commit` records actual usage when the provider call succeeds or returns usage.
- `Release` frees any leased concurrency/request slot.
- Middleware should call `Commit` when actual usage is available and always call `Release`.
- Implementations must make repeated `Release` safe.
- Implementations must make `Release` safe after `Commit`.
- Middleware should call `Release` with a context that survives request cancellation, such as `context.WithoutCancel(ctx)` or a bounded background context.

Reconciliation contract:

- `Reserve` consumes or holds the estimated usage according to the limiter implementation.
- `Commit` records actual usage.
- If actual usage is lower than estimated, implementations may refund unused units when they can do so safely.
- If actual usage is higher than estimated, implementations should charge the delta if the backend supports it.
- A failed delta charge must not erase the fact that the provider call already happened; implementations should return an error so the application can audit/account for the mismatch.
- Concurrency leases are released by `Release`, not by `Commit`.

In-memory limiter implementation:

- appropriate for Maestro/local tests
- token bucket and concurrency semaphore
- optional stats interface

Stats should be a separate optional interface:

```go
type LimiterStats interface {
    Stats(ctx context.Context) (LimiterSnapshot, error)
}
```

Consumers discover it with a type assertion. Core `Limiter` stays minimal so distributed implementations are not forced to support stats before they need them.

Distributed limiter implementation:

- not required in this package initially
- Morris can implement it in its own repo using PostgreSQL
- later, a generic SQL/Redis implementation can be added if it remains app-neutral

## Token Estimation

The package should define a token estimator interface.

```go
type TokenEstimator interface {
    EstimateChat(req ChatRequest) UsageUnits
    EstimateEmbeddings(req EmbeddingRequest) UsageUnits
}
```

Default estimator can be approximate. Provider-specific estimators may be added. Overestimation is safer for rate limiting than underestimation.

A separate, exported text-level helper is available at the core level for consumers that need to estimate the token count of a standalone string (budget-aware chunking, ad-hoc inspection) without going through the request-shaped interface (ADR-0013):

```go
func EstimateTextTokens(s string) int
```

Bias is intentionally **neutral** (~4 chars/token, rune-counted) — distinct from the request-shaped estimator's high bias — because over-estimating during chunking produces smaller-than-necessary chunks and thus more downstream API calls. The two estimators serve different purposes and should not be consolidated.

## Provider Clients

Initial provider support:

### Anthropic

Required for Morris chat and already useful in Maestro.

Package:

```text
llms/providers/anthropic
```

Expected:

- chat completion
- tool calls
- usage metadata when available
- error classification
- injectable HTTP client or transport where the SDK/provider shape allows it

Embeddings are not required.

### OpenAI

Required for Morris embeddings.

Package:

```text
llms/providers/openai
```

Expected:

- embeddings (delivered in v0.1)
- chat via the OpenAI **Responses API**, not Chat Completions: OpenAI is
  deprecating Chat Completions, and Maestro's tested client already uses
  Responses. Required for the Maestro cut-over (v0.2), not optional.
- tool calls (with chat)
- usage metadata
- error classification
- injectable HTTP client or transport where the SDK/provider shape allows it

### Google

Chat client, required for the Maestro cut-over (v0.2). Extracted from
Maestro's tested `google.golang.org/genai` implementation. Not needed by
Morris.

### Ollama

Chat client for local development, required for the Maestro cut-over
(v0.2). Extracted from Maestro's tested `github.com/ollama/ollama/api`
implementation. Not needed by Morris.

## Configuration Boundary

The package should accept already-resolved provider configs.

```go
type ProviderConfig struct {
    Provider string
    Model    string
    APIKey   string
    BaseURL  string
    Headers  map[string]string
    HTTPClient *http.Client
}
```

The package should not know:

- where the API key came from
- whether it was read from env, Secret Manager, KMS, a desktop config file, or a database
- whether the active model is admin-managed or hardcoded

Applications own config resolution.

Provider packages should expose an options pattern rather than a large constructor matrix:

```go
client := anthropic.New(
    anthropic.WithAPIKey(apiKey),
    anthropic.WithModel(model),
    anthropic.WithHTTPClient(httpClient),
)
```

HTTP injection is needed for tests, proxies, observability, and app-managed timeout policy.

## Metadata And Context

`Metadata map[string]string` is for app-provided labels, not middleware-private coordination.

Key convention:

- package-owned keys use `llms.*`
- provider-owned keys use `provider.<name>.*`
- application-owned keys should use an application prefix, such as `maestro.*` or `morris.*`

Middleware that needs private typed state should prefer context values with unexported key types rather than mutating shared metadata.

## Concurrency

All core clients, middleware wrappers, fakes, and in-memory limiters should be safe for concurrent use unless explicitly documented otherwise.

Provider clients should avoid mutable request-scoped state on the client struct. Middleware that tracks metrics or limiter state must synchronize access.

## Optional Model Registry

The package may include a model registry for informational metadata:

- provider
- context window
- maximum output tokens
- default embedding dimensions
- rough pricing
- rough token-estimation hints

Hard invariant: unknown model names must not fail solely because the registry does not know them. The registry is additive and advisory.

## Test Support

The package should include deterministic test clients.

```go
type FakeChatClient struct { ... }
type FakeEmbeddingClient struct { ... }
```

Needed behavior:

- fixed response text
- scripted tool calls
- scripted errors
- usage metadata injection
- deterministic embeddings by input text or configured vector map

This is important because both Maestro and Morris need most tests to run without real provider calls.

## Security And Privacy

The package should be careful by default:

- never log API keys
- never log request content in middleware by default
- expose hooks so applications can redact or audit according to their own rules
- keep raw provider responses optional
- avoid global mutable config

Morris will apply stricter content classification and audit rules outside the package.

## Versioning

Use semantic versioning.

Milestones (reprioritized 2026-05-15 — see "Roadmap Update" below; the
original sequencing was Morris-first, but the Maestro cut-over gates on the
full chat provider set, so chat parity moves ahead of resilience
middleware).

### v0.1 — delivered (tagged v0.1.0)

- core chat and embeddings interfaces
- OpenAI embeddings provider
- Anthropic chat provider
- middleware chaining
- rate limiter interfaces
- in-memory limiter
- deterministic fakes

### v0.2 — Maestro chat parity (next; gates the Maestro cut-over)

- OpenAI chat provider via the Responses API
- Google chat provider (genai), extracted from Maestro
- Ollama chat provider, extracted from Maestro
- shared internal provider error-classifier helper (introduced here, with
  four providers, rather than deferred — anthropic/openai are migrated onto
  it)
- Maestro consumes the package in one clean cut-over once all four chat
  providers + embeddings are in (no incremental adoption)

### v0.3 — resilience middleware

- retry/timeout/circuit middleware
- metrics hook interfaces
- richer provider error classification

### v0.4

- optional streaming interfaces (`StreamingChatClient`)
- optional generic Redis/Postgres limiter if it can stay app-neutral

## Roadmap Update — 2026-05-15

v0.1.0 shipped (steps 1–9 below, all merged; live-verified against the real
Anthropic and OpenAI APIs). Two decisions reshape what comes next:

- **Maestro adoption is a single clean cut-over, not incremental.** Maestro
  will not route some providers through the package while keeping its own
  for others — too hard to test. Therefore the cut-over is blocked until the
  *entire* chat provider set Maestro uses (Anthropic ✓, OpenAI, Google,
  Ollama) plus embeddings is available.
- **Consequently, provider chat parity is reprioritized ahead of the
  resilience middleware.** The original milestones were Morris-first
  (Anthropic chat + OpenAI embeddings is all Morris needs now). Following
  that order would strand the Maestro cut-over behind retry/timeout/circuit
  work it does not need. So v0.2 is now the three remaining chat providers
  (OpenAI via Responses API, Google, Ollama) + the shared error-classifier;
  resilience middleware slips to v0.3.

These providers are extractions from Maestro's tested implementations, not
greenfield, which lowers risk. They reuse the Anthropic provider's
structure (options pattern, httptest-based unit tests, build-tagged live
test, typed-error classification).

## Extraction Plan

1. Create new `maestro-llms` repository.
2. Copy/adapt Maestro core LLM types into app-neutral names.
3. Remove Maestro agent/config/logging dependencies.
4. Add embedding interfaces before porting providers.
5. Extract Anthropic chat provider.
6. Extract OpenAI provider with embeddings first.
7. Extract middleware chain.
8. Define limiter interfaces and adapt Maestro in-memory limiter.
9. Add fakes and tests.
10. Extract remaining Maestro chat providers (OpenAI Responses API, Google,
    Ollama) + shared internal error-classifier helper. (v0.2)
11. Update Maestro to consume the package in one clean cut-over once step 10
    is complete.
12. Add Morris dependency (Morris needs only Anthropic chat + OpenAI
    embeddings, both already in v0.1, so this is not blocked by step 10).

## Open Questions

1. Which provider-specific raw response fields, if any, should get normalized instead of left in `Raw`?

Resolved by review:

- Module path is `github.com/SnapdragonPartners/maestro-llms`.
- Core package import name should be `llms`.
- OpenAI embeddings and Anthropic chat are required in v0.1 (delivered).
- OpenAI chat ships in v0.2 via the **Responses API** (not Chat
  Completions — OpenAI is deprecating it and Maestro already uses
  Responses).
- Google and Ollama chat providers move from v0.3 to v0.2: the Maestro
  cut-over needs the full chat set and is a single clean cut-over, not
  incremental.
- Provider constructor shape is a functional options pattern
  (`New(WithAPIKey, WithModel, WithBaseURL, WithHTTPClient, ...)`),
  validated against the v0.1 anthropic/openai providers.
- Shared provider error-classifier is introduced in v0.2 (four providers),
  not deferred to v0.3.
- Model registry is allowed but strictly optional/advisory.
- `Raw any` is opt-in and outside the stability contract.
- Limiter stats are a separate optional interface.

## Initial Recommendation

Proceed with a small extraction focused on what Morris and Maestro both need immediately:

- app-neutral chat interface
- app-neutral embeddings interface
- Anthropic chat client
- OpenAI embeddings client
- middleware chain
- limiter reservation interface
- in-memory limiter for Maestro
- fakes for tests

Do not include streaming in v0.1. Do not include Morris distributed rate limiting in the shared package yet; Morris should implement the `Limiter` interface with PostgreSQL in its own codebase first. If that implementation later proves app-neutral, promote it into `maestro-llms`.
