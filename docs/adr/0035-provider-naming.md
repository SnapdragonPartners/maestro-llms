# 0035. Provider naming: `openai.New` is chat, anthropicvertex takes `WithVertex`/`WithBaseURL`

- **Status:** Accepted
- **Date:** 2026-10-27

## Context

The #84 step 2 whole-API audit found two provider packages whose names
break the pattern the others follow (#84 step 3, v0.15 PR 3). At v1 the
names are frozen, so they have to be settled now.

1. **openai has the constructors the other way round.** In every other
   provider package `New` builds the chat `*Client`, and `google` builds
   embeddings with `NewEmbeddings` → `*EmbeddingClient`. In `openai`,
   `New` builds the embeddings `*Client` (it shipped first, in v0.1) and
   the chat client is `NewChat` → `*ChatClient`. A reader who knows one
   provider guesses wrong in this one, and both Codex and the Claude audit
   flagged it independently.
2. **anthropicvertex names the same settings differently from google.**
   Both packages configure a Vertex backend, but `google` takes
   `WithVertex(project, location)` and `WithBaseURL`, while
   `anthropicvertex` takes `WithProjectID`, `WithRegion` and `WithEndpoint`.
   An app that configures both (Morris) writes the same values twice under
   different names. `anthropicvertex` also has `WithContext`, which no other
   provider has.
3. **The `vllm` alias can drift.** ADR-0027 made `vllm` a permanent
   forwarding alias of `openaicompat`. Nothing checks that an export added
   to `openaicompat` is forwarded, so an import-path swap could stop
   compiling.

## Decision

1. **openai: `New` → `*Client` (chat, Responses API); `NewEmbeddings` →
   `*EmbeddingClient`.** `NewChat` and `ChatClient` are removed, and the old
   embeddings `Client` becomes `EmbeddingClient`. The method sets are
   unchanged: `Client` has `Model`, `Complete`, `ListModels` and
   `LatestInFamily`; `EmbeddingClient` has `Model`, `DefaultDimensions` and
   `Embed`.
2. **openai: one shared `Option` set; `New` rejects `WithDimensions`.** As
   in `google` (ADR-0027: "an option the constructor does not accept is a
   config error, never silently ignored"), `WithDimensions` records that it
   was set, and `New` returns a config `*llms.ProviderError` naming it
   ("WithDimensions applies only to NewEmbeddings"). It is rejected when set
   at all, including `WithDimensions(0)`, so the check does not depend on
   the value.
3. **anthropicvertex: `WithVertex(project, location)` and `WithBaseURL`.**
   They replace `WithProjectID` + `WithRegion` and `WithEndpoint`, with the
   same argument order and validation message shape as `google.WithVertex`.
   Both values must be non-empty. The location is what the SDK calls
   `region`; behavior is unchanged (the PSC precedence of ADR-0029 is
   untouched; only the option's name changes).
4. **anthropicvertex: `WithContext` is removed.** The context was passed to
   `vertex.WithCredentials`, which uses it only to build the SDK's HTTP
   client around the converted token source. With app-supplied credentials
   that does no discovery and no network call, so there is nothing for a
   caller's context to cancel. Token refreshes don't see it either: they go
   through the app's `TokenProvider` (`oauth2adapt` calls it with
   `context.Background()`). `New` now passes `context.Background()`, as
   `google.New` already does for `genai.NewClient`.
5. **No aliases for the old names.** The toolkit is pre-v1 and v0.x minor
   versions may break (README "Versioning"). A deprecated alias added now
   would have to stay for all of v1. ADR-0027 kept `vllm` because a package
   path cannot be renamed in place; these are identifiers in a package that
   stays.
6. **A test keeps `vllm` in export parity with `openaicompat`.**
   `llms/providers/vllm/parity_test.go` parses both packages and fails if
   either exports a top-level identifier the other lacks, or if a function's
   parameter or result types differ. Methods come with the type aliases and
   are not compared.

## Alternatives considered

- **Keep `openai` as it is and document the exception.** The exception is
  exactly what the audit flagged; at v1 it would be permanent.
- **Rename only the chat side (`New` → chat) and keep embeddings as
  `NewEmbedding`/`Embedder`.** `google` already set the names
  (`NewEmbeddings`, `EmbeddingClient`); matching them is the point.
- **Separate option types for chat and embeddings in `openai`.** Type-safe,
  but `google` chose one shared set in ADR-0027 and a shared set is what
  apps already pass. Two shapes for one concept across providers is the
  problem this ADR removes.
- **Keep `WithContext` and document it.** It would be the only
  construction-time context in the toolkit, and it controls nothing.

## Consequences

- **Breaking changes**, for the release notes:
  - `openai.NewChat` → `openai.New`; `openai.ChatClient` → `openai.Client`.
  - `openai.New` (embeddings) → `openai.NewEmbeddings`; the old
    `openai.Client` → `openai.EmbeddingClient`.
  - `openai.New` with `WithDimensions` is a config error.
  - `anthropicvertex.WithProjectID(p)` + `WithRegion(r)` →
    `WithVertex(p, r)`; `WithEndpoint` → `WithBaseURL`; drop `WithContext`.
- **The `openai.New` meaning change is caught by the compiler** wherever
  the result is used as an embeddings client: the new `*Client` has no
  `Embed` method and does not satisfy `llms.EmbeddingClient`. Code that
  passes `WithDimensions` to the old `openai.New` fails at construction
  with a config error naming the option. No known consumer builds OpenAI
  embeddings.
- Known call sites: Maestro `pkg/agent/internal/llmadapter/llmadapter.go:66`
  (`NewChat`), Morris `internal/llm/vertex/vertex.go:43-50` (`WithContext`,
  `WithRegion`, `WithProjectID`, `WithEndpoint`), buggrit
  `internal/llm/llm.go:226` (`NewChat`). All four consumers are compiled
  against a release candidate before migration coverage is called complete
  (#84).
- No wire or behavior change: requests, auth and PSC precedence are the
  same. `TestVertexPSCPrecedenceAndMiddleware` pins the project and
  location reaching the Vertex path in the right order.

## Amends

- [ADR-0015](0015-vllm-provider.md): the hosted/self-hosted swap in its
  Consequences is now `openaicompat.New(...)` vs `openai.New(...)`.
- [ADR-0029](0029-vertex-injected-client-auth.md): `anthropicvertex`'s
  `WithEndpoint` is now `WithBaseURL`. The decision (the base URL is
  applied after the Vertex option, so it wins over the regional URL) is
  unchanged.

## References

- `llms/providers/openai/chat.go` (`New`, `Client`),
  `llms/providers/openai/embeddings.go` (`NewEmbeddings`,
  `EmbeddingClient`), `llms/providers/openai/openai.go` (the shared
  `Option` set, `WithDimensions`)
- `llms/providers/anthropic/anthropicvertex/anthropicvertex.go`
  (`WithVertex`, `WithBaseURL`, `New`)
- `llms/providers/google/google.go` (`WithVertex`, `WithBaseURL`, the
  `embedOnly` check this follows)
- Tests: `llms/providers/openai/chat_test.go`
  (`TestNewConfigErrors`, `TestNewRejectsEmbeddingOptions`),
  `llms/providers/anthropic/anthropicvertex/anthropicvertex_test.go`
  (`TestNewConfigValidation`, `TestVertexPSCPrecedenceAndMiddleware`),
  `llms/providers/vllm/parity_test.go`
  (`TestExportParityWithOpenAICompat`)
- [ADR-0027](0027-pre-v1-api-cleanup.md) (the shared `google.Option`, the
  permanent `vllm` alias)
