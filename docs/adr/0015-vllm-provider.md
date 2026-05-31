# 0015. vLLM provider (leaf package, OpenAI Chat Completions wire shape)

- **Status:** Accepted
- **Date:** 2026-05-31

## Context

vLLM is the canonical self-hosted GPU inference server in the
OpenAI-compatible ecosystem. It serves arbitrary HuggingFace-format
models behind an `OpenAI-shaped` HTTP API: `/v1/chat/completions`,
`/v1/models`, optionally `/v1/embeddings` and (in recent versions)
`/v1/responses`. Adding a vLLM provider was the first non-port feature
queued after the v0.2 cut-over (per the
`first-greenfield-feature-vllm` memory). Triggered now by a concrete
dev instance at `100.102.40.8:8000` running `mistralai/Ministral-3-14B-
Instruct-2512` on vLLM v0.21.0.

Two structural choices to make:

1. **Where the code lives.** Riding on the existing `openai` package
   would mean either coupling vLLM into a Responses-API client (wrong
   surface — vLLM's mature surface is Chat Completions) or refactoring
   `openai` to expose both Responses and Chat Completions clients for
   no consumer benefit on the OpenAI side. The existing `openai`
   adapter is deliberately Responses-only (see OC1/OC3 in
   `MAESTRO_DIVERGENCES.md`).

2. **Which OpenAI surface.** vLLM does expose `/v1/responses` in
   recent releases, but it is newer, less battle-tested, and not what
   the Mistral tool-call parser is wired against. Chat Completions is
   the universally-deployed surface every OpenAI-compatible inference
   server (vLLM, llama.cpp, TGI, LM Studio, LocalAI) implements first
   and best.

## Decision

Ship vLLM as a separate leaf package at `llms/providers/vllm`,
using the **openai-go SDK's Chat Completions** path
(`client.Chat.Completions.New`) with a configurable base URL. Mirrors
the precedent of `anthropicvertex` as a leaf alongside `anthropic`.

- **No new dependency**: `openai-go` is already imported. The Chat
  Completions surface (`openai.ChatCompletionNewParams`,
  `openai.ChatCompletion`) is part of the same SDK that backs the
  existing OpenAI Responses adapter; we just exercise a different
  endpoint path.
- **Configuration**: `vllm.New(WithBaseURL, WithModel, WithAPIKey)`.
  `WithAPIKey` is optional — vLLM's default install has no auth, and
  even when an operator sets `VLLM_API_KEY` it is a plain bearer
  token, so the same `option.WithAPIKey` plumbing works. An empty key
  is allowed at construction (in contrast to the OpenAI / Anthropic
  adapters which require one), since "no auth" is a normal vLLM
  configuration.
- **Tool calling** is wired through Chat Completions' standard
  `tools` + `tool_choice` fields. Whether tools work at runtime is
  model-dependent: vLLM relies on a per-model `--tool-call-parser`
  on the server side. The toolkit forwards the request and trusts
  the server to either emit tool calls or not; we do not gate on the
  model.
- **`ModelLister`** is implemented (`/v1/models` is universally
  available); **`LatestInFamily` is NOT** — HuggingFace-style names
  (`mistralai/Ministral-3-14B-Instruct-2512`,
  `Qwen/Qwen2.5-72B-Instruct`) have no canonical family convention,
  and the operator picks what to serve. Matches the Ollama precedent.
- **`ModelInfo.Created`** carries vLLM's `created` field, which is
  the **model load time on this vLLM instance**, not the upstream
  HuggingFace release date. Documented at the field level so callers
  don't read it as freshness. Same shape as Ollama's `modified_at`
  caveat.
- **Streaming** is deferred per ADR-0003. vLLM supports SSE streaming
  on `/v1/chat/completions`, but until a streaming-aware middleware
  semantics ADR lands we ship `Complete` only.
- **Live integration test** is gated behind a `MAESTRO_VLLM` env var
  (full base URL). Unset → `t.Skip` with a clear message. Mirrors
  the existing local-only-test pattern used for Ollama on macOS
  (`MAESTRO_ANTHROPIC_API_KEY` fallback, `OLLAMA_HOST`,
  `OPENAI_API_KEY`). CI runs hermetic httptest unit tests only;
  developers run live tests against any reachable vLLM instance.
  Optional `MAESTRO_VLLM_MODEL` overrides the model ID, defaulting
  to the first one `/v1/models` reports.

**Why no opt-in for `/v1/responses` even though vLLM has it.** The
Responses surface is the wrong shape for the current vLLM ecosystem:
tool-call parsers (Mistral, Hermes, Llama, etc.) are wired against
Chat Completions; almost no vLLM user hits Responses. Adding it would
mean two code paths to maintain for a feature few will use, and the
existing OpenAI Responses adapter is already there for callers
genuinely on OpenAI. If a consumer asks for vLLM-via-Responses
specifically, that gets a future ADR.

## Consequences

- Maestro / Morris / maestro-cms can use vLLM through a stable
  `llms.ChatClient` interface alongside Anthropic, OpenAI, Google,
  and Ollama. Switching between hosted and self-hosted is a
  `client = vllm.New(...)` vs `client = openai.NewChat(...)` swap;
  no other plumbing changes.
- The package is greenfield (no Maestro reference implementation), so
  `MAESTRO_DIVERGENCES.md` rows for vLLM (`V1`, `V2`, ...) are
  informational rather than acceptance-gating — they document
  vLLM-specific behaviors a cut-over consumer should know about
  (`Created` semantics, no-auth default, model-dependent tool
  calling) rather than divergences from a prior implementation.
- Choosing Chat Completions over Responses for vLLM means we maintain
  two OpenAI-shaped code paths internally (Responses for OpenAI
  proper, Chat Completions for vLLM). Reuse-by-shared-helpers would
  obscure provider intent; keeping them separate matches the
  "provider packages are leaf imports" stance from the spec.
- vLLM has no upstream tokenization endpoint exposed by the standard
  HTTP API (unlike Anthropic's `count_tokens` or Gemini's
  `CountTokens`), so it stays char-based for token estimation under
  ADR-0013. If a tokenizer-backed `TextEstimator` variant is added
  later for vLLM specifically, that lands with the broader text
  estimator ADR.
- The empty-API-key allowance is a real loosening of the toolkit's
  general "missing key → config error at construction" stance. It is
  scoped to vLLM because vLLM has a legitimate no-auth deployment
  mode; other providers (Anthropic, OpenAI hosted, Google) keep the
  strict requirement.

## References

- `llms/providers/vllm/` — implementation.
- `llms/providers/openai/` — Responses adapter, deliberately separate.
- ADR-0002 (Ollama no-SDK pattern, the closest precedent for a
  non-hosted backend), ADR-0003 (streaming deferral), ADR-0007
  (`ToolChoiceRequired` semantics that flow through vLLM unchanged),
  ADR-0012 (`ModelLister` per-provider implementation pattern; vLLM
  mirrors Ollama's list-only shape).
- `docs/MAESTRO_DIVERGENCES.md` — informational V-prefixed rows.
- vLLM upstream: https://docs.vllm.ai/en/latest/serving/openai_compatible_server.html
