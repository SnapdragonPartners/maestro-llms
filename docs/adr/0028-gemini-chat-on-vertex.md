# 0028. Gemini chat on Vertex AI

- **Status:** Accepted
- **Date:** 2026-10-18

## Context

ADR-0009 put Claude (`anthropicvertex`) and Gemini **embeddings** on Vertex
AI, but `google.New` always built the genai client for the Gemini API with
an API key. So the toolkit could not send Gemini **chat** to Vertex, even
though Gemini is first-party there and needs no Model Garden enablement
(#104). ADR-0027 already gave `New` and `NewEmbeddings` one `google.Option`
type, including `WithVertex(project, location)` and `WithCredentials`, and
had `New` reject those two options until this change.

## Decision

- **`google.New` accepts the Vertex options.** `WithVertex` +
  `WithCredentials` select `genai.BackendVertexAI`. `WithAPIKey` still
  selects the Gemini API. No signature or type changes; ADR-0027's
  temporary rejection is removed.
- **One backend selector for both constructors.** `settings.clientConfig`
  validates the shared options and builds the genai config: model and
  retries, the fail-closed ambiguity check (`WithAPIKey` with either Vertex
  option), the Vertex requirements (non-empty project and location,
  non-nil credentials with a `TokenProvider`, no ADC discovery), and
  `WithBaseURL` / `WithHTTPClient`. `NewEmbeddings` adds only its
  truncation contract on top, so chat and embeddings cannot drift apart.
- **PSC precedence is ADR-0009's rule, unchanged.** genai uses
  `Credentials` only when it builds the HTTP client itself. An injected
  `WithHTTPClient` replaces that client, so on Vertex it **must carry Google
  auth itself** (for example via `httptransport.AddAuthorizationMiddleware`),
  exactly as `anthropicvertex.WithHTTPClient` requires. `WithCredentials`
  stays required, so no configuration falls back to ambient ADC. This is now
  stated on `google.WithHTTPClient`; it was previously documented only for
  the Anthropic path.
- **Model listing strips the Vertex prefix.** On Vertex, genai lists the
  Google publisher catalog (base models only) with names
  `publishers/google/models/<id>`, and `stripPrefix` now removes that as
  well as the Gemini API's `models/`. `ModelInfo.ID` stays usable as
  `WithModel(id)` on either backend, and `familyOf` / `LatestInFamily`
  (ADR-0012) work unchanged.

### Backend audit

Checked against genai v1.75 and live on Vertex (`morris-qa`, location
`global`, 2026-10-18):

| Area | Finding |
|---|---|
| Thought signatures (ADR-0010) | Same `Part.ThoughtSignature` field on both backends; a 326-byte signature round-tripped in the live tool-use test and the toolloop scenarios. |
| Cached input (ADR-0023) | Same `cachedContentTokenCount`; live: 9557 input, 9194 cache read on repeat calls. |
| Reasoning (ADR-0022) | `thinkingBudget` (2.5) and `thinkingLevel` (3) both honored live. |
| `ProviderParams` (ADR-0024) | The per-request `HTTPOptions.ExtrasRequestProvider` merges field by field over the client's options, so a PSC `BaseURL` survives; live override of `generationConfig.maxOutputTokens` took effect. |
| Context overflow (ADR-0026) | The unparenthesized Vertex wording classifies as `ErrorKindContextLength` live, as the #79 fixture predicted. |
| Model listing (ADR-0012) | Needed the prefix fix above. `Created` stays zero on both backends. |
| Field support | genai v1.75 rejects no request field the adapter sends on Vertex. |

## Consequences

- Additive: existing API-key callers see no change. A `google.New` call
  that passed `WithVertex` / `WithCredentials` used to fail with a config
  error and now builds a Vertex client.
- Morris (or any Vertex consumer) can run Gemini chat on the same
  credentials it already passes to `anthropicvertex` and `NewEmbeddings`.
- Live tests `TestIntegrationVertexGemini*` (gated on `VERTEX_PROJECT`;
  `VERTEX_GEMINI_LOCATION` defaults to `global`) cover chat, tool use, the
  shared toolloop scenarios, reasoning, cached input, `ProviderParams`,
  context overflow, and listing.
- **Amends ADR-0009** (Gemini chat joins the Vertex backend; the PSC rule
  now also documented for genai) and **ADR-0027** (the temporary `New`
  rejection of the Vertex options is lifted).

## References

- Issue #104; ADR-0009, ADR-0010, ADR-0012, ADR-0022, ADR-0023, ADR-0024,
  ADR-0026, ADR-0027
- `docs/MAESTRO_DIVERGENCES.md` G4
