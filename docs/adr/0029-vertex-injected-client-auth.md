# 0029. Vertex: `WithCredentials` authenticates an injected `WithHTTPClient`

- **Status:** Accepted; amended by [ADR-0035](0035-provider-naming.md) (`anthropicvertex.WithEndpoint` is now `WithBaseURL`; the precedence is unchanged)
- **Date:** 2026-10-18

## Context

ADR-0009 set the PSC precedence rule for the Vertex paths: the caller's
`WithHTTPClient` is applied after the SDK's Vertex setup, so it **replaces**
the client the SDK built from the credentials, and "therefore MUST carry
Google auth itself". ADR-0028 (#104) found genai behaves the same way: it
uses `ClientConfig.Credentials` only for a client it builds itself.

Every other provider works the other way. The Anthropic, OpenAI,
openaicompat and Gemini API key backends add their API key per request
whatever client is injected (genai sets `x-goog-api-key` per request). So
"`WithHTTPClient` handles the network, the credentials option handles auth"
held everywhere except Vertex. As a result:

- Moving between a direct backend and Vertex changed what the app's
  PSC/proxy client had to do.
- `WithCredentials` was required but did nothing once a client was
  injected.
- Forgetting the rule produced a 401 on the first call.

The package exists so that changes of model and provider touch consumers as
little as possible, and no current consumer injects a client on a Vertex
path (#108).

## Decision

On every Vertex constructor, `WithCredentials` authenticates the injected
client. The client supplies only the network layer (dialer, CA, proxy,
observability).

- **`anthropicvertex`:** the caller's client is passed to the SDK
  **before** `vertex.WithCredentials`. anthropic-sdk-go documents that a
  client passed first "is honored ...: its transport is wrapped with OAuth
  authorization and its other settings (Timeout, CheckRedirect, Jar) are
  preserved" (`vertex.authorizeClient`, on a shallow copy). `WithEndpoint`
  is still applied after it, so it wins over the regional URL. The Vertex
  path and `anthropic_version` middleware apply either way. This uses the
  SDK's own mechanism; nothing is reimplemented.
- **`google`** (`New` and `NewEmbeddings`, Vertex backend): the shared
  `clientConfig` (ADR-0028) builds a shallow copy of the caller's client
  whose transport is `httptransport.NewClient` over the caller's transport,
  with the credentials and the `X-Goog-User-Project` quota header. That is
  what genai does for the client it builds when none is injected. The Gemini
  API key backend is unchanged: an injected client is used as given.
- **The caller's client is never mutated.** Its `Timeout`, `Jar` and
  `CheckRedirect` carry over to the copy. `anthropicvertex` always passes
  the SDK a shallow copy, because the SDK treats the pointer
  `http.DefaultClient` as "no client" and would otherwise substitute its own,
  dropping an app's PSC transport set on `DefaultClient`.
- **The caller's credentials are never mutated.** `httptransport.NewClient`
  replaces `Credentials.TokenProvider` in place (with a caching wrapper).
  genai calls it with the app's credentials for the client it builds itself,
  so through v0.12 even a plain Vertex `google.New` / `NewEmbeddings`
  mutated them, which races when an app shares one `*auth.Credentials`
  across goroutines. `clientConfig` now gives genai and `authorizeClient` a
  private shallow copy (the struct holds only provider fields and the JSON
  bytes). `anthropicvertex` converts the credentials with `oauth2adapt` and
  never passes them to `httptransport`.
- `WithCredentials` stays required, so no configuration falls back to
  ambient ADC.

### Rejected

- **Keep ADR-0009's rule** (documented in ADR-0028): a provider-specific
  trap, and the opposite of every direct provider.
- **A separate network-only option** (for example `WithTransport`): two ways
  to inject networking, with the trap left on `WithHTTPClient`.

## Consequences

- **Behavior change, breaking on paper only.** An injected client that
  already adds Google auth keeps working. The toolkit's layer sets the
  header and the client's own layer then sets it again, normally for the
  same identity, so the client may drop its auth layer. Morris passes only
  `WithEndpoint`, so no current consumer is affected.
- The toolkit's auth layer runs the credentials' universe-domain check, as
  the SDK-built clients already did.
- Tests pin, on both paths, that a plain injected client gets the
  `WithCredentials` token and that the caller's client is unchanged:
  - `TestVertexPSCPrecedenceAndMiddleware`
  - `TestVertexChatRoutingAndAuth`
  - `TestAutoTruncateTrueVertexPassesThrough`
  - `TestInjectedDefaultClientHonored` (`http.DefaultClient` injected)

  `TestVertexConstructorsDoNotMutateCredentials` builds every google Vertex
  constructor concurrently from one credentials value, with and without an
  injected client. It fails under `-race` and on the `TokenProvider` check
  without the private copy.

  The Gemini API key backend is pinned as not wrapped
  (`TestGeminiAPIInjectedClientNotWrapped`).
- Live tests use injected clients with no auth of their own:
  `TestIntegrationVertexAnthropicReasoningReplay` and
  `TestIntegrationVertexGeminiInjectedClient`.
- **Amends ADR-0009** (the PSC precedence rule) and **ADR-0028** (its
  statement of that rule for genai).

## References

- Issue #108; ADR-0009, ADR-0027, ADR-0028
- `docs/MAESTRO_DIVERGENCES.md` X12
