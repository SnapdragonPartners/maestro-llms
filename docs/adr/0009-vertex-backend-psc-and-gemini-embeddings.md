# 0009. Vertex AI backend (Anthropic + Gemini embeddings), PSC endpoint injection, task-typed embeddings

- **Status:** Accepted
- **Date:** 2026-05-17

> Design decision of record. The implementation is a v0.4-class multi-PR
> series and is **not** included in the docs change that introduced this ADR.

## Context

Morris's default managed-AI posture is Claude **and** `gemini-embedding-001`
reached via **Google Vertex AI** over **Private Service Connect (PSC)**, with
no static provider API keys (runtime service account + explicit IAM, infra
managed by OpenTofu). The package must make this expressible without
importing app-specific cloud/credential assumptions, and without regressing
the leaf-import guarantee. Reviewed with Team Morris; their corrections are
folded in below.

## Decision

### Anthropic-on-Vertex — same semantics, separate package

`anthropic-sdk-go` exposes a `/vertex` subpackage. **Correcting an earlier
analysis error:** an `anthropic.WithVertex(...)` option on the base provider
would *not* preserve leaf imports — importing `.../vertex` into
`llms/providers/anthropic` makes every Anthropic consumer pull Google-auth
deps. Instead:

- Base `llms/providers/anthropic` gains a low-level
  `WithRequestOptions(...option.RequestOption)` escape hatch and imports **no**
  Vertex code.
- A new leaf package `llms/providers/anthropic/anthropicvertex` imports
  `.../vertex`, builds the Vertex request options, and constructs the base
  client via `WithRequestOptions`. Only its importers pay the Google-auth
  dependency; all of `convert.go` (request/response/tool/cache mapping) is
  reused unchanged because it is the same underlying `anthropic.Client`.

### Credentials & endpoint are app-supplied

No toolkit-level ADC discovery. The Vertex paths accept an app-supplied
authenticated HTTP client / credentials / request options and an explicit
endpoint override. Credential acquisition (service account, Workload
Identity) stays in the application.

**Sharp edge (must be designed explicitly, with option-order tests):**
`vertex.WithCredentials` builds its *own* authenticated HTTP client, which
collides with a PSC custom transport/endpoint. The implementation must define
and document a precedence rule (how the Vertex auth client and the PSC
transport/endpoint compose) and cover the option ordering with httptest.

### PSC = endpoint + transport injection only

The package provides base-URL/endpoint override and `*http.Client`/transport
injection on the Vertex paths. PSC itself — private DNS, restricted VIP,
VPC-SC perimeter, firewall, IAM, service-account roles — is Morris/OpenTofu
and explicitly **out of scope** for `maestro-llms`.

### Gemini embeddings (new capability)

A new `EmbeddingClient` in `llms/providers/google` (`NewEmbeddings`), Vertex
backend via `genai` (`BackendVertexAI`, injectable `Endpoint`/`HTTPClient`).
Morris's supported path is **Vertex-only**; the direct Gemini-API embedding
backend is included only if it falls out of `genai` cheaply.

- **Task-typed embeddings (core API addition).** Add provider-neutral
  `EmbeddingRequest.Task` (an `EmbeddingTask` enum) and optional
  `EmbeddingInput.Title`. Task type (retrieval-document vs retrieval-query,
  …) materially changes vectors for RAG; it is per-embedding intent and must
  not hide in `Metadata`. Same pattern as `CacheBreakpoint`: neutral,
  honored where supported (Gemini), ignored where not (OpenAI). `Title` is
  only meaningful with a retrieval-document task.
- **`gemini-embedding-001` is single-input.** The client returns a typed
  `bad_request`/validation error when `len(Inputs) > 1`. **No internal
  fan-out, no chunking-contract exception.** This is just a strict batch
  limit (= 1) under the existing spec rule; the load-bearing "app owns
  chunking/batching/progress" decision is preserved. Morris ingestion sets
  batch size from model metadata/config.
