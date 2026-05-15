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
	// ToolChoiceTool forces the named tool; ToolChoice.Name must be set.
	ToolChoiceTool ToolChoiceType = "tool"
)

// ToolChoice expresses the caller's tool-use constraint for a request.
type ToolChoice struct {
	Type ToolChoiceType
	Name string // set when Type == ToolChoiceTool
}

// ToolCall is a model request to invoke a tool. Parameters is the exact
// provider-emitted JSON, preserved verbatim to avoid lossy map[string]any
// round-trips; callers unmarshal it into their own typed structs.
type ToolCall struct {
	ID         string
	Name       string
	Parameters json.RawMessage
}

// ToolResult is the result of executing a ToolCall, sent back in a RoleTool
// message. Content is a string in v0 (textual or JSON-string); structured or
// multimodal results may be added later without changing round-trip semantics.
type ToolResult struct {
	ToolCallID string
	Content    string
	IsError    bool
}
