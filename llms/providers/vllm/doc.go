// Package vllm implements the maestro-llms chat client for self-hosted
// vLLM inference servers (https://docs.vllm.ai). vLLM serves arbitrary
// HuggingFace-format models behind the OpenAI-compatible HTTP API. This
// package speaks the standard OpenAI Chat Completions surface
// (/v1/chat/completions) via the openai-go SDK with a configurable base
// URL, not the OpenAI-proprietary Responses API used by the sibling
// `openai` package. See ADR-0015 for the rationale.
//
// vLLM has no canonical "family" concept for model identifiers — operators
// run whatever HuggingFace model they choose (mistralai/Ministral-3-14B,
// Qwen/Qwen2.5-72B, custom fine-tunes) — so ModelLister is implemented but
// LatestInFamily is intentionally not. Matches the Ollama precedent.
// ModelInfo.Created carries the model load time on the vLLM instance, not
// the upstream release date; same caveat as Ollama's modified_at.
//
// Tool calling is forwarded through the standard `tools` / `tool_choice`
// fields. Whether the model actually emits tool calls is determined by the
// server's per-model `--tool-call-parser` configuration (vLLM ships Mistral,
// Hermes, Llama, Pythonic and other parsers). The toolkit does not gate on
// this; we forward the request and let the server decide.
//
// Streaming is deferred per ADR-0003. vLLM supports SSE on Chat Completions
// but until streaming-aware middleware semantics land we ship Complete only.
package vllm
