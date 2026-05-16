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
func blocksForParts(parts []llms.ContentPart) (toolResults, others []anthropic.ContentBlockParamUnion) {
	for i := range parts {
		p := parts[i]
		switch p.Type {
		case llms.ContentText:
			if p.Text != "" {
				others = append(others, anthropic.NewTextBlock(p.Text))
			}
		case llms.ContentToolCall:
			if p.ToolCall != nil {
				others = append(others, anthropic.NewToolUseBlock(
					p.ToolCall.ID, p.ToolCall.Parameters, p.ToolCall.Name))
			}
		case llms.ContentToolResult:
			if p.ToolResult != nil {
				toolResults = append(toolResults, anthropic.NewToolResultBlock(
					p.ToolResult.ToolCallID, p.ToolResult.Content, p.ToolResult.IsError))
			}
		}
	}
	return toolResults, others
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
		tr, other := blocksForParts(m.Content)
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
func (c *Client) buildTools(req llms.ChatRequest) ([]anthropic.ToolUnionParam, *llms.ProviderError) {
	if len(req.Tools) == 0 {
		return nil, nil
	}
	tools := make([]anthropic.ToolUnionParam, 0, len(req.Tools))
	for i := range req.Tools {
		t := req.Tools[i]
		schema := anthropic.ToolInputSchemaParam{}
		if len(t.InputSchema) > 0 {
			var raw map[string]any
			if err := json.Unmarshal(t.InputSchema, &raw); err != nil {
				return nil, badRequest(c.model, "tool "+t.Name+": invalid input schema JSON: "+err.Error())
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
		}
		tp := anthropic.ToolParam{Name: t.Name, InputSchema: schema}
		if t.Description != "" {
			tp.Description = anthropic.String(t.Description)
		}
		tools = append(tools, anthropic.ToolUnionParam{OfTool: &tp})
	}
	return tools, nil
}

func toolChoice(tc llms.ToolChoice) (anthropic.ToolChoiceUnionParam, bool) {
	switch tc.Type {
	case llms.ToolChoiceAuto:
		return anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}, true
	case llms.ToolChoiceNone:
		return anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}, true
	case llms.ToolChoiceTool:
		return anthropic.ToolChoiceUnionParam{OfTool: &anthropic.ToolChoiceToolParam{Name: tc.Name}}, true
	default:
		return anthropic.ToolChoiceUnionParam{}, false
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
	if ch, ok := toolChoice(req.ToolChoice); ok {
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
