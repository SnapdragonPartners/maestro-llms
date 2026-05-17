package anthropic

import (
	"encoding/json"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

func badRequest(model, msg string) *llms.ProviderError {
	return &llms.ProviderError{
		Provider: providerName, Model: model,
		Kind: llms.ErrorKindBadRequest, Message: msg,
	}
}

// systemText extracts the system prompt from req.System. v0 system parts must
// be text-only.
func (c *Client) systemText(req llms.ChatRequest) (string, *llms.ProviderError) {
	var b strings.Builder
	for i := range req.System {
		p := req.System[i]
		if p.Type != llms.ContentText {
			return "", badRequest(c.model, "system parts must be text-only in v0")
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

// blocksForParts converts app-neutral content parts to SDK content blocks,
// returning tool-result blocks separately so they can be emitted first within
// a user turn (Anthropic requires tool_result immediately after tool_use).
// Malformed parts (empty text, nil tool payload, unknown type) are rejected
// rather than silently dropped: dropping them changes conversation semantics
// and produces misleading downstream alternation errors.
func (c *Client) blocksForParts(parts []llms.ContentPart) (toolResults, others []anthropic.ContentBlockParamUnion, perr *llms.ProviderError) {
	for i := range parts {
		p := parts[i]
		switch p.Type {
		case llms.ContentText:
			if p.Text == "" {
				return nil, nil, badRequest(c.model, "empty text content part")
			}
			others = append(others, anthropic.NewTextBlock(p.Text))
		case llms.ContentToolCall:
			if p.ToolCall == nil {
				return nil, nil, badRequest(c.model, "tool_call content part with nil ToolCall")
			}
			others = append(others, anthropic.NewToolUseBlock(
				p.ToolCall.ID, p.ToolCall.Parameters, p.ToolCall.Name))
		case llms.ContentToolResult:
			if p.ToolResult == nil {
				return nil, nil, badRequest(c.model, "tool_result content part with nil ToolResult")
			}
			toolResults = append(toolResults, anthropic.NewToolResultBlock(
				p.ToolResult.ToolCallID, p.ToolResult.Content, p.ToolResult.IsError))
		default:
			return nil, nil, badRequest(c.model, "unknown content part type: "+string(p.Type))
		}
	}
	return toolResults, others, nil
}

// buildMessages maps the app-neutral conversation to Anthropic's strict
// user/assistant alternation: RoleTool and RoleUser both map to user, and
// consecutive user-side turns are merged so alternation holds.
func (c *Client) buildMessages(req llms.ChatRequest) ([]anthropic.MessageParam, *llms.ProviderError) {
	var out []anthropic.MessageParam
	var userBuf []anthropic.ContentBlockParamUnion

	flush := func() {
		if len(userBuf) > 0 {
			out = append(out, anthropic.MessageParam{
				Role:    anthropic.MessageParamRoleUser,
				Content: userBuf,
			})
			userBuf = nil
		}
	}

	for i := range req.Messages {
		m := req.Messages[i]
		tr, other, perr := c.blocksForParts(m.Content)
		if perr != nil {
			return nil, perr
		}
		switch m.Role {
		case llms.RoleAssistant:
			flush()
			if len(other) == 0 {
				return nil, badRequest(c.model, "assistant message has no content")
			}
			out = append(out, anthropic.MessageParam{
				Role:    anthropic.MessageParamRoleAssistant,
				Content: other,
			})
		case llms.RoleUser, llms.RoleTool:
			// tool_result blocks first, then text, within the user turn.
			userBuf = append(userBuf, tr...)
			userBuf = append(userBuf, other...)
		default:
			return nil, badRequest(c.model, "unknown message role: "+string(m.Role))
		}
	}
	flush()

	if len(out) == 0 {
		return nil, badRequest(c.model, "request has no messages")
	}
	if out[0].Role != anthropic.MessageParamRoleUser {
		return nil, badRequest(c.model, "conversation must start with a user message")
	}
	return out, nil
}

// buildTools converts raw JSON Schema tool definitions. Unknown top-level
// schema keys are passed through via ExtraFields so $defs/additionalProperties
// etc. are preserved.
// parseToolSchema converts one tool's raw JSON Schema. Anthropic requires an
// object schema (the SDK forces type:"object"); a non-object top-level type is
// rejected rather than silently dropped. Unknown keys pass through ExtraFields
// so $defs/additionalProperties etc. are preserved.
func (c *Client) parseToolSchema(name string, rawSchema json.RawMessage) (anthropic.ToolInputSchemaParam, *llms.ProviderError) {
	schema := anthropic.ToolInputSchemaParam{}
	if len(rawSchema) == 0 {
		return schema, nil
	}
	var raw map[string]any
	if err := json.Unmarshal(rawSchema, &raw); err != nil {
		return schema, badRequest(c.model, "tool "+name+": invalid input schema JSON: "+err.Error())
	}
	if tv, ok := raw["type"]; ok {
		if s, _ := tv.(string); s != "object" {
			return schema, badRequest(c.model, "tool "+name+`: input schema type must be "object"`)
		}
	}
	if props, ok := raw["properties"]; ok {
		schema.Properties = props
	}
	if reqd, ok := raw["required"].([]any); ok {
		for _, r := range reqd {
			if s, ok := r.(string); ok {
				schema.Required = append(schema.Required, s)
			}
		}
	}
	extra := map[string]any{}
	for k, v := range raw {
		if k != "properties" && k != "required" && k != "type" {
			extra[k] = v
		}
	}
	if len(extra) > 0 {
		schema.ExtraFields = extra
	}
	return schema, nil
}

func (c *Client) buildTools(req llms.ChatRequest) ([]anthropic.ToolUnionParam, *llms.ProviderError) {
	if len(req.Tools) == 0 {
		return nil, nil
	}
	tools := make([]anthropic.ToolUnionParam, 0, len(req.Tools))
	for i := range req.Tools {
		t := req.Tools[i]
		schema, perr := c.parseToolSchema(t.Name, t.InputSchema)
		if perr != nil {
			return nil, perr
		}
		tp := anthropic.ToolParam{Name: t.Name, InputSchema: schema}
		if t.Description != "" {
			tp.Description = anthropic.String(t.Description)
		}
		tools = append(tools, anthropic.ToolUnionParam{OfTool: &tp})
	}
	return tools, nil
}

func (c *Client) toolChoice(tc llms.ToolChoice) (param anthropic.ToolChoiceUnionParam, set bool, perr *llms.ProviderError) {
	switch tc.Type {
	case llms.ToolChoiceAuto:
		return anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}, true, nil
	case llms.ToolChoiceNone:
		return anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}, true, nil
	case llms.ToolChoiceRequired:
		return anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}, true, nil
	case llms.ToolChoiceTool:
		if tc.Name == "" {
			return anthropic.ToolChoiceUnionParam{}, false,
				badRequest(c.model, `tool choice type "tool" requires a tool name`)
		}
		return anthropic.ToolChoiceUnionParam{OfTool: &anthropic.ToolChoiceToolParam{Name: tc.Name}}, true, nil
	default:
		return anthropic.ToolChoiceUnionParam{}, false, nil
	}
}

func (c *Client) toParams(req llms.ChatRequest) (anthropic.MessageNewParams, error) {
	sys, perr := c.systemText(req)
	if perr != nil {
		return anthropic.MessageNewParams{}, perr
	}
	msgs, perr := c.buildMessages(req)
	if perr != nil {
		return anthropic.MessageNewParams{}, perr
	}
	tools, perr := c.buildTools(req)
	if perr != nil {
		return anthropic.MessageNewParams{}, perr
	}

	maxTokens := int64(req.MaxTokens)
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	params := anthropic.MessageNewParams{
		Model:     c.model,
		Messages:  msgs,
		MaxTokens: maxTokens,
	}
	if sys != "" {
		params.System = []anthropic.TextBlockParam{{Text: sys}}
	}
	if req.Temperature != nil {
		params.Temperature = anthropic.Float(float64(*req.Temperature))
	}
	if len(tools) > 0 {
		params.Tools = tools
	}
	ch, ok, perr := c.toolChoice(req.ToolChoice)
	if perr != nil {
		return anthropic.MessageNewParams{}, perr
	}
	if ok {
		params.ToolChoice = ch
	}
	return params, nil
}

// toResponse maps an SDK Message to the app-neutral ChatResponse. Message is
// the source of truth; Text and ToolCalls are convenience mirrors.
func toResponse(msg *anthropic.Message) llms.ChatResponse {
	var parts []llms.ContentPart
	var text strings.Builder
	var toolCalls []llms.ToolCall

	for i := range msg.Content {
		block := msg.Content[i]
		switch block.Type {
		case "text":
			t := block.AsText().Text
			text.WriteString(t)
			parts = append(parts, llms.ContentPart{Type: llms.ContentText, Text: t})
		case "tool_use":
			tu := block.AsToolUse()
			tc := llms.ToolCall{
				ID:         tu.ID,
				Name:       tu.Name,
				Parameters: tu.Input,
			}
			toolCalls = append(toolCalls, tc)
			parts = append(parts, llms.ContentPart{Type: llms.ContentToolCall, ToolCall: &tc})
		}
	}

	return llms.ChatResponse{
		Message:    llms.Message{Role: llms.RoleAssistant, Content: parts},
		Text:       text.String(),
		ToolCalls:  toolCalls,
		StopReason: llms.StopReason(msg.StopReason),
		Usage: llms.Usage{
			InputTokens:       int(msg.Usage.InputTokens),
			OutputTokens:      int(msg.Usage.OutputTokens),
			TotalTokens:       int(msg.Usage.InputTokens + msg.Usage.OutputTokens),
			CacheReadTokens:   int(msg.Usage.CacheReadInputTokens),
			CacheWriteTokens:  int(msg.Usage.CacheCreationInputTokens),
			ProviderRequestID: msg.ID,
		},
		Raw: msg,
	}
}
