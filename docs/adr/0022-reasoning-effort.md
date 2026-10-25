# 0022. Provider-neutral reasoning control (`ChatRequest.Reasoning`, `ChatResponse.AppliedReasoning`)

- **Status:** Accepted; amended by [ADR-0032](0032-enum-like-types-v1.md) (new `ReasoningEffort` constants may be added after v1; only the constants have meaning)
- **Date:** 2026-10-17

## Context

Reasoning models can spend minutes and tens of thousands of tokens on a single
turn. buggrit measured GLM-5.3 (via LunaRoute) at 5–9 minutes per agentic turn
(#86). Before this change, the only bound was `MaxTokens`, which truncates the
reasoning into an empty answer (ADR-0020) instead of asking the model to think
less. Every provider has a control, but they differ in name, values and model
coverage:

| Provider | Control | Values |
|---|---|---|
| OpenAI Responses | `reasoning.effort` | none, minimal, low, medium, high, xhigh, max |
| OpenAI-compatible (`vllm`) | `reasoning_effort` | same enum; honoring varies by server/model |
| Anthropic | `output_config.effort` | low, medium, high, xhigh, max |
| | `thinking` | disabled; enabled + `budget_tokens` (before Opus 4.7) |
| Google | `thinkingConfig.thinkingLevel` | minimal, low, medium, high (Gemini 3) |
| | `thinkingConfig.thinkingBudget` | tokens (Gemini 2.5) |
| Ollama | `think` | false, or low/medium/high (level-taking models such as gpt-oss) |

Support also varies **by model** within a provider. Anthropic models from Opus
5.5 on reject `thinking: disabled`, and from Opus 4.7 on reject
`budget_tokens`. Opus 4.5 has no `xhigh`/`max`. OpenAI non-reasoning models
reject `reasoning.effort`. Which Gemini field applies depends on the
generation. A probe on #86 showed LunaRoute passes `reasoning_effort` through
(SGLang validates it). GLM treats it as a soft bound (2–4× fewer reasoning
tokens, and `none` doesn't turn reasoning off), while DeepSeek turns reasoning
off at `none`.

## Decision

1. **Request.**

   ```go
   type ReasoningEffort string // constants: None, Minimal, Low, Medium, High, XHigh, Max

   type ReasoningConfig struct {
       Effort       ReasoningEffort // zero: not set
       BudgetTokens int             // <= 0: not set
   }

   ChatRequest.Reasoning *ReasoningConfig
   ```

   A closed set of constants. Go can't close a string type, so a value outside
   the set is treated as unsupported.

2. **Mapping is 1:1 or omitted, with no failure.** Each adapter has a fixed
   table. A value maps to exactly one wire value, or it has no entry and the
   field is left out, so the **model default** applies. There is no clamping
   to a "nearest" level and no local error. A local failure can't be retried
   and the compiler won't catch it, so degraded is better than failed (Dan).

3. **No per-model prediction (option a).** Mappings are per provider. When a
   specific model rejects a value the provider supports, the provider's 400
   comes back unchanged. That's a genuinely invalid request, not something for
   the toolkit to guess around. Rejected alternatives:
   - Hand-written per-model tables: they go stale with every launch, and model
     knowledge is app knowledge.
   - Retrying without the field on a 400: that needs message matching.
   - Runtime capability lookup: only some providers expose it. It may come
     later where they do.

4. **Report what was sent.** `ChatResponse.AppliedReasoning *ReasoningConfig`
   holds the fields the adapter mapped, in neutral terms, or nil when nothing
   was sent. It's the request-side counterpart of `Usage`, and it's how a
   fallback to the model default is reported back, since the package has no
   logger.

5. **Per-provider tables.**

   | Neutral | Anthropic | OpenAI / vllm | Google | Ollama |
   |---|---|---|---|---|
   | `none` | `thinking: disabled` | `none` | omitted | `think: false` |
   | `minimal` | omitted | `minimal` | `MINIMAL` | omitted |
   | `low` / `medium` / `high` | `output_config.effort` | same | `LOW` / `MEDIUM` / `HIGH` | `think: "low"` etc. |
   | `xhigh` / `max` | `output_config.effort` | same | omitted | omitted |
   | `BudgetTokens` | `thinking: enabled, budget_tokens` | omitted | `thinkingBudget` | omitted |

   - **Anthropic:** `BudgetTokens` and `none` both use the `thinking` field.
     When both are set, the budget wins, and `none` is left out of
     `AppliedReasoning`.
   - **Google:** the adapter doesn't guess the generation. `Effort` maps to
     `thinkingLevel` and `BudgetTokens` to `thinkingBudget`, so the caller picks
     the field that fits the model. Both are sent when both are set.

## Consequences

- Apps can bound reasoning on every provider through one field and see exactly
  what was sent. Measured live on 2026-10-17 with the same counting prompt
  (reasoning tokens):
  - OpenAI gpt-5-mini: minimal 0, high 1792.
  - Gemini 2.5 Flash: budget 128 gave 82; budget 4096 gave 3180.
  - Gemini 3 Flash: low 1372, high 15358.
  - LunaRoute GLM-5.3: low 615, high 731 (soft).
  - LunaRoute DeepSeek: none 0.
  - Anthropic Haiku 4.5: budget 1024 turns thinking on (1548).
  - Opus 5.5: `none` is rejected by the model with a 400, which is decision 3
    working as intended.
- An unsupported value silently runs at the model default. Callers who care
  compare `AppliedReasoning` with what they asked for.
- No escape hatch for fields the tables don't cover (`chat_template_kwargs`,
  Anthropic's `display`). That is #93 (`ProviderParams`).
- **Not done here:** an Anthropic `thinking.block_binding.prefix_mismatch_behavior`
  default, which ADR-0021 considered for this change. The adapter still only
  sends a `thinking` value when the caller asks for one (`none` or a budget).
  Setting `drop_block` by default would mean sending a thinking type and a beta
  header on every request, and the reasons ADR-0021 gave still hold.

## References

- Issue #86, including the LunaRoute probe and the design decisions in its
  comments. Related: #92/ADR-0021, #93.
- `llms/chat.go` (`ReasoningConfig`, `ReasoningEffort`, `AppliedReasoning`).
  `mapReasoning` / `mapEffort` in each provider package.
- Live: `livetest.ReasoningControl`, plus `TestIntegration*Reasoning{Effort,Budget}`
  in each provider and `TestIntegrationAnthropicReasoningNoneRejectedByModel`.
- `docs/MAESTRO_DIVERGENCES.md` X8.
