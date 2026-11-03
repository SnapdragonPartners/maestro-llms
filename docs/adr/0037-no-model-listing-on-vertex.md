# 0037. No model listing on Vertex AI: `ListModels` fails fast with a config error

- **Status:** Accepted
- **Date:** 2026-10-31

## Context

ADR-0012 added the optional `llms.ModelLister` capability. On Vertex AI it
did two different things, and neither was useful (#130):

- **`anthropicvertex`** returns the base `*anthropic.Client`, so it has the
  base package's `ListModels`. Vertex doesn't serve the Anthropic
  `/v1/models` endpoint, so every call failed with a network 404 classified
  as `bad_request`. A successful type assertion on `ModelLister` normally
  means listing works, and here it did not.
- **`google` on Vertex** (ADR-0028) lists through genai's `Models.All`,
  which on that backend queries `publishers/google/models`, the Model
  Garden catalog for Google's models. It works, after stripping the
  publisher prefix.

Before v1 freezes the behavior, we checked what an app could actually learn
from Vertex, against a test project on the `global` endpoint:

- **The Model Garden catalog** (`GET v1beta1/publishers/anthropic/models`;
  `v1` has no list method) lists what Anthropic publishes on Vertex, not
  what the project can call. Most of the Claude models it listed returned
  404 (not enabled) or 403 (data sharing required) on a one-token call.
  The request path names no project, so the catalog cannot reflect a
  project's enablement.
- **Vertex quota metrics** (Service Usage `consumerQuotaMetrics`) show
  capacity, not access. Several models with their own positive quota still
  returned 404. Newer Claude models share one quota per family (for
  example `anthropic-claude-opus`) across several versions, most of them
  not callable. The quota names are not a documented API, and reading them
  needs `serviceusage.quotas.get`, a separate API and permission from the
  one chat uses.

The only reliable test of "can this project call this model" is calling it.
Even knowing a model is enabled, without its quota per region, doesn't tell
an app where to send traffic. So no signal Vertex offers gives an app
something to act on, and a list it can't act on is worse than no list: an
app that fills a model picker from it, or follows `LatestInFamily` to a
newer ID, ends up with models that fail at call time.

## Decision

**On Vertex AI, `ListModels` fails fast with a `*llms.ProviderError` of kind
`config`, without sending a request, for both providers.** The error names
the provider and the client's model, and its message is `model listing is
not supported on Vertex AI`. Each provider's one-shot `LatestInFamily`
method returns the same error, because it lists first.

- **`google`:** `ListModels` checks the genai client's backend
  (`ClientConfig().Backend == genai.BackendVertexAI`). No API change.
- **`anthropic` / `anthropicvertex`:** no public API change either.
  `anthropicvertex.New` passes an internal marker
  (`llms/providers/anthropic/internal/backend.NoModelListing`) through
  `anthropic.WithRequestOptions`, the channel that already carries the
  Vertex auth and endpoint options. `anthropic.New` removes the marker
  before it counts or applies the request options, so the marker never
  stands in for the caller's auth, and records the backend name for
  `ListModels`. The marker is a `RequestOption` whose `Apply` does nothing,
  the same pattern the SDK uses for `option.WithoutEnvironmentDefaults`.
- **Everywhere else `ListModels` is unchanged**: the Anthropic API, OpenAI
  and the Gemini API list the models those credentials can use; Ollama
  lists locally pulled models; `openaicompat` lists what the server serves.

Both Vertex clients still satisfy `llms.ModelLister`, so on Vertex a
successful type assertion does not mean listing works. That is the one
documented exception to ADR-0012's discovery rule. An app that needs the
models a Vertex project can call keeps that list in its own configuration,
or probes candidates with its own requests, with its own cost and caching
policy.

The `google` pure helpers (`familyOf`, `LatestInFamily(currentID, models)`)
still accept `publishers/google/models/<id>` names, so an app that builds a
list itself can still use them.

## Alternatives considered

- **Return the Model Garden catalog** (the original option 1 in #130):
  rejected. "Exists on Vertex" gives an app nothing to act on, and it makes
  `LatestInFamily` suggest models the project can't call.
- **Infer access from quotas, then probe** (a quota-first discovery
  pipeline): rejected. The quotas didn't narrow the candidates (positive
  quota on models the project couldn't call), so it adds a second API, a
  permission and an undocumented name mapping, and still ends in probing.
- **Probe each catalog model with a minimal request:** rejected for this
  package. It costs a request per model on every listing, counts against
  quota, and confuses a temporary failure with "not enabled". That policy
  belongs to the app.
- **Return a distinct client type from `anthropicvertex.New` that has no
  `ListModels`:** this would make the type assertion honest, but it changes
  the return type, and `google` would need the same split by backend
  inside one constructor, which a type can't express. Failing fast keeps
  both constructors' types and gives one rule for Vertex.
- **A public `anthropic.WithoutModelListing()` option:** rejected. The rule
  is about Vertex, not something an app chooses, so it stays internal and
  adds nothing to the v1 surface. If a real consumer needs it for another
  backend, such as a gateway without `/v1/models`, it can be added later
  without a break.

## Consequences

- Behavioral change, no API change. On Vertex, `anthropicvertex`'s
  `ListModels` goes from a network 404 (`bad_request`) to an immediate
  `config` error, and `google`'s goes from listing the publisher catalog
  to the same immediate error. No known consumer calls `ListModels` or
  `LatestInFamily` (checked: Maestro, Morris, maestro-cms, buggrit).
- Amends ADR-0012 (the discovery rule now has a documented Vertex
  exception) and ADR-0028 (the decision to list on Vertex and strip the
  publisher prefix in `ListModels`).
- Divergence X14.

## References

- #130; #84 (v1 readiness).
- `llms/providers/google/models.go` — `ListModels`.
- `llms/providers/anthropic/models.go` — `ListModels`;
  `llms/providers/anthropic/anthropic.go` — `New` removes the marker.
- `llms/providers/anthropic/internal/backend` — the marker.
- `llms/providers/anthropic/anthropicvertex/anthropicvertex.go` — `New`
  passes it.
