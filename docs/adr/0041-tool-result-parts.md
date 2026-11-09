# 0041. Image and document content in tool results

- **Status:** Accepted; amends [ADR-0040](0040-image-and-document-parts.md) (media in a tool result)
- **Date:** 2026-11-08

## Context

maestro-agents' first agent calls a `get_document` tool that returns the
statement PDF so the model can read it (#148). Until now a tool result was
a string (`llms.ToolResult.Content`, `toolloop.ToolResult.Content`). The
workaround, putting the document in a following user message, breaks the
tool-call/result pairing and makes the model treat the document as user
input rather than as the tool's answer. Any tool that returns a
screenshot, a chart, a rendered page or a file has the same need.

ADR-0040 added image and document parts on user messages and left media in
tool results to this change: "through the tool result rather than as a
tool-message part". The spec always expected the growth ("Structured or
multimodal tool results, if added, extend `ToolResult` with new fields
without changing the core round-trip semantics").

The providers differ in what a tool result can carry:

| Provider | Tool result content |
|---|---|
| Anthropic (also on Vertex) | `tool_result` content blocks: text, image, document |
| OpenAI Responses | `function_call_output` output list: `input_text`, `input_image`, `input_file` |
| Gemini (API and Vertex) | `functionResponse.response` (JSON) plus `functionResponse.parts` (inline data), on Gemini 3 |
| Ollama `/api/chat` | tool message `content`: a string |
| OpenAI Chat Completions (`openaicompat`) | tool message `content`: a string, or an array of text parts |

## Decision

1. **`Parts []ContentPart` beside `Content`** on both `llms.ToolResult` and
   `toolloop.ToolResult`. `Content` stays the common case. Changing
   `Content`'s type would break every caller; a new field is additive
   under the v1 contract.
2. **Text, image and document parts only.** No tool calls, tool results or
   reasoning inside a result, so nothing nests. A text part must be
   non-empty; a media part must pass `media.Check` (ADR-0040 item 4).
3. **Content and Parts are exclusive.** A result that sets both is
   invalid, rather than one taking precedence: a precedence rule would
   silently drop the other field. A result with neither is an empty
   result, as before.
4. **One check, shared.** `llms/internal/media.CheckToolResult` is the
   choke point for items 2 and 3. The validation middleware, `toolloop`
   and every adapter call it, so each fails closed on its own.
5. **Per provider, fail before the call; no fallback.** Each adapter maps
   what its provider documents and rejects anything else with
   `ProviderError{Kind: BadRequest}` before any network call. Media is
   never moved into a following user message: that is the pairing break
   the issue asked us to avoid.
   - Anthropic: `tool_result` content blocks, with the same media types as
     in a user message (images JPEG, PNG, GIF, WebP; documents PDF and
     UTF-8 plain text). `CacheBreakpoint` on a result part is honored and
     counts toward the four-marker cap in conversation order, so a
     document a tool returned can be cached across the turns of a loop
     (ADR-0040 item 7).
   - OpenAI Responses: `function_call_output` with an output list, the
     same media types and encoding as in a user message (`input_image`
     with detail `auto`, PDF as `input_file` named `document.pdf`). A
     `Content`-only result keeps the plain-string output.
   - Gemini: the text parts, joined by a blank line, are the response's
     `content` field (beside `is_error`, as before), and each image or
     document is a `functionResponse` part with inline data, any media
     type. Gemini keeps the response JSON and its parts apart, so the
     order of text relative to media is not kept.
   - Ollama and `openaicompat`: tool messages are text-only here. Text
     parts are joined by a blank line into the message content; an image
     or document is rejected. Joining text loses nothing the model would
     see; it is not a fallback.
6. **Capability per provider, not per model** (ADR-0022, ADR-0040 item 6).
   Multimodal function responses need Gemini 3. Verified on 2026-11-08:
   on the Gemini API, `gemini-2.5-flash` returns a 400 ("Multimodal
   function responses are not supported for this model"), but on Vertex
   it accepts the request, bills the media and answers as if it was not
   there. The adapter does not predict which models support this; the
   docs say to use Gemini 3 on Vertex.
7. **`toolloop` passes Parts through unchanged.** The loop copies `Parts`
   into the transcript's `llms.ToolResult` without copying the slice or
   the media bytes (copying a large PDF per call buys nothing), so neither
   the executor (after returning) nor an `OnToolCall` observer may modify
   them. An executor result that fails `CheckToolResult` is a contract
   violation and returns `OutcomeToolError` naming the tool, the same as
   `Stop` with `IsError`, rather than surfacing later as a provider
   `bad_request`.
8. **The toolkit's other readers learn Parts.** `DefaultEstimator` counts
   result parts like message parts (text by bytes, media from ADR-0040's
   figures). `compact.Summarize` renders a result's text parts (each
   truncated on its own) and a placeholder line per media part, for
   example `[tool returned a document (application/pdf, 8 bytes); not
   shown]`. `testllm` deep-copies result parts and their bytes.

## Consequences

- Additive under the v1 contract ("New fields on exported structs ... are
  additive"): one new field on each `ToolResult`. Two side effects the
  contract already allows: both `ToolResult` types stop being comparable
  with `==` (comparability is promised only for `ModelRef` and `Usage`),
  and `toolloop.Outcome` moves one field (`TerminalResult`) for the
  `fieldalignment` lint, which matters only to unkeyed struct literals,
  which the contract rules out. Both go in the release notes.
- A reader of `ToolResult.Content` (logging, audit, Morris's content
  classification) sees an empty string for a multimodal result and must
  read `Parts`. This goes in the release notes.
- A tool loop resends a tool's media with every later turn, like media in
  a user message; `CacheBreakpoint` is the cost lever on Anthropic.
- An app on Gemini must run a Gemini 3 model for media in tool results;
  on Vertex an older model fails silently (item 6).

## Alternatives considered

- **`Parts` takes precedence when set** (the issue's other option).
  Rejected for item 3: a result that sets both is a bug, and a precedence
  rule hides it.
- **Moving media into a following user message** for providers without
  multimodal tool results. Rejected: it breaks pairing and changes what
  the model sees (item 5).
- **Text parts as a Chat Completions text-part array** on `openaicompat`.
  Rejected: servers differ on array content in tool messages, and a
  joined string is equivalent to the model.
- **Copying Parts in `toolloop`.** Rejected (item 7): the loop already
  hands the executor's strings through unchanged, and a defensive copy of
  every returned document costs memory for no contract the loop needs.

## Verification

Live tests (`llms/internal/livetest/media.go`: `ToolResultPDF`,
`ToolResultImage`, `ToolResultCachedPDF`) run a tool loop whose tool
returns a one-page PDF or a red PNG, check the answer, and check
`DefaultEstimator`'s estimate for the last request is at least the
reported input tokens. They passed on 2026-11-08 against Anthropic
(`claude-haiku-4-5`, including a cache marker on the document), Anthropic
on Vertex (`claude-opus-4-6`), Gemini on Vertex and the Gemini API
(`gemini-3-flash-preview`), and OpenAI (`gpt-6-luna` for the PDF;
`gpt-5.1` and `gpt-4.1` for the image). `gpt-6-luna` named the red image
gray in the tool result, and black once for the same image in a user
message: a weakness of that model's vision on this image, not of the
mapping. Ollama (`mistral-small3.2`) and LunaRoute
(`deepseek-4.1-flash`) passed their tool-loop scenarios with the changed
tool-message code.