- **Auto-truncation.** Vertex defaults `auto_truncate = true` (silent quality
  loss). The Google embeddings client exposes `WithAutoTruncate(bool)`
  defaulting to **false** — oversized input fails with a typed error rather
  than being silently truncated, for retrieval quality and auditability.
  Kept as a provider-client option, not a core field (truncation behavior is
  provider-specific; OpenAI already errors by default).

### Streaming stays deferred

Morris ADR 0008 expected streaming; for MVP/cut-over it is **explicitly out
of scope** (Morris is adjusting that expectation). ADR-0003 stands; this work
does not reopen it. See ADR-0003's revisited note.

## Consequences

- Leaf imports preserved; the new Google-auth deps land only on
  `anthropicvertex` importers (and `genai`, already a dep). `govulncheck`
  must pass on those deps before the implementation merges (the Ollama-SDK
  lesson).
- Morris gets keyless Claude+Gemini-embeddings over Vertex/PSC with the same
  provider-neutral retry/timeout/circuit/ratelimit/metrics stack.
- Net-new core surface: `EmbeddingTask`, `EmbeddingInput.Title`,
  `anthropic.WithRequestOptions`. Spec updated; additive, so no
  `MAESTRO_DIVERGENCES` row (not a behavior change vs Maestro's tested impl).
- Not solved here (Morris/OpenTofu): Vertex API enablement, `aiplatform`
  IAM, PSC/restricted-API DNS, VPC-SC perimeter, egress, model/region config.
- This is a v0.4-class series; sequence as its own ADR-tracked PR set
  (mirroring the middleware series), with the auth-vs-PSC precedence design +
  option-order tests called out as a first-class deliverable.

## References

- `llms/providers/anthropic` + planned `anthropic/anthropicvertex`
- `llms/providers/google` (planned `NewEmbeddings`)
- `docs/specification.md` — Embeddings API (Task/Title), provider/auth notes
- ADR-0003 (streaming deferred — revisited 2026-05-17), ADR-0001 (process)
- `github.com/anthropics/anthropic-sdk-go/vertex`; `google.golang.org/genai`
  (`BackendVertexAI`, `EmbedContent`)

## Implementation progress

Appended as PRs land (this ADR's prose above is unchanged — append-only).

- **PR-A** (`anthropic.WithRequestOptions`): merged.
- **PR-C** (`EmbeddingTask` / `EmbeddingInput.Title`): open.
- **PR-B** (`anthropicvertex`): realizes the Anthropic-on-Vertex path with
  option **(a)** confirmed — use the SDK's Vertex middleware, do not
  reimplement it. Implemented option order: `vertex.WithCredentials(...)`
  first, then the caller's `option.WithBaseURL(pscEndpoint)` and
  `option.WithHTTPClient(pscClient)` LAST. The Vertex path /
  `anthropic_version` middleware always applies; the network transport +
  endpoint are whatever is applied last, so a PSC client overrides Vertex's
  self-built one and therefore MUST carry Google auth itself. A non-nil
  `*google.Credentials` with a nil `TokenSource` is rejected up front (it
  would otherwise let Google's transport fall back to ambient ADC).
  `govulncheck` on the new Google-auth deps: no called vulnerabilities,
  leaf-isolated to `anthropicvertex` importers.
- **PR-D** (`google.NewEmbeddings`): Gemini/Vertex embedding client.
  Realizes Task→Gemini-task mapping, `Title`, per-request dimensions,
  order/ID preservation, and `gemini-embedding-001` single-input rejection
  (no fan-out). **Auto-truncate refinement:** genai's
  `EmbedContentConfig.AutoTruncate` is `bool,omitempty`, so
  `autoTruncate:false` is unrepresentable on the wire and Vertex would
  silently truncate by default. We therefore do NOT rely on the Vertex
  flag for the safe path: `maestro-llms` enforces no-silent-truncation
  with a client-side maximum input-byte guard (`MaxInputBytes`, literal
  UTF-8 bytes — not tokens). Fail-closed: `AutoTruncate=true` is
  Vertex-only (Gemini-API construction fails); `AutoTruncate=false`
  requires `MaxInputBytes>0` or `NewEmbeddings` fails (rather than look
  safe while Vertex truncates). govulncheck: no called vulnerabilities.
