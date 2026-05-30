# 0012. Model listing & per-family upgrade detection

- **Status:** Accepted
- **Date:** 2026-05-30

## Context

Long-running consumers (Maestro, multi-month projects) pin a specific
model ID at config time — e.g. `claude-opus-4-5-20251201` — and have no
in-band way to learn that a newer model exists in the same family. The
concrete report: a Maestro user noticed they were still on Opus 4.5
months after 4.7 had shipped, by accident, looking at logs.

The need is **upgrade visibility**, not auto-update: surface "Opus 4.7 is
available, upgrade?" to the user; let the user decide. That distinction
matters — auto-update is consumer policy (which provider tiers count,
how to handle preview models, when to refresh) and does not belong in
the toolkit.

Each SDK already exposes a model-list API
(`anthropic.Models.List`, `openai.Models.List`, `genai.Models.List`,
Ollama `/api/tags`), but the wire shapes diverge: Anthropic returns
`time.Time` `CreatedAt`; OpenAI returns Unix seconds `Created`; Gemini
has **no created date** at all (only an auto-incrementing `Version` and
the version number inside the ID itself); Ollama returns `modified_at`
which is the *local* pull time, not the provider's release time. Family
naming also diverges (Anthropic's `opus`/`sonnet`/`haiku`; OpenAI's
`gpt-5`/`gpt-4o`/`o3-mini`; Gemini's `gemini-N-pro`/`flash`).

The risk of a "give me the latest" API is that it makes the wrong path
the easy path for production code (which should pin IDs deliberately
for reproducibility), and that "latest" without family context is
useless for the actual use case (latest *sonnet* helps no one running
on opus).

## Decision

Add a small optional capability + per-provider helpers, hewing to the
existing pattern (capability growth via optional interfaces +
type assertion, never widening core interfaces — see CLAUDE.md and the
`StreamingChatClient` precedent).

### Core (`llms` package)

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

`ModelLister` is **optional**: consumers discover it with a type
assertion on a `ChatClient`. Adding it is not a breaking widening of
`ChatClient`. Providers without a list API simply don't implement it.

### Per-provider helpers (Anthropic / OpenAI / Google)

Two-tier shape so callers can either cache the list and query offline
or do the upgrade check in one call:

```go
// Pure helper (no I/O). Returns (newer, true) only if a strictly-newer
// model exists in the same family as currentID, else (zero, false).
func LatestInFamily(currentID string, models []llms.ModelInfo) (llms.ModelInfo, bool)

// One-shot convenience on the provider's chat client: chains
// ListModels + LatestInFamily.
func (c *Chat) LatestInFamily(ctx context.Context, currentID string) (llms.ModelInfo, bool, error)
```

### Ollama

`ListModels` only — no `LatestInFamily`. Ollama lists *locally pulled*
models, not the provider catalog; "latest" has no consistent meaning.
The capability gap is honest: a type assertion to a future `FamilyResolver`
would fail. `Created` is populated from `modified_at` and documented as
local pull time, not provider release time.

### Family parsing (per provider, permissive by default)

The provider package is the right home for family-naming knowledge —
it already encodes wire shapes, error classification, and IDs.

- **Anthropic.** Family = `claude-{opus|sonnet|haiku}` matched anywhere
  in the ID. Crosses generations: `claude-3-5-sonnet-20240620` and
  `claude-sonnet-4-5-20251201` both have family `claude-sonnet`. "Latest"
  ordered by `CreatedAt` desc.
- **OpenAI.** Family = the ID stripped of any trailing `-YYYY-MM-DD`
  date suffix. So `gpt-5-2026-03-15` → `gpt-5`; `gpt-5-mini-2025-12-01`
  → `gpt-5-mini`; `o1-preview-2024-09-12` → `o1-preview`. "Latest"
  ordered by `Created` Unix-seconds desc. **No toolkit-level filter for
  chat vs embeddings vs image**: the OpenAI list returns everything but
  `LatestInFamily` is self-filtering by family-prefix match, so an
  embeddings model never collides with a `gpt-*` family.
- **Google.** Resource-path prefix (`models/`) stripped. Family =
  `gemini-{pro|flash|nano|ultra}` matched in the ID — so
  `gemini-1.5-pro-001` and `gemini-3-pro-preview` both have family
  `gemini-pro`. "Latest" ordered by **numeric version parsing** from
  the ID (`gemini-3-…` > `gemini-2.5-…` > `gemini-1.5-…`), because the
  Gemini list does not expose a created date. Ties broken by lexical
  ID order.

This parsing is intentionally **permissive**: major-version bumps stay
in the same family. Callers that want stricter pinning (e.g. "stay
within the same major version") can filter the `ListModels` result
themselves; the helper is offered as a convenience over the data,
not as the only policy.

## Consequences

- Maestro and similar consumers get the data + the one-shot upgrade
  check (`if newer, ok, _ := client.LatestInFamily(ctx, currentID); ok
  { promptUser(newer.ID) }`) without the toolkit taking a position on
  auto-update.
- Provider-specific family conventions are isolated to provider
  packages, where wire shapes and error classifications already live.
  Core stays neutral.
- The split between `ModelLister` (data, all providers) and
  `LatestInFamily` (provider-side classifier) keeps Ollama / future
  vLLM honest: they implement only what they actually have. Type
  assertion is the discovery mechanism, matching `StreamingChatClient`.
- Permissive family parsing crosses generations on purpose: it serves
  the reported use case ("I missed 4.7 because I was on 4.5") without
  embedding extra editorial choices. The cost is callers wanting
  stricter behavior have to filter the list themselves; this is a
  documented trade-off, not a defect.
- "Newest by date" works for Anthropic and OpenAI; for Google we order
  by parsed version numbers in the ID, since the genai list does not
  expose a created date. That parsing is a small piece of
  provider-specific code in `llms/providers/google`; if Gemini's
  naming ever drifts, only that helper changes.
- `ModelInfo.Created` from Ollama is **local pull time**, not provider
  release time. Documented at the field level so callers don't read it
  as freshness from the registry.
- No `MAESTRO_DIVERGENCES.md` row: additive new surface, no Maestro
  behavior to diverge from.
- **Non-goals (binding):** not a cross-provider abstraction over model
  identity; not auto-update; not toolkit-side caching/TTL; not a stable-
  vs-preview filter; not a recommendation engine. Apps build those
  on top.

## References

- `llms/model_list.go` — `ModelLister`, `ModelInfo`.
- `llms/providers/{anthropic,openai,google}/models.go` — per-provider
  `ListModels`, `LatestInFamily`, family parsers.
- `llms/providers/ollama/models.go` — `ListModels` only.
- ADR-0001 (process), ADR-0003 (optional-capability pattern precedent
  — `StreamingChatClient`), ADR-0011 (toolloop helper — same neutral-
  shipping-data + per-provider-policy split).
- `docs/specification.md` — `ModelLister` capability added.
