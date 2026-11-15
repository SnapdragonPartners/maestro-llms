# 0048. Provider-neutral chat constructor: `providers.New(id, Options)`

- **Status:** Proposed
- **Date:** 2026-11-15

## Context

Every provider is its own leaf package with its own `New(...Option)`
(spec "Package Shape"), so every consumer that lets configuration pick the
provider writes the same switch (issue #175):

- **buggrit:** `internal/llm/llm.go`, `newProviderClient` (anthropic,
  openai, google, ollama, openaicompat).
- **Maestro:** `pkg/agent/internal/llmadapter/llmadapter.go`,
  `NewChatClient` (anthropic, openai, google, ollama; it passes Ollama's
  base URL in the API-key argument).
- **maestro-agents:** its standalone host needs the same switch, and it is
  compiled into Morris, which calls both Anthropic and Gemini on Vertex AI.

The copies already differ in which providers they cover and how each is
configured. The mapping from a provider choice to a provider package belongs
beside the provider packages.

## Decision

### 1. API (additive to v1)

A new leaf package `llms/providers` (decided with Dan, 2026-11-15):

```go
package providers

// New builds the chat client for provider id from opts.
func New(id llms.ProviderID, opts Options) (llms.ChatClient, error)

type Options struct {
    HTTPClient *http.Client // every provider; PSC / custom transport
    Vertex     *Vertex      // nil: the provider's direct API
    Model      string       // required by every provider; passed through unchecked
    APIKey     string
    BaseURL    string       // optional except for openaicompat
}

// Vertex selects Vertex AI for anthropic (anthropicvertex) and google.
type Vertex struct {
    Credentials *auth.Credentials // app-supplied; no ADC (ADR-0009)
    Project     string
    Location    string
}

var (
    ErrUnknownProvider   = errors.New("providers: unknown provider")
    ErrUnsupportedOption = errors.New("providers: option not supported by this provider")
)
```

`Options` grows only by new fields (README stability contract), so callers
use keyed literals.

### 2. Routing

| `id` | `Vertex == nil` | `Vertex != nil` |
|---|---|---|
| `ProviderAnthropic` | `anthropic.New` | `anthropicvertex.New(WithVertex, WithCredentials, …)` |
| `ProviderOpenAI` | `openai.New` | refused |
| `ProviderGoogle` | `google.New` | `google.New(WithVertex, WithCredentials, …)` |
| `ProviderOllama` | `ollama.New` | refused |
| `ProviderOpenAICompat` | `openaicompat.New` | refused |
| anything else (incl. `""`) | `ErrUnknownProvider` | `ErrUnknownProvider` |

`ProviderVLLM` has the same value as `ProviderOpenAICompat` (ADR-0027), so
it routes to `openaicompat` without a case of its own. A Vertex client keeps
its adapter's ID (`ProviderAnthropic` / `ProviderGoogle`): a `ProviderID`
names the wire protocol, not the deployment.

Each non-zero option is forwarded as the provider's own `With…` option, and
only then: an empty field is not forwarded, so every default is the
provider package's default (for example Ollama's local base URL).

### 3. Each option means one thing, or is refused

An option a provider can't use is a construction error, never ignored and
never repurposed (the Maestro API-key-as-base-URL case). The refusals, all
checked before any provider constructor runs:

- `APIKey` with `ProviderOllama` (Ollama takes no key).
- `Vertex` with openai, ollama or openaicompat.
- `APIKey` with `Vertex` (anthropicvertex has no key option; `google`
  already refuses the pair as an ambiguous backend, and `providers`
  refuses it first so both Vertex routes give the same error).

Everything else is the provider package's job, unchanged: a missing model,
a missing base URL for openaicompat, missing or token-less Vertex
credentials, `HTTPClient` plus credentials authenticating a copy
(ADR-0029), `ListModels` failing fast on Vertex (ADR-0037). The neutral
constructor behaves exactly as the direct call it makes, and no validation
is duplicated.

### 4. Errors

Every error is a `*llms.ProviderError` of kind `config`, as for every
provider constructor. `Provider` is `id`. The two errors raised by
`providers` itself carry a sentinel as `Cause`, so
`errors.Is(err, providers.ErrUnknownProvider)` and
`errors.Is(err, providers.ErrUnsupportedOption)` match; the message names
the provider and the option. Errors from a provider constructor are
returned as they are.

### 5. Return value

`New` returns the concrete client as `llms.ChatClient`, so optional
interfaces (`llms.ModelLister`, and whatever grows later) stay reachable by
type assertion.

### 6. Not included

- **SDK `MaxRetries`:** the middleware chain owns retries and the SDK
  default is already 0 in every adapter (`google` and `ollama` have no SDK retries at all). Can be added later as a field.
- **Embeddings** (`NewEmbeddings`): no consumer has asked; a sibling
  function can be added later.
- **Provider-specific settings** (`anthropic.WithRequestOptions`,
  `google.WithMaxInputBytes`, …): call the provider package directly. The
  neutral constructor covers the common case and hides nothing.
- **Inferring the provider from the model name.** The mapping isn't 1:1
  (Opus through Anthropic or Vertex, DeepSeek through many OpenAI-compatible
  hosts), and the package ships no model registry (spec "Model Metadata").
- **Migrating buggrit and Maestro**: each adopts it on its own schedule.

### 7. Testing

Unit tests, no network:

- Each provider ID (and `ProviderVLLM`) builds a client whose `Model()`
  reports the expected `ProviderID` and model name; both Vertex routes build
  with a static-token `auth.Credentials`.
- Options reach the provider: a `BaseURL` + `HTTPClient` pointed at an
  `httptest` server receives the request for each provider (and `APIKey`
  arrives where the provider sends it), so a dropped option is caught.
- Each refusal in §3 and the unknown/empty ID return a config
  `ProviderError` matching the right sentinel.
- A provider's own validation error (missing model) passes through
  unchanged.
- The returned clients satisfy `llms.ModelLister` where the concrete client
  does.

## Consequences

- Consumers replace their switch with one call; the provider set and each
  option's meaning are decided in one place, here.
- Importing `llms/providers` links every provider package and SDK. That is
  the point; apps that want one provider keep importing it directly, and the
  core `llms` package stays SDK-free.
- A new provider package adds a case here in the same PR.
- The spec "Package Shape" lists `llms/providers/` as a package of its own.

## References

- Issue #175.
- Spec "Package Shape", "Provider", "Model Metadata".
- ADR-0009 (app-supplied auth, no ADC), ADR-0027 (`vllm` → `openaicompat`),
  ADR-0028 (Gemini on Vertex), ADR-0029 (credentials with an injected HTTP
  client), ADR-0037 (no model listing on Vertex).
