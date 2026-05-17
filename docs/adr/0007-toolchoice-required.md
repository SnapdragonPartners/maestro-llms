# 0007. `ToolChoiceRequired` — force a tool call without naming one

- **Status:** Accepted
- **Date:** 2026-05-17

## Context

The `ToolChoice` model expressed `auto`, `none`, and `tool` (force a
*specific named* tool). It could not express **"the model must call one of
the offered tools, but may pick which"** — the standard "required"/"any"
mode every major provider supports.

This surfaced as a Maestro cut-over blocker (external review, P1): Maestro's
OpenAI client hard-codes `tool_choice = required` whenever tools are present,
Anthropic supports `any`, and coder behavior depends on tool-using turns.
With only `tool` (named) available, callers had to either leave it `auto`
(model may not call a tool) or force one specific tool (wrong when several
are valid) — neither matches the existing Maestro behavior, and the
divergence rows OC2/G2 promised a `required` the type could not represent.

Per the reviewer (and agreed): this is **not Maestro-specific**. "Must call
some offered tool" is a general, provider-neutral LLM capability.

## Decision

Add `ToolChoiceRequired` to the core `ToolChoiceType`. Semantics: the model
must call **at least one** of the offered tools; it chooses which.
`ToolChoiceTool` is unchanged (force a specific tool; `Name` required).

Provider mapping:

| Provider | Mapping |
|---|---|
| Anthropic | `tool_choice: {type: "any"}` |
| OpenAI (Responses) | `tool_choice: "required"` |
| Google (genai) | `FunctionCallingConfigMode = ANY`, no `AllowedFunctionNames` |
| Ollama | **best-effort**: Ollama has no `tool_choice`; tools are offered and the model decides (same wire result as `auto`). Not enforced. |

Spec updated (`ToolChoiceType` const + prose); divergences OC2/G2 resolved
(callers can now request the forced behavior explicitly); OL2 extended to
note `required` is best-effort like `tool`.

## Consequences

- Maestro cut-over unblocked for forced-tool behavior: callers set
  `ToolChoice{Type: ToolChoiceRequired}` instead of relying on a hard-coded
  per-client `required`.
- Anthropic/OpenAI/Gemini enforce it; **Ollama cannot** — a caller that
  needs a guaranteed tool call must not depend on Ollama for it. This is
  documented (spec, OL2, the `ToolChoiceRequired` doc comment) so the
  limitation is explicit, not silent.
- A `Required` (or `Tool`) choice with **no tools offered** is impossible;
  every adapter rejects it up front with a `bad_request` `*ProviderError`
  (via `ToolChoice.RequiresTools()`), rather than emitting an invalid call
  (Anthropic/OpenAI/Gemini) or silently degrading to a plain chat (Ollama).
- No behavior change for existing `auto`/`none`/`tool` callers.
- Per-provider wire-capture tests assert the serialized choice
  (`any` / `"required"` / `ANY` / tools-offered).

## References

- `llms/tool.go` — `ToolChoiceRequired`
- `llms/providers/{anthropic,openai,google,ollama}` — adapters + tests
- `docs/specification.md` — ToolChoice section
- `docs/MAESTRO_DIVERGENCES.md` — OC2, G2, OL2
- ADR-0001 (process)
