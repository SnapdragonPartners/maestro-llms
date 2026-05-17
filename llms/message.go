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
// small in v0 and may grow (e.g. images) without changing the request contract.
type ContentPartType string

const (
	// ContentText is a plain-text part; Text is populated.
	ContentText ContentPartType = "text"
	// ContentToolCall is an assistant tool invocation; ToolCall is populated.
	ContentToolCall ContentPartType = "tool_call"
	// ContentToolResult is a tool execution result; ToolResult is populated.
	ContentToolResult ContentPartType = "tool_result"
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
