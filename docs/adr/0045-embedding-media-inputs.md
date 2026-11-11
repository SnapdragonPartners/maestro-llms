# 0045. Image and document inputs for embeddings

- **Status:** Accepted; amends [ADR-0006](0006-validation-error.md) (adds `ValidationEmbeddings`) and [ADR-0009](0009-vertex-backend-psc-and-gemini-embeddings.md) (the Vertex single-input rule)
- **Date:** 2026-11-11

## Context

Morris wants to embed images with Gemini embeddings on Vertex AI (#164).
Embeddings are text-only today: `llms.EmbeddingInput` has `ID`, `Text` and
`Title`. The image and document parts from ADR-0040 and ADR-0041 apply
only to chat (user messages and tool results).

What the providers offer (checked 2026-11-11):

- **`gemini-embedding-2`** (Gemini API and Vertex) is natively multimodal:
  text, images, PDF documents, video and audio map into one vector space,
  and a multi-part content yields **one** embedding. The
  [Vertex model page](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/models/gemini/embedding-2)
  lists at most 6 images per prompt, as PNG, JPEG, WebP, BMP, HEIC, HEIF or
  AVIF, with no stated per-file size limit. The older preview-era
  [embeddings page](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/embeddings/get-multimodal-embeddings)
  lists only JPEG and PNG. These figures go in this ADR and the live test,
  not in code (no per-model data, ADR-0040 item 6).
- **genai v1.77** already carries media. On Vertex, `Models.EmbedContent`
  sends every model whose name contains `gemini` (except the bare name
  `gemini-embedding-001`) or `maas` through the `:embedContent` API, with
  a full `Content` (inline data included). That path takes **one content
  per call**: more returns a plain `fmt` error from the SDK. The match is
  on the raw model string, so a resource path whose project or location
  contains `gemini` or `maas` also takes this route. Other models go
  through the predict API, where genai's `tContentsForEmbed` keeps only
  the **first part's text** of each content: later parts are silently
  dropped, and a content whose first part is media fails with an untyped
  SDK error. On the Gemini API backend the contents pass through whole.
- **Usage.** On Vertex, genai fills `ContentEmbedding.Statistics.TokenCount`
  on both routes: from `usageMetadata.promptTokenCount` (with per-modality
  details) on `:embedContent`, and from the prediction's statistics on
  predict. The Gemini API reports no token count. The adapter leaves
  `Usage` zero today.
- **`multimodalembedding@001`** (Vertex) uses predict instances with
  `image.bytesBase64Encoded`, which genai does not produce.
- **OpenAI embeddings** are text-only.

A defect found while scoping: `google`'s `batchLimit` treats only
`gemini-embedding-001` as single-input, so a batch to `gemini-embedding-2`
on Vertex fails today, on text, with the SDK's untyped error instead of
the typed `bad_request` the spec requires ("Provider clients return a
typed validation/config error when `Inputs` exceeds the provider or model
batch limit").

## Decision

1. **`Parts []ContentPart` beside `Text`** on `llms.EmbeddingInput`.
   `Text` stays the common case. A new field is additive under the v1
   contract. **One input yields one vector**, whatever the number of
   parts: on Gemini a multi-part content is one embedding.
2. **Text, image and document parts only.** A text part must be
   non-empty; a media part must pass `media.Check` (ADR-0040 item 4). No
   tool calls, tool results or reasoning. Video and audio are not in
   scope (there is no part type for them).
3. **`Text` and `Parts` are exclusive.** An input that sets both is
   invalid, rather than one taking precedence (ADR-0041 item 3). An input
   with neither keeps today's behavior: it is sent, and the provider
   decides.
4. **`Title` and `Task` still apply** to an input with `Parts`, with their
   existing meaning and limits (ADR-0009: `Title` is per call in genai, so
   it is honored for a single-input request with
   `EmbeddingTaskRetrievalDocument`).
5. **One check, shared.** A new `media.CheckEmbeddingInput` is the choke
   point for items 2 and 3. The new validation middleware and every
   embedding adapter call it, so each fails closed on its own.
6. **`ValidationEmbeddings`** (amends ADR-0006, which ruled it out because
   embeddings had no app-neutral structural rule; item 5 is one). It
   checks every input with `CheckEmbeddingInput` and returns the existing
   `*ValidationError`, so `errors.As` works for both chat and embeddings.
   Its message reads "invalid embedding request: ..." (an unexported field
   picks the prefix; a `ValidationError` built by a caller keeps the chat
   prefix, so nothing visible to callers changes). `RecommendedEmbeddings`
   puts it outermost, as `RecommendedChat` does.
7. **Per provider, fail before the call; no per-model prediction**
   (ADR-0022, ADR-0040 item 6).
   - `google`, Gemini API and the Vertex `:embedContent` route: the parts
     map 1:1 into one `genai.Content`: text parts as text, media as inline
     data with the normalized media type. Any image or document type
     passes; a model that cannot take it returns the provider's error.
   - `google`, the Vertex predict route (`text-embedding-005`,
     `multimodalembedding@001` and other models item 9's predicate does
     not send to `:embedContent`): genai would keep only the first part's
     text, so the adapter joins the text parts by a blank line into one
     text part and rejects an image or document as `bad_request` before
     the call. This is a property of the API route the SDK chooses, from
     the same predicate as item 9, not a prediction about the model.
   - `openai`: text parts are joined by a blank line (`media.JoinText`,
     as ADR-0041 does for text-only tool messages); an image or document
     is `ProviderError{Kind: BadRequest}` before the call.
8. **`MaxInputBytes` measures text only.** It is the no-silent-truncation
   guard (ADR-0009) and protects text from being cut, so it measures the
   text **as sent**: on the Vertex predict route, the joined string from
   item 7, separators included; on the `:embedContent` route and the
   Gemini API, where text parts are sent as separate parts, the sum of
   their bytes. Image and document bytes do not count and are bounded by
   the provider. An app sized for text would otherwise reject nearly
   every image. The check runs on the translated request, so it cannot
   drift from what the adapter sends; boundary tests cover parts that fit
   only without separators.
9. **The Vertex route and single-input rule follow genai's routing**
   (amends ADR-0009). One unexported predicate mirrors genai's
   `tIsVertexEmbedContentModel` on the **raw** model string (contains
   `gemini` and is not exactly `gemini-embedding-001`, or contains `maas`)
   and picks the route for item 7. On Vertex, a model on the
   `:embedContent` route is single-input, and so is `gemini-embedding-001`
   by its base name (the existing check, which also covers a full path to
   it); `len(Inputs) > 1` is a typed `bad_request` before the call. On the
   Gemini API, only `gemini-embedding-001` stays single-input. Tests cover
   bare names, full resource paths, and a project or location containing
   `gemini` or `maas`. A predicate test pins the genai version's behavior,
   so an SDK bump that changes routing shows up as a failing test. This
   fixes the defect above for text and is required for media.
10. **Estimate, no bound.** `DefaultEstimator.EstimateEmbeddings` counts
    an input's `Parts` with the chat rules (text by bytes, media from the
    ADR-0040 figures, per-part overhead). `BoundEmbeddings` stays
    unbounded (ADR-0043).
11. **Reported usage on Vertex.** When every returned embedding carries
    a positive `Statistics.TokenCount`, `google` sets
    `Usage.EmbeddingTokens` and `Usage.TotalTokens` to their sum (rounded
    up per embedding), as `openai` does from `prompt_tokens`. If any
    embedding lacks one (the Gemini API, a partial response, or an
    `:embedContent` response without `usageMetadata`, for which genai
    still sets empty statistics), `Usage` stays zero rather than
    undercounting, and the limiter keeps the
    estimate (spec "Usage Metadata": leave fields zero when the provider
    does not report them).
12. **`testllm`.** `FakeEmbeddingClient`'s deterministic vector hashes
    each part (type, media type, bytes, text), so equal inputs give equal
    vectors and an image differs from its media type alone.
    `copyEmbeddingRequest` deep-copies `Parts` and their bytes.

## Consequences

- Additive under the v1 contract: one field on `EmbeddingInput`, one new
  middleware constructor, one new internal check. Three side effects the
  contract allows: `EmbeddingInput` stops being comparable with `==`
  (comparability is promised only for `ModelRef` and `Usage`);
  `middleware.ValidationError` gains an unexported field, which breaks
  only unkeyed literals such as `ValidationError{"reason"}` (the README
  rules those out: "Don't use unkeyed struct literals for this package's
  types"); and `RecommendedEmbeddings` now rejects a malformed input
  before retry and rate-limit work. All three go in the release notes.
- **Behavior change on Vertex:** a multi-input request to a `gemini*` or
  `maas` embedding model other than `gemini-embedding-001` now fails with
  a typed `bad_request` before the call instead of the SDK's untyped
  error. Such a request never succeeded, so no working caller breaks.
- A reader of `EmbeddingInput.Text` (logging, audit, Morris's content
  classification) sees an empty string for a multimodal input and must
  read `Parts`.
- Images in embeddings bill as image tokens; `MaxInputBytes` does not
  limit their size (item 8).
- **Behavior change on Vertex:** Gemini and text embeddings on Vertex
  start reporting `Usage.EmbeddingTokens`/`TotalTokens` (item 11), text
  included. A rate limiter in the chain now commits these actuals instead
  of keeping the estimate. Release notes.
- **Behavior change on the Vertex predict route:** text-only `Parts` are
  joined rather than truncated to the first part, and media there is a
  typed `bad_request` (item 7). Text-only callers (`Text` set) see no
  change.

## Alternatives considered

- **`Parts` takes precedence when set.** Rejected (item 3).
- **`MaxInputBytes` counts media bytes** (the issue's first suggestion),
  or a separate `WithMaxMediaBytes`. Rejected for item 8; a media cap has
  no consumer yet and is additive later.
- **No validation middleware, adapter checks only.** Rejected: chat has
  one, and a malformed input would spend retry and rate-limit work first.
- **A new `EmbeddingValidationError` type.** Rejected: callers would need
  two `errors.As` checks for one condition.
- **Images only, no documents** (the issue's scope). Rejected:
  `gemini-embedding-2` embeds PDFs, `ContentDocument` and `media.Check`
  already exist, and the adapter maps documents with the same code.
- **Supporting `multimodalembedding@001`.** Rejected: genai cannot send
  its request shape, and a hand-built predict call would be a second
  client for a superseded model.
- **Passing parts through on the Vertex predict route** and letting the
  provider decide. Rejected: genai drops every part after the first
  without an error, so the provider never sees them (item 7).
- **Matching the route on the base model name.** Rejected: genai matches
  the raw string, so a path whose project contains `maas` would pass a
  base-name check and still hit the SDK's untyped error (item 9).
- **Mapping each part to its own vector.** Rejected: Gemini returns one
  embedding per content, and the response contract is one vector per
  input.

## Implementation plan

One PR (`feat/embedding-media`), then the v1.4 docs wrap and tag.

1. `llms/embedding.go`: `Parts` with doc comments (items 1-4).
2. `llms/internal/media`: `CheckEmbeddingInput` (items 2, 3) with tests.
3. `llms/middleware`: `ValidationEmbeddings` and the embedding message
   prefix (item 6); `RecommendedEmbeddings` puts it first; the
   "intentionally no ValidationEmbeddings" comments go.
   `EstimateEmbeddings` counts `Parts` (item 10). Tests for each.
4. `llms/providers/google/embeddings.go`: check inputs, the route
   predicate and single-input rule (item 9), map `Parts` per route
   (item 7), `MaxInputBytes` on the text as sent (item 8, with a
   separator boundary test on predict), usage from
   statistics (item 11). Unit tests against the recorded request and
   response: media bytes and type on the wire, one content for a
   multi-part input on `:embedContent`, joined text and a pre-call media
   rejection on predict, the typed batch error on Vertex (bare names,
   paths, `maas`/`gemini` substrings) and none on the Gemini API, and
   usage summed from statistics, zero when any is missing.
5. `llms/providers/openai/embeddings.go`: join text parts, reject media
   (item 7), with tests.
6. `llms/testllm`: hashing and the deep copy (item 12), with tests.
7. Live test on Vertex, gated on `VERTEX_PROJECT`, model
   `gemini-embedding-2` (override by env): an image-only input, an image
   plus text input (one vector), a one-page PDF, and a two-input request
   rejected as `bad_request` before the call; each media case checks that
   `Usage.EmbeddingTokens` is positive. A text-only `Parts` input on
   `text-embedding-005` (predict route) checks the joined text gives the
   same vector as the same string in `Text`. The model name and limits
   go in the test and this ADR's Verification section.
8. Docs: spec "Embeddings API" (`Parts`, `ValidationEmbeddings`, the
   single-input rule), ADR-0006 and ADR-0009 status lines ("amended by
   0045"), the ADR index, and `MAESTRO_DIVERGENCES.md` row X17
   (informational: Maestro embeds text only).
9. Status moves to Accepted in the same PR once live tests pass (done).

## Verification

Live tests on 2026-11-11, project `morris-qa`
(`llms/internal/livetest/embedding.go`, run from
`google/vertex_integration_test.go`):

- `TestIntegrationVertexEmbeddingMedia`, `gemini-embedding-2` on `global`:
  an image (256x256 red PNG), the image with a caption, and a one-page
  PDF each gave one 3,072-dimension vector, with reported usage of 258,
  263 and 258 tokens. The image was nearer "A solid red square." than an
  unrelated sentence (cosine 0.4608 against 0.2645), so the bytes were
  embedded. `DefaultEstimator`'s estimate was at least the reported
  tokens in every case, and a two-input request was a `bad_request`
  before the call. Availability depends on the name: `gemini-embedding-2`
  returned 404 on `us-central1` and was served on `global`;
  `gemini-embedding-2-preview` passed the same test on `us-central1` and
  returned 404 on `global`. The test defaults to the GA name on `global`
  (`VERTEX_MULTIMODAL_EMBEDDINGS_MODEL` / `_LOCATION` override).
- `TestIntegrationVertexEmbeddingJoinedText`, `text-embedding-005` on
  `us-central1` (predict route): two text parts gave the same vector as
  the same text joined by a blank line in `Text` (cosine at least
  0.9999), each reporting 14 tokens from the prediction statistics.
- Regression: `TestIntegrationVertexGeminiEmbeddings`
  (`gemini-embedding-001`) and `TestIntegrationOpenAIEmbeddings` pass.

Not verified live: media on the Gemini API backend (the unit tests check
the wire shape; Morris uses Vertex), and the 6-images-per-prompt limit.
