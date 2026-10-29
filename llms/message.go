package llms

// Role identifies the speaker of a message. There is no system role: system
// instructions are carried by ChatRequest.System, not as mid-conversation
// messages.
type Role string

const (
	// RoleUser is a message from the human/application user.
	RoleUser Role = "user"
	// RoleAssistant is a message from the model, which may contain tool calls.
	RoleAssistant Role = "assistant"
	// RoleTool is a message carrying tool results back to the model. A single
	// RoleTool message may contain multiple tool_result parts; provider
	// adapters split or merge as the provider wire format requires.
	RoleTool Role = "tool"
)

// ContentPartType is the discriminant for ContentPart. The set is intentionally
// small and may grow (e.g. images) without changing the request contract.
type ContentPartType string

const (
	// ContentText is a plain-text part; Text is populated.
	ContentText ContentPartType = "text"
	// ContentToolCall is an assistant tool invocation; ToolCall is populated.
	ContentToolCall ContentPartType = "tool_call"
	// ContentToolResult is a tool execution result; ToolResult is populated.
	ContentToolResult ContentPartType = "tool_result"
	// ContentReasoning is a model's reasoning from an assistant turn;
	// Reasoning is populated. Adapters emit it only where the provider
	// requires reasoning to be sent back (Anthropic thinking blocks) and
	// replay it only to the provider that produced it. See ADR-0021.
	ContentReasoning ContentPartType = "reasoning"
)

// ContentPart is one discriminated piece of a message. Exactly one of the
// payload fields is set, selected by Type. Tool calls and tool results are
// content parts, not side-channel fields, so a conversation round-trips
// unambiguously.
type ContentPart struct {
	Type       ContentPartType
	Text       string
	ToolCall   *ToolCall
	ToolResult *ToolResult
	Reasoning  *Reasoning
	// CacheBreakpoint is an optional, provider-neutral hint: everything in
	// the prompt up to and including this part may be prompt-cached.
	// Providers that expose explicit inline cache control honor it (Anthropic
	// maps it to cache_control: ephemeral on the corresponding block);
	// providers without an inline-breakpoint API ignore it (OpenAI prefix-
	// caches automatically; Gemini's explicit caching is a separate
	// cached-content API; Ollama has none). It is purely advisory: setting or
	// ignoring it never changes the model's output, only cache economics.
	CacheBreakpoint bool
}

// Reasoning is provider-owned reasoning state from an assistant turn, kept in
// the transcript so the provider can see its earlier reasoning when the
// conversation is sent back. It is opaque: replay it unchanged, or remove
// every reasoning part with WithoutReasoning. Editing or removing individual
// reasoning parts, or editing earlier turns while keeping later reasoning,
// can make a provider reject the request (Anthropic binds each thinking
// block to the conversation before it). See ADR-0021.
type Reasoning struct {
	// Provider is the adapter that produced this part. Adapters replay only
	// their own parts and drop the rest (ReasoningFrom), so a transcript can
	// move between providers.
	Provider ProviderID
	// Text is the human-readable reasoning or summary the provider returned,
	// for display and logging only. It may be empty (Anthropic's default
	// "omitted" display, redacted thinking) and is never sent back.
	Text string
	// ProviderData is the provider's own encoding of the reasoning, sent back
	// unchanged. The core never interprets it.
	ProviderData []byte
}

// Message is one app-neutral conversation turn. Provider adapters translate to
// and from the provider wire shape at the provider boundary.
type Message struct {
	Role    Role
	Content []ContentPart
}

// Text returns a plain-text content part.
func Text(s string) ContentPart {
	return ContentPart{Type: ContentText, Text: s}
}

// UserText returns a user message containing a single text part.
func UserText(s string) Message {
	return Message{Role: RoleUser, Content: []ContentPart{Text(s)}}
}

// AssistantText returns an assistant message containing a single text part.
func AssistantText(s string) Message {
	return Message{Role: RoleAssistant, Content: []ContentPart{Text(s)}}
}

// ToolResultMessage returns a RoleTool message carrying the given results. Pass
// every result for the tool calls in the preceding assistant turn; validation
// middleware rejects missing, duplicate, or orphaned tool results.
func ToolResultMessage(results ...ToolResult) Message {
	parts := make([]ContentPart, len(results))
	for i := range results {
		r := results[i]
		parts[i] = ContentPart{Type: ContentToolResult, ToolResult: &r}
	}
	return Message{Role: RoleTool, Content: parts}
}
