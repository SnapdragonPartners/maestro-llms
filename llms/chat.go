package llms

import "context"

// ChatClient is the synchronous chat/completion interface. It is intentionally
// closed: streaming is a separate optional capability (see StreamingChatClient)
// so that adding streaming later is not a breaking change for every provider,
// fake, and middleware.
type ChatClient interface {
	// Complete generates a completion synchronously.
	Complete(ctx context.Context, req ChatRequest) (ChatResponse, error)
	// Model returns the model this client targets.
	Model() ModelRef
}

// StreamingChatClient is an optional capability a ChatClient may also implement.
// Callers and middleware discover it with a type assertion. Streaming is not
// implemented yet; StreamChunk is a forward declaration. Both stay outside the
// v1 stability contract until streaming is designed (ADR-0003).
type StreamingChatClient interface {
	Stream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error)
}

// StreamChunk is one incremental piece of a streamed response. Its shape is a
// forward-declared placeholder and may change when streaming is actually
// designed. Streaming itself is deliberately deferred (no consumer needs it
// yet) and middleware wraps Complete only — see docs/adr/0003.
type StreamChunk struct {
	Delta string
	Done  bool
	Err   error
}

// ChatRequest is a provider-neutral completion request.
type ChatRequest struct {
	// System carries system/developer instructions, normalized per provider
	// adapter. In v0 these parts must be text-only. System instructions are
	// not legal as mid-conversation Messages.
	System []ContentPart
	// Messages is the ordered conversation.
	Messages []Message
	// Tools the model may call.
	Tools []ToolDefinition
	// ToolChoice constrains tool use.
	ToolChoice ToolChoice
	// Purpose is an app-supplied label; not interpreted by the core package.
	Purpose Purpose
	// MaxTokens caps output tokens; zero means provider/model default.
	MaxTokens int
	// Temperature: nil means provider/model default; a non-nil 0 means
	// intentionally deterministic.
	Temperature *float32
	// Metadata is app-supplied, provider-neutral context for middleware and
	// logging. It is never sent to a provider unless a middleware or provider
	// explicitly opts a key in.
	Metadata map[string]string
	// Reasoning, if set, asks a reasoning model to bound its thinking. Each
	// adapter maps what its provider supports and omits the rest (the model
	// default then applies); ChatResponse.AppliedReasoning reports what was
	// sent. See ADR-0022.
	Reasoning *ReasoningConfig
	// ProviderParams are raw request-body fields for when the application
	// knows more about a model than this package does, keyed by provider.
	// Each adapter deep-merges only its own entry into the wire request
	// after mapping everything else: objects merge recursively, and any
	// other value replaces the mapped one. Values must marshal to JSON
	// (else bad_request). Middleware (estimation, validation, rate limiting)
	// does not see them, and they are outside the stability contract: the
	// wire shapes are the providers'. See ADR-0024.
	ProviderParams map[ProviderID]map[string]any
}

// ReasoningEffort is a provider-neutral reasoning-effort level. Use the
// constants; a value outside them is treated as unsupported and omitted.
type ReasoningEffort string

// Reasoning-effort levels, lowest to highest. Not every provider supports
// every level; ADR-0022 has the per-provider table.
const (
	ReasoningEffortNone    ReasoningEffort = "none"
	ReasoningEffortMinimal ReasoningEffort = "minimal"
	ReasoningEffortLow     ReasoningEffort = "low"
	ReasoningEffortMedium  ReasoningEffort = "medium"
	ReasoningEffortHigh    ReasoningEffort = "high"
	ReasoningEffortXHigh   ReasoningEffort = "xhigh"
	ReasoningEffortMax     ReasoningEffort = "max"
)

// ReasoningConfig bounds a reasoning model's thinking. Both fields are
// optional and independent; the zero value of each means "not set".
//
// Adapters map each field 1:1 to their provider's control, or omit it when
// the provider has none for that value, so the model default applies. They
// never predict which values a particular model accepts: a value the
// provider supports but the model rejects comes back as the provider's error.
type ReasoningConfig struct {
	// Effort is a reasoning-effort level.
	Effort ReasoningEffort
	// BudgetTokens is a thinking-token budget, for providers that take one
	// (Gemini 2.5 thinkingBudget, Anthropic budget_tokens on models before
	// Opus 4.7). Zero or negative means not set.
	BudgetTokens int
}

// StopReason is the provider-reported reason a response ended. Values are
// provider-specific strings; the core package does not normalize them in v0.
type StopReason string

// ChatResponse is a provider-neutral completion response.
type ChatResponse struct {
	// Message is the source of truth for the assistant turn. It can be
	// empty (no text, no tool calls) when a reasoning model spends its
	// whole output budget on reasoning; StopReason then reports the
	// provider's length/max-tokens reason. It may then still hold
	// ContentReasoning parts, which do not count as a reply. An empty
	// Message cannot be sent back to a provider, so do not append it to a
	// transcript you will resend.
	Message Message
	// Text and ToolCalls are convenience mirrors derived from Message.
	// Providers must populate Message; tests should assert mirrors match it.
	Text      string
	ToolCalls []ToolCall
	// StopReason is why the response ended.
	StopReason StopReason
	// Usage is normalized token accounting.
	Usage Usage
	// AppliedReasoning is the reasoning control the adapter actually sent,
	// in neutral terms: only the fields it mapped are set. It is nil when the
	// request had no Reasoning or nothing in it could be mapped, in which
	// case the model default applied. Compare it with ChatRequest.Reasoning
	// to see what a provider could not express. See ADR-0022.
	AppliedReasoning *ReasoningConfig
	// Raw is optional provider-specific data for advanced callers. It is
	// outside the stability contract; middleware must not depend on it and
	// callers that use it accept provider-package breakage risk.
	Raw any
}
