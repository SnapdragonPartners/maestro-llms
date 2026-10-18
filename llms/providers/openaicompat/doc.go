// Package openaicompat implements the maestro-llms chat client for any server
// or gateway that speaks the OpenAI Chat Completions wire shape
// (/v1/chat/completions): self-hosted vLLM (https://docs.vllm.ai), hosted
// routers such as LunaRoute, and similar OpenAI-compatible endpoints. It uses
// the openai-go SDK with a configurable base URL, not the OpenAI-proprietary
// Responses API used by the sibling `openai` package. See ADR-0015 for the
// design and ADR-0027 for the rename from `vllm` (that package remains as a
// deprecated alias).
//
// Compatible servers have no canonical "family" concept for model identifiers
// — a vLLM operator runs whatever HuggingFace model they choose
// (mistralai/Ministral-3-14B, Qwen/Qwen2.5-72B, custom fine-tunes) — so
// ModelLister is implemented but LatestInFamily is intentionally not. Matches
// the Ollama precedent. On vLLM, ModelInfo.Created carries the model load time
// on the instance, not the upstream release date; same caveat as Ollama's
// modified_at.
//
// Tool calling is forwarded through the standard `tools` / `tool_choice`
// fields. Whether the model actually emits tool calls is the server's call
// (on vLLM, its per-model `--tool-call-parser` configuration: Mistral,
// Hermes, Llama, Pythonic and others). The toolkit does not gate on this; we
// forward the request and let the server decide.
//
// Verified live against vLLM and the LunaRoute router with GLM and DeepSeek
// models (issue #54). Reasoning models that return reasoning in a separate
// reasoning_content field are handled: Text is the answer only, and reasoning
// is counted in Usage.ReasoningTokens (ADR-0016). The reasoning text itself
// is not surfaced (it remains in Raw).
//
// Streaming is deferred per ADR-0003. Chat Completions supports SSE, but until
// streaming-aware middleware semantics land we ship Complete only.
package openaicompat
