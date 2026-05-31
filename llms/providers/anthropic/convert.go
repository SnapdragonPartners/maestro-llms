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

// anthropicMaxCacheBreakpoints is Anthropic's hard limit on cache_control
// markers per request (system + content). Exceeding it is a provider 400, so
// the advisory CacheBreakpoint hint must never push past it.
const anthropicMaxCacheBreakpoints = 4

// cacheBudget caps how many content cache breakpoints become cache_control.
// The earliest `skip` marked parts are dropped (emitted as plain text) so the
// HONORED ones are the last in conversation order — the longest cached
// prefixes — and the total stays within the provider limit. Deterministic,
// and the hint stays advisory: dropping extras never fails the request.
type cacheBudget struct {
	skip int
	seen int
}

func (b *cacheBudget) honor() bool {
	keep := b.seen >= b.skip
	b.seen++
	return keep
}

func systemHasBreakpoint(parts []llms.ContentPart) bool {
	for i := range parts {
		if parts[i].CacheBreakpoint {
			return true
		}
	}
	return false
}

func countContentBreakpoints(msgs []llms.Message) int {
	n := 0
	for i := range msgs {
		for j := range msgs[i].Content {
			if msgs[i].Content[j].Type == llms.ContentText && msgs[i].Content[j].CacheBreakpoint {
				n++
			}
		}
	}
	return n
}

// blocksForParts converts app-neutral content parts to SDK content blocks,
// returning tool-result blocks separately so they can be emitted first within
// a user turn (Anthropic requires tool_result immediately after tool_use).
// Malformed parts (empty text, nil tool payload, unknown type) are rejected
// rather than silently dropped: dropping them changes conversation semantics
// and produces misleading downstream alternation errors.
func (c *Client) blocksForParts(parts []llms.ContentPart, cb *cacheBudget) (toolResults, others []anthropic.ContentBlockParamUnion, perr *llms.ProviderError) {
	for i := range parts {
		p := parts[i]
		switch p.Type {
		case llms.ContentText:
			if p.Text == "" {
				return nil, nil, badRequest(c.model, "empty text content part")
			}
			if p.CacheBreakpoint && cb.honor() {
				others = append(others, anthropic.ContentBlockParamUnion{OfText: &anthropic.TextBlockParam{
					Text:         p.Text,
					CacheControl: anthropic.NewCacheControlEphemeralParam(),
				}})
			} else {
				others = append(others, anthropic.NewTextBlock(p.Text))
			}
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
func (c *Client) buildMessages(req llms.ChatRequest, cb *cacheBudget) ([]anthropic.MessageParam, *llms.ProviderError) {
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
		tr, other, perr := c.blocksForParts(m.Content, cb)
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
	// Cap cache_control markers at Anthropic's limit. The system block (if
	// marked) consumes one; the remaining budget goes to the LAST content
	// breakpoints (longest cached prefixes). Excess is silently emitted as
	// plain text — the hint is advisory and must not fail the request.
	sysBP := systemHasBreakpoint(req.System)
	budget := anthropicMaxCacheBreakpoints
	if sysBP {
		budget--
	}
	skip := countContentBreakpoints(req.Messages) - budget
	if skip < 0 {
		skip = 0
	}
	msgs, perr := c.buildMessages(req, &cacheBudget{skip: skip})
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
		sysBlock := anthropic.TextBlockParam{Text: sys}
		// System is flattened to one block; a cache breakpoint on any system
		// part marks the (whole) system prompt as cacheable — the common
		// "cache the system prompt" case. It consumes one of the 4 markers
		// (accounted for in `budget` above).
		if sysBP {
			sysBlock.CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
		params.System = []anthropic.TextBlockParam{sysBlock}
	}
	if req.Temperature != nil {
		params.Temperature = anthropic.Float(float64(*req.Temperature))
	}
	if len(tools) > 0 {
		params.Tools = tools
	}
	if req.ToolChoice.RequiresTools() && len(req.Tools) == 0 {
		return anthropic.MessageNewParams{},
			badRequest(c.model, "tool choice "+string(req.ToolChoice.Type)+" requires at least one tool")
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
			InputTokens: int(msg.Usage.InputTokens),
			// Anthropic's wire output_tokens already INCLUDES thinking
			// tokens when extended thinking is enabled. The SDK exposes
			// no separate field for the visible/thinking split, so we
			// cannot derive ReasoningTokens. We carry the wire total in
			// OutputTokens (documented limitation in ADR-0016) and
			// mirror it as BillableOutputTokens for accounting.
			// Consumers wanting the visible/thinking breakdown must
			// inspect the assistant message's ThinkingBlock content.
			OutputTokens:         int(msg.Usage.OutputTokens),
			BillableOutputTokens: int(msg.Usage.OutputTokens),
			TotalTokens:          int(msg.Usage.InputTokens + msg.Usage.OutputTokens),
			CacheReadTokens:      int(msg.Usage.CacheReadInputTokens),
			CacheWriteTokens:     int(msg.Usage.CacheCreationInputTokens),
			ProviderRequestID:    msg.ID,
		},
		Raw: msg,
	}
}
