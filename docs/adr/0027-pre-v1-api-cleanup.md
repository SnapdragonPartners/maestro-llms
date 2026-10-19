# 0027. Pre-v1 API cleanup: `openaicompat` package, one Google credential type, shared Google options

- **Status:** Accepted
- **Date:** 2026-10-18

## Context

At v1.0.0, every exported name and signature becomes permanent: changing one
afterwards means a `/v2` import path (#84). Three surfaces were accidents of
history (#82):

1. **`llms/providers/vllm` is not vLLM-specific.** It is the generic OpenAI
   Chat Completions client. It was verified on the hosted LunaRoute gateway
   (#54, divergence V6), and buggrit uses it as its "openai-compatible"
   provider. The name tells gateway users to look elsewhere.
2. **The two Vertex entry points took different Google credential types**,
   each inherited from the SDK underneath:
   - `anthropicvertex.WithCredentials` took `*google.Credentials`
     (`golang.org/x/oauth2/google`).
   - `google.EmbeddingConfig.Credentials` took `*auth.Credentials`
     (`cloud.google.com/go/auth`).

   So Morris had to look up the same GCP identity twice, through two
   libraries, and the README had to warn about it.
3. **`google.NewEmbeddings(EmbeddingConfig)` took a config struct**, while
   every other constructor in the module takes functional options. #104
   (Gemini chat on Vertex) needs the same Vertex settings on `google.New`.
   Two option sets would have meant defining those settings twice.

## Decision

### 1. `openaicompat`, with `vllm` as a permanent deprecated alias

- The package moves to `llms/providers/openaicompat`, with no behavior change.
- `llms/providers/vllm` stays **permanently**, through v1, as a deprecated
  forwarding package:
  - `Client` and `Option` are type aliases.
  - `New` and every `With*` forward their arguments unchanged.

  So existing imports keep compiling, and the two packages' values are
  interchangeable. The alias has no logic of its own. A test pins that the
  forwarding applies every option.
- The provider ID becomes `llms.ProviderOpenAICompat = "openaicompat"`.
- `llms.ProviderVLLM` stays as a deprecated constant **with the same value**.
  Code that uses either constant keeps matching. Code that compares against
  the literal `"vllm"` must change. That covers `ModelRef.Provider` and
  `ProviderError.Provider` (metrics labels, logs) and `ProviderParams` map
  keys.
- The ID follows the package because a `ProviderID` names the wire protocol
  a client speaks (ADR-0021), and here that protocol is OpenAI Chat
  Completions, not vLLM.

### 2. `*auth.Credentials` everywhere

- Both Vertex entry points take `*auth.Credentials` from
  `cloud.google.com/go/auth`:
  - `anthropicvertex.WithCredentials`
  - `google.WithCredentials`, below
- This is the newer library. genai uses it, and Google's SDKs are moving to
  it.
- `anthropicvertex` converts the credentials internally with
  `oauth2adapt.Oauth2CredentialsFromAuthCredentials`, because the Anthropic
  Vertex SDK still takes the oauth2 type. The older type no longer appears
  in our API.
- Both entry points reject credentials with a nil `TokenProvider` as a config
  error. Such credentials cannot mint tokens, and the contract that the app
  supplies the credentials (ADR-0009) forbids any fallback to ambient ADC.
  For `anthropicvertex`, this replaces the old nil-`TokenSource` check. For
  embeddings, the check is new.
- No ADC discovery is added. The app still builds the credentials, typically
  with `credentials.DetectDefault`.
- Provider-SDK types that are deliberate escape hatches stay in the API. The
  one case is `anthropic.WithRequestOptions(...option.RequestOption)`. That
  means a major bump of anthropic-sdk-go may force a major bump of this
  module. Wrapping the type would add little and lose the escape hatch.

### 3. One `google.Option` for chat and embeddings

- `google.EmbeddingConfig` is removed, and `NewEmbeddings` now takes options:
  `NewEmbeddings(opts ...Option)`.
- `New` and `NewEmbeddings` share one `Option` type, so each backend option
  is defined once:
  - `WithAPIKey`
  - `WithVertex(project, location)`
  - `WithCredentials`
  - `WithBaseURL` (the old `Endpoint` field)
  - `WithHTTPClient`
- The embedding settings become `WithDimensions`, `WithMaxInputBytes` and
  `WithAutoTruncate`.
- **An option the constructor does not accept is a config error, never
  silently ignored:**
  - `New` rejects the embedding-only options.
  - `New` also rejects the Vertex options, as a separate check, until #104.
    #104 then removes that one rejection and adds behavior. It changes no
    signature or type.
  - Every option `New` accepts also applies to `NewEmbeddings`.
    `WithMaxRetries` is the same validated no-op on both, so `NewEmbeddings`
    has nothing to reject.
- Setting `WithVertex` or `WithCredentials` at all selects Vertex. Combined
  with `WithAPIKey`, that fails closed as an ambiguous backend, as before.
- The truncation rule that fails closed (ADR-0009 refinement) is unchanged:
  `WithMaxInputBytes(n>0)` is required unless `WithAutoTruncate(true)`, and
  `WithAutoTruncate(true)` works only on Vertex.

## Consequences

- **Breaking for buggrit and Maestro: nothing until they choose to move.** The
  `vllm` import keeps compiling. Moving to `openaicompat` is an import-path
  change. Code that matches the literal `"vllm"` provider string sees
  `"openaicompat"`.
- **Breaking for Morris, in two places:**
  - The Claude path builds credentials with `credentials.DetectDefault`
    instead of `oauth2google.FindDefaultCredentials`, about a one-line
    change.
  - The embeddings `EmbeddingConfig{...}` literal becomes option calls.
- The `vllm` alias and `ProviderVLLM` are permanent v1 surface. They cost one
  small file and one constant.
- Integration tests keep their `TestIntegrationVLLM*` names and the
  `MAESTRO_VLLM*` environment variables, because they target a vLLM server.
- **ADR-0009 is amended:** the credential type, and the
  `EmbeddingConfig` → options change. **ADR-0015 is amended:** the package
  name and provider ID.

## References

- Issue #82 (decisions recorded in its comments), #104, #84
- ADR-0009 (Vertex backend, app-supplied credentials, the truncation rule
  that fails closed)
- ADR-0015 (`vllm` provider), ADR-0021 (`ProviderID` names the wire
  protocol)
- `docs/MAESTRO_DIVERGENCES.md` V7

## Amendments

- **ADR-0028 (#104):** `New`'s temporary rejection of `WithVertex` /
  `WithCredentials` is lifted; `New` and `NewEmbeddings` now select their
  backend through one shared function.
