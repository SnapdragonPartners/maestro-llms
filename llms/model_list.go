package llms

import (
	"context"
	"time"
)

// ModelLister is an optional capability a ChatClient may also implement
// to expose the provider's catalog of available models. Callers and
// middleware discover it with a type assertion — adding it does NOT widen
// ChatClient (same pattern as StreamingChatClient). Providers without a
// list API (or where listing has no consistent meaning, like a future
// vLLM operator-served endpoint) simply do not implement it; the failed
// type assertion is the right signal.
//
// See ADR-0012 for the design and binding non-goals. "Latest in family"
// helpers live in each provider's package because family naming
// (claude-opus/sonnet/haiku, gpt-5/gpt-4o, gemini-pro/flash) is
// per-provider convention.
type ModelLister interface {
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// ModelInfo is the provider-neutral entry returned by ListModels. The
// shape is intentionally minimal — Raw carries the SDK-specific payload
// for callers that need fields outside the stability contract.
type ModelInfo struct {
	// ID is the provider's model identifier as it would be passed back
	// to the chat client (e.g. "claude-opus-4-7-20251015",
	// "gpt-5-2026-03-15", "gemini-3-pro-preview", "llama3.2:1b").
	ID string
	// Family is the provider-classified family name used by that
	// provider's LatestInFamily helper. Empty when family parsing is
	// not applicable (Ollama, vLLM) or the ID does not match a known
	// pattern. Callers should not interpret Family across providers —
	// it is a per-provider convention.
	Family string
	// Created is when the provider released this model, where the SDK
	// exposes it. Zero where unavailable (some Gemini list entries
	// have no date). For Ollama, Created is the LOCAL pull time
	// (modified_at on the local filesystem), not the provider release
	// time — read with that caveat.
	Created time.Time
	// Raw is the underlying SDK payload (anthropic.ModelInfo, openai.Model,
	// *genai.Model, etc.). Outside the stability contract; callers that
	// use it accept provider-package breakage risk.
	Raw any
}
