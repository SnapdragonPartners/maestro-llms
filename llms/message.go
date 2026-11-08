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
// small and grows in minor releases (ADR-0032), so a switch over it needs a
// default case.
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
	// ContentImage is an image the user sends to the model; Media is
	// populated and Media.MediaType is an image type ("image/png"). Allowed
	// on user messages only. Each adapter accepts the image types its
	// provider documents and rejects others before the call. See ADR-0040.
	ContentImage ContentPartType = "image"
	// ContentDocument is a document the user sends to the model, such as a
	// PDF; Media is populated and Media.MediaType is not an image type.
	// Allowed on user messages only. Adapters whose provider takes no
	// documents (Ollama, openaicompat) reject it before the call. See
	// ADR-0040.
	ContentDocument ContentPartType = "document"
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
	Media      *Media
	// CacheBreakpoint is an optional, provider-neutral hint: everything in
	// the prompt up to and including this part may be prompt-cached.
	// Providers that expose explicit inline cache control honor it (Anthropic
	// maps it to cache_control: ephemeral on the corresponding text, image
	// or document block, ADR-0040);
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

// Media is the payload of an image or document part: the raw bytes and their
// media type. Adapters encode the bytes as their provider requires (base64,
// a data URI); callers never pre-encode. Only inline bytes are supported:
// URLs and provider file references are not part of this type. See
// ADR-0040.
type Media struct {
	// MediaType is the IANA media type of Data, for example "image/png" or
	// "application/pdf". Parameters ("; charset=utf-8") are allowed and
	// ignored when matching a provider's supported types; matching is
	// case-insensitive.
	MediaType string
	// Data is the raw content, not base64-encoded. It must be non-empty.
	Data []byte
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

// Image returns an image content part with the given media type (for example
// "image/png") and raw bytes. The bytes are not copied.
func Image(mediaType string, data []byte) ContentPart {
	return ContentPart{Type: ContentImage, Media: &Media{MediaType: mediaType, Data: data}}
}

// Document returns a document content part with the given media type (for
// example "application/pdf") and raw bytes. The bytes are not copied.
func Document(mediaType string, data []byte) ContentPart {
	return ContentPart{Type: ContentDocument, Media: &Media{MediaType: mediaType, Data: data}}
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
