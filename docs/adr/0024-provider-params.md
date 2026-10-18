# 0024. Per-request `ProviderParams`: raw provider body fields, deep-merged

- **Status:** Accepted
- **Date:** 2026-10-17

## Context

The provider-neutral mappings (ADR-0022 reasoning, `MaxTokens`, `Temperature`
and so on) are deliberately per provider, not per model, and cover a shared
subset. Apps sometimes know more about a model than the toolkit does:
- `chat_template_kwargs` on OpenAI-compatible servers;
- Anthropic `thinking.display`;
- Gemini fields that have no neutral mapping yet;
- a mapped value that one particular model needs set differently.

The only escape hatch was `anthropic.WithRequestOptions` (ADR-0009), and it is
per client and Anthropic-only (#93).

## Decision

(Dan, recorded on #93.)

1. **Shape:** `ChatRequest.ProviderParams map[llms.ProviderID]map[string]any`,
   keyed by the typed `ProviderID` (ADR-0021). Each adapter applies only its
   own entry, so one request can carry fields for several providers and
   still be routed to any of them.
2. **The same deep merge on every provider, with no path syntax.** The entry
   is merged into the wire body after the adapter has mapped everything
   else. When both sides are objects they merge recursively; any other
   value (scalar, array or `null`) replaces the mapped one. There are no
   deletion semantics. A top-level replace was rejected: nearly all of
   Gemini's knobs live under `generationConfig`, so overriding one field
   would wipe the adapter's temperature, max tokens and thinking config.
3. **One implementation**, in `llms/providers/internal/extraparams`. The
   entry is first normalized through a JSON round trip (`UseNumber`), so
   struct values merge as objects and large integers keep their precision.
   - openai, vllm and anthropic (including `anthropicvertex`): a
     per-request SDK middleware deep-merges into the serialized JSON body.
     The body is re-encoded only when there is an entry for that provider.
   - google: genai's per-request `HTTPOptions.ExtrasRequestProvider` runs
     the same merge. genai's own `ExtraBody` merge was not used because it
     logs a warning on type mismatches.
   - ollama: the same merge, applied to the wire JSON.
4. **Values must marshal to JSON.** A value that can't (a channel, a func)
   is a local `bad_request`, raised before any network call. That is a
   genuinely invalid request. ADR-0022's no-local-failure rule is about
   judgement calls, and this isn't one.
5. **`AppliedReasoning` stays truthful.** When an entry overrides a field the
   adapter mapped for reasoning, that field drops out of `AppliedReasoning`.
   "Overrides" means the entry sets the exact leaf, or replaces one of its
   ancestors with a non-object (`extraparams.Overrides`). The reasoning
   paths per adapter:
   - anthropic: `thinking.type`, `thinking.budget_tokens`, `output_config.effort`
   - openai: `reasoning.effort`
   - vllm: `reasoning_effort`
   - google: `generationConfig.thinkingConfig.thinkingLevel` and `.thinkingBudget`
   - ollama: `think`
6. **Chat only.** `EmbeddingRequest` is unchanged, so the existing embeddings
   consumer can't be affected. Adding the field there later would be
   additive.

## Consequences

- Apps can send any provider field per request without new toolkit
  surface. Verified live on 2026-10-17: a 16-token output cap set through
  each provider's own field beat `MaxTokens` 1024 on Anthropic (first-party
  and Vertex), OpenAI, Gemini (nested `generationConfig`), Ollama
  (`options.num_predict`) and LunaRoute. On LunaRoute DeepSeek a
  `reasoning_effort: none` override beat the mapped `High`: 0 reasoning
  tokens, and `AppliedReasoning` came back nil.
- **Outside the stability contract.** The wire shapes belong to the
  providers, and an override can break a request in ways the adapter can't
  check (for example, overriding `messages`).
- **Middleware doesn't see these fields.** The estimator, `ValidationChat`
  and the limiter work from the neutral request, so an override that raises
  output (a larger `max_tokens`) isn't reserved for. Apps that use it with a
  limiter should set `MaxTokens` to match.
- `anthropic.WithRequestOptions` remains as the per-client escape hatch.

## References

- Issue #93 (design decisions in its comments). Related: #86/ADR-0022,
  #92/ADR-0021.
- `llms/chat.go` (`ChatRequest.ProviderParams`),
  `llms/providers/internal/extraparams`, and each adapter's `Complete`.
- Live: `livetest.ProviderParamsOverride`, plus
  `TestIntegrationLunaRouteProviderParams/reasoning_effort`.
- `docs/MAESTRO_DIVERGENCES.md` X10.
