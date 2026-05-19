package llms

import "encoding/json"

// ToolDefinition describes a tool the model may call. InputSchema is raw JSON
// Schema so the package does not impose a schema-generation library.
type ToolDefinition struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// ToolChoiceType constrains whether and how the model may call tools.
type ToolChoiceType string

const (
	// ToolChoiceAuto lets the model decide whether to call a tool.
	ToolChoiceAuto ToolChoiceType = "auto"
	// ToolChoiceNone forbids tool calls.
	ToolChoiceNone ToolChoiceType = "none"
	// ToolChoiceRequired forces the model to call at least one of the
	// offered tools but lets the model pick which (unlike ToolChoiceTool,
	// which names a specific tool). Maps to Anthropic "any", OpenAI
	// "required", Gemini ANY-mode. Ollama cannot enforce tool_choice, so
	// there it is best-effort: tools are offered, the model decides.
	ToolChoiceRequired ToolChoiceType = "required"
	// ToolChoiceTool forces the named tool; ToolChoice.Name must be set.
	ToolChoiceTool ToolChoiceType = "tool"
)

// ToolChoice expresses the caller's tool-use constraint for a request.
type ToolChoice struct {
	Type ToolChoiceType
	Name string // set when Type == ToolChoiceTool
}

// RequiresTools reports whether this choice is impossible without at least
// one offered tool: Required (must call some tool) and Tool (must call a
// named one). Provider adapters reject such a request up front rather than
// emitting an impossible call or silently degrading it.
func (tc ToolChoice) RequiresTools() bool {
	return tc.Type == ToolChoiceRequired || tc.Type == ToolChoiceTool
}

// ToolCall is a model request to invoke a tool. Parameters is the exact
// provider-emitted JSON, preserved verbatim to avoid lossy map[string]any
// round-trips; callers unmarshal it into their own typed structs.
type ToolCall struct {
	ID         string
	Name       string
	Parameters json.RawMessage
	// ProviderSignature is an opaque, provider-owned blob that must be
	// round-tripped unchanged when this tool call is sent back in a later
	// turn. The core never interprets it. It exists so provider-required
	// per-tool-call state survives the stateless response→app→request loop
	// without a per-client cache: e.g. Gemini 3's mandatory functionCall
	// `thought_signature`. Providers that don't use it leave it nil and
	// ignore it (like ContentPart.CacheBreakpoint). See ADR-0010.
	ProviderSignature []byte
}

// ToolResult is the result of executing a ToolCall, sent back in a RoleTool
// message. Content is a string in v0 (textual or JSON-string); structured or
// multimodal results may be added later without changing round-trip semantics.
type ToolResult struct {
	ToolCallID string
	Content    string
	IsError    bool
}
