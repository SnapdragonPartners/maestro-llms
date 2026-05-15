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
// implemented in v0; StreamChunk is a forward declaration so the contract is
// fixed before any provider grows streaming support.
type StreamingChatClient interface {
	Stream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error)
}

// StreamChunk is one incremental piece of a streamed response. Its shape is a
// v0 placeholder and may change before streaming ships (v0.3).
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
}

// StopReason is the provider-reported reason a response ended. Values are
// provider-specific strings; the core package does not normalize them in v0.
type StopReason string

// ChatResponse is a provider-neutral completion response.
type ChatResponse struct {
	// Message is the source of truth for the assistant turn.
	Message Message
	// Text and ToolCalls are convenience mirrors derived from Message.
	// Providers must populate Message; tests should assert mirrors match it.
	Text      string
	ToolCalls []ToolCall
	// StopReason is why the response ended.
	StopReason StopReason
	// Usage is normalized token accounting.
	Usage Usage
	// Raw is optional provider-specific data for advanced callers. It is
	// outside the stability contract; middleware must not depend on it and
	// callers that use it accept provider-package breakage risk.
	Raw any
}
