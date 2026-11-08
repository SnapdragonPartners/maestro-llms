# 0040. Image and document content parts

- **Status:** Accepted; amends [ADR-0008](0008-prompt-cache-hint.md) (`CacheBreakpoint` on media)
- **Date:** 2026-11-08

## Context

maestro-agents (#147) needs to send images and documents to a model: its
intake agent sends PDF bytes, on Gemini on Vertex in Morris. Until now the
conversation model was text, tool calls, tool results and reasoning. Every
adapter already rejected an unknown `ContentPartType` with a
`ProviderError{Kind: BadRequest}` before the call, so no release dropped a
part silently. The spec always expected this growth ("the content shape
also allows later multimodal expansion without changing the top-level
request contract"), and ADR-0032 lists `ContentPartType` as a type that
grows in minor releases.

The providers differ in what they accept:

| Provider | Images | Documents |
|---|---|---|
| Anthropic (also on Vertex) | `image` block, base64: JPEG, PNG, GIF, WebP | `document` block: base64 PDF, or a text source for plain text |
| OpenAI Responses | `input_image` with a data URL | `input_file` with a data URL (PDF) and a file name |
| Gemini (API and Vertex) | `inlineData`, any media type | `inlineData`, any media type |
| Ollama `/api/chat` | message `images` array (base64) | none |
| OpenAI Chat Completions (`openaicompat`) | `image_url` part with a data URL | backend-dependent |

Two parts of the toolkit reason about content and had to learn about
media: the rate-limit `DefaultEstimator`, which charged a media part only
its 8-token per-part overhead, and `compact.Summarize`, which renders
dropped turns as text for a summarizer.

## Decision

1. **Two part types sharing one payload.** `ContentImage` (`"image"`) and
   `ContentDocument` (`"document"`), each with `ContentPart.Media *Media`
   (`MediaType string`, `Data []byte`), and the constructors
   `llms.Image(mediaType, data)` and `llms.Document(mediaType, data)`.
   Separate types because the providers distinguish them (Anthropic image
   vs document blocks, OpenAI `input_image` vs `input_file`), and "this
   provider takes no documents" reads more clearly as a part-type error than
   as a MIME-type rule. A single `ContentMedia` part discriminated by MIME
   type was the alternative.
2. **Inline bytes only.** `Data` is raw bytes; adapters encode them (base64
   or a data URI). URLs and provider file references are not part of this
   cut: they work differently everywhere (Gemini wants the File API or
   `gs://` URIs, Ollama has none), and a URL makes the provider fetch on the
   app's behalf. Either can be added later as new `Media` fields.
3. **User messages only.** Media is allowed on `RoleUser` messages.
   `System` stays text-only, and assistant and tool messages reject media.
   Media inside tool results comes with #148, through the tool result
   rather than as a tool-message part.
4. **One structural check, shared.** `llms/internal/media.Check` is the
   choke point the validation middleware and every adapter call: a payload
   is present, `Data` is non-empty, `MediaType` parses as `type/subtype`,
   an image part has an `image/*` type, and a document part does not. Media
   types are matched case-insensitively with parameters dropped. Adapters
   check again, so they fail closed without the validation middleware.
5. **Per provider, fail before the call.** Each adapter maps what its
   provider documents and rejects anything else with
   `ProviderError{Kind: BadRequest}` before any network call. Nothing is
   dropped silently, and nothing is moved somewhere else in the request.
   - Anthropic: images `image/jpeg`, `image/png`, `image/gif`,
     `image/webp`; documents `application/pdf` (base64 source) and
     `text/plain` (text source; the bytes must be UTF-8).
   - OpenAI Responses: the same four image types as `input_image` (detail
     `auto`, the API default); `application/pdf` as `input_file` with the
     fixed file name `document.pdf`. Each media part is its own user
     message item, in order, the same as text parts.
   - Gemini: any media type as `inlineData`. Gemini's accepted set is broad
     and model-dependent, so the type passes through and an unsupported one
     is the provider's 400.
   - Ollama: images in the message's `images` array, any image type;
     documents rejected.
   - `openaicompat`: images as `image_url` parts with a data URI, any image
     type, in a content-part array (a text-only user message keeps the
     plain-string form); documents rejected, because Chat Completions
     servers differ on file input.
6. **Capability per provider, not per model.** Whether a specific model
   accepts images is not predicted (ADR-0022 declined per-model capability
   prediction; there is no model registry). A model without vision
   surfaces as the provider's 400, or, on some servers, as an answer that
   ignores the image. maestro-agents accepted this on #147.
7. **`CacheBreakpoint` on media.** Anthropic honors `CacheBreakpoint` on
   image and document parts as it does on text (`cache_control` on the
   block, counted toward the four-marker cap). This is the documented
   pattern for a document kept in context across turns: the tool loop
   resends the whole transcript every turn, so mark the document part, or
   the last part of the prompt that doesn't change, and later turns read it
   from the cache. Other providers ignore the hint, as before.
8. **The estimator counts media from documented figures.**
   `DefaultEstimator` stays provider- and model-neutral:
   - An image is sized from its header (PNG, JPEG and GIF through the
     standard library; WebP by reading its RIFF header): the largest of
     `pixels/750` (Anthropic's formula without its resize cap),
     `ceil(w/32)·ceil(h/32)·1.2` (OpenAI's patch formula) and 1,120
     (Gemini 3's default per-image cost), capped at 36,000 (OpenAI's
     largest accepted image, 30,000 patches at 1.2). An image whose header
     doesn't parse is charged the cap.
   - A PDF costs 4,000 tokens per page, a typical-high figure rather than a
     strict bound (Anthropic documents 1,500–3,000 text tokens per page
     plus the page as an image). The page count is the larger of the page
     objects and the largest `/Count` entry visible in the file; when
     neither is visible (compressed object streams), the bytes are counted
     as text.
   - Any other document counts its bytes as text. Every document adds 100
     tokens for the provider's framing.
   - Sums saturate, so a very large estimate can't wrap into an admitted
     small one.

   The figures cover current models. Older models may bill images
   differently as they reach end of life, and an app on such a model, or
   one that knows its page counts, supplies its own `TokenEstimator`. The
   rate-limit middleware still commits actual usage after each call.
9. **Compaction uses a placeholder.** `compact.Summarize` does not send
   media to the summarizer: the summarizer model may not accept it, and
   resending it would undo the compaction. Each media part in a dropped
   turn is rendered as one line, for example
   `[user attached an image (image/png, 5 bytes); not shown]`. `KeepLast`
   keeps or drops whole messages, so media in kept messages is unchanged.
10. **Fakes copy media.** `testllm` deep-copies `Media` (including `Data`)
    when it records a request.

## Consequences

- Additive under the v1 contract: new `ContentPartType` values (ADR-0032),
  a new field on `ContentPart`, a new `Media` type and two constructors.
- Code that switches on part type, such as Morris's audit and content
  classification, starts seeing `image` and `document` parts. Such switches
  already need a `default` case (ADR-0032). This goes in the release notes.
- Requests with media reserve far more from the limiter than before, which
  is the point: before, an image counted as 8 tokens.
- Media is resent with every turn of a tool loop; `CacheBreakpoint` is the
  cost lever on Anthropic, and the other providers' automatic or separate
  caching applies as before.

## Alternatives considered

- **One `ContentMedia` part type** discriminated by MIME type. Rejected for
  the reasons in item 1; it would also make "document" a property of the
  MIME type, which Gemini and Anthropic define differently.
- **A provider-aware estimator** (an optional interface receiving the
  `ModelRef`, so each provider uses its own maximum). The middleware does
  know the provider, but the ranges are wide within each provider
  (OpenAI's tile and patch models, Anthropic's standard and high-resolution
  tiers), so each branch would still need the provider's largest figure,
  and the per-provider constants would go stale as models change. Images
  with readable dimensions already get a close bound. It can be added later
  as an optional interface if a consumer needs it.
- **A strict per-page PDF bound** (about 7,800 tokens on Anthropic's
  high-resolution tier). Rejected: a 100-page PDF would reserve about 780k
  tokens and be refused by most limiters, against a typical real cost of
  2,000–4,000 per page.
- **Moving unsupported media elsewhere** (into text, or another message).
  Rejected: a silent rewrite changes what the model sees. The pre-call
  error is the contract.

## Verification

Live tests (`llms/internal/livetest/media.go`) send a 256×256 red PNG and a
generated one-page PDF with a code word, check the answer, and check that
`DefaultEstimator`'s input estimate is at least the reported input tokens.
They passed on 2026-11-08 against Anthropic (`claude-haiku-4-5`; image,
PDF and text document), Anthropic on Vertex (`claude-opus-4-6`), OpenAI
(`gpt-6-luna`), Gemini on Vertex (`gemini-2.5-flash`), Ollama
(`mistral-small3.2`) and LunaRoute (`glm-5.3-vision`). The Gemini API
backend shares the converter with Gemini on Vertex and was not run (no key
in that environment).
