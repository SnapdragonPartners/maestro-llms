package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
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
		case llms.ContentReasoning:
			// Only this provider's parts reach here (buildMessages runs
			// llms.ReasoningFrom first).
			b, err := thinkingBlockParam(p.Reasoning)
			if err != nil {
				return nil, nil, badRequest(c.model, err.Error())
			}
			others = append(others, b)
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

	// Replay only Anthropic's own reasoning; other providers' is dropped
	// (ADR-0021).
	msgs := llms.ReasoningFrom(providerName, req.Messages)
	for i := range msgs {
		m := msgs[i]
		tr, other, perr := c.blocksForParts(m.Content, cb)
		if perr != nil {
			return nil, perr
		}
		switch m.Role {
		case llms.RoleAssistant:
			flush()
			if !hasReply(m.Content) {
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

func (c *Client) toolChoice(req llms.ChatRequest) (param anthropic.ToolChoiceUnionParam, set bool, perr *llms.ProviderError) {
	tc := req.ToolChoice
	if len(req.Tools) == 0 {
		if tc.RequiresTools() {
			return anthropic.ToolChoiceUnionParam{}, false,
				badRequest(c.model, "tool choice "+string(tc.Type)+" requires at least one tool")
		}
		// With no tools offered, auto/none are equivalent to omitting the
		// tool choice, and strict servers reject one sent without tools (#88).
		return anthropic.ToolChoiceUnionParam{}, false, nil
	}
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
	rw := mapReasoning(req.Reasoning)
	params.Thinking = rw.thinking
	params.OutputConfig.Effort = rw.effort
	if len(tools) > 0 {
		params.Tools = tools
	}
	ch, ok, perr := c.toolChoice(req)
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
		case blockThinking, blockRedactedThinking:
			if part, ok := reasoningPart(&block); ok {
				parts = append(parts, part)
			}
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
		Usage:      buildUsage(msg),
		Raw:        msg,
	}
}

// buildUsage normalizes Anthropic usage per ADR-0016 as amended by ADR-0018,
// and input per ADR-0023.
// The wire output_tokens includes thinking; output_tokens_details.
// thinking_tokens is the thinking subset (the SDK documents it as always
// <= output_tokens). When the details field is present, OutputTokens is the
// visible remainder and ReasoningTokens the thinking count. When it is
// absent (older API versions or deployments), the combined wire count stays
// in OutputTokens and ReasoningTokens is zero rather than reported as a
// measured 0.
func buildUsage(msg *anthropic.Message) llms.Usage {
	wireOut := int(msg.Usage.OutputTokens)
	visible, reasoning := wireOut, 0
	if msg.Usage.OutputTokensDetails.JSON.ThinkingTokens.Valid() {
		reasoning = min(max(int(msg.Usage.OutputTokensDetails.ThinkingTokens), 0), wireOut)
		visible = wireOut - reasoning
	}
	// Anthropic's input_tokens excludes cache reads and writes; InputTokens
	// includes them, so it is all input processed (ADR-0023).
	read := max(int(msg.Usage.CacheReadInputTokens), 0)
	write := max(int(msg.Usage.CacheCreationInputTokens), 0)
	input := max(int(msg.Usage.InputTokens), 0) + read + write
	return llms.Usage{
		InputTokens:          input,
		OutputTokens:         visible,
		ReasoningTokens:      reasoning,
		BillableOutputTokens: wireOut,
		TotalTokens:          input + wireOut,
		CacheReadTokens:      read,
		CacheWriteTokens:     write,
		ProviderRequestID:    msg.ID,
	}
}

// Anthropic thinking block types.
const (
	blockThinking         = "thinking"
	blockRedactedThinking = "redacted_thinking"
)

// thinkingWire is the ProviderData encoding of an Anthropic thinking or
// redacted_thinking block: the block's own fields, which are sent back
// unchanged (ADR-0021).
type thinkingWire struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
}

// reasoningPart captures a thinking or redacted_thinking response block as a
// ContentReasoning part. The block's fields go into ProviderData verbatim;
// Text mirrors the readable thinking (empty under the default "omitted"
// display and for redacted blocks).
func reasoningPart(block *anthropic.ContentBlockUnion) (llms.ContentPart, bool) {
	w := thinkingWire{Type: block.Type}
	text := ""
	switch block.Type {
	case blockThinking:
		tb := block.AsThinking()
		w.Thinking, w.Signature = tb.Thinking, tb.Signature
		text = tb.Thinking
	case blockRedactedThinking:
		w.Data = block.AsRedactedThinking().Data
	default:
		return llms.ContentPart{}, false
	}
	data, err := json.Marshal(w)
	if err != nil {
		return llms.ContentPart{}, false
	}
	return llms.ContentPart{Type: llms.ContentReasoning, Reasoning: &llms.Reasoning{
		Provider:     providerName,
		Text:         text,
		ProviderData: data,
	}}, true
}

// thinkingBlockParam rebuilds the request block from a part reasoningPart
// produced. The readable Reasoning.Text is ignored: only ProviderData is
// sent back, so editing Text cannot alter the block.
func thinkingBlockParam(r *llms.Reasoning) (anthropic.ContentBlockParamUnion, error) {
	if r == nil {
		return anthropic.ContentBlockParamUnion{}, errors.New("reasoning content part with nil Reasoning")
	}
	var w thinkingWire
	if err := json.Unmarshal(r.ProviderData, &w); err != nil {
		return anthropic.ContentBlockParamUnion{}, fmt.Errorf("reasoning content part: malformed ProviderData: %w", err)
	}
	switch w.Type {
	case blockThinking:
		if w.Signature == "" {
			return anthropic.ContentBlockParamUnion{}, errors.New("reasoning content part: thinking block without signature")
		}
		return anthropic.NewThinkingBlock(w.Signature, w.Thinking), nil
	case blockRedactedThinking:
		if w.Data == "" {
			return anthropic.ContentBlockParamUnion{}, errors.New("reasoning content part: redacted_thinking block without data")
		}
		return anthropic.NewRedactedThinkingBlock(w.Data), nil
	default:
		return anthropic.ContentBlockParamUnion{}, fmt.Errorf("reasoning content part: unknown block type %q", w.Type)
	}
}

// hasReply reports whether an assistant message's parts include text or a
// tool call, the only parts an assistant turn sends besides reasoning
// (tool_result parts are split off and dropped there). A reasoning-only or
// tool-result-only assistant turn is rejected like an empty one, matching
// middleware.ValidationChat.
func hasReply(parts []llms.ContentPart) bool {
	for i := range parts {
		if t := parts[i].Type; t == llms.ContentText || t == llms.ContentToolCall {
			return true
		}
	}
	return false
}

// reasoningWire is the Anthropic mapping of a ReasoningConfig: the wire
// fields to set and the neutral record of what they express (ADR-0022).
type reasoningWire struct {
	applied  *llms.ReasoningConfig
	thinking anthropic.ThinkingConfigParamUnion
	effort   anthropic.OutputConfigEffort
}

// mapReasoning maps a ReasoningConfig 1:1 to Anthropic's controls, omitting
// what Anthropic can't express so the model default applies:
//
//   - Effort low/medium/high/xhigh/max → output_config.effort. Minimal has
//     no Anthropic level and is omitted.
//   - Effort none → thinking {type: "disabled"} (models from Opus 5.5 on
//     reject it; that is the provider's 400, not predicted here).
//   - BudgetTokens → thinking {type: "enabled", budget_tokens} (models
//     before Opus 4.7; budget must be at least 1024 and below MaxTokens).
//     It takes the thinking field, so it wins over Effort none.
func mapReasoning(r *llms.ReasoningConfig) reasoningWire {
	var w reasoningWire
	if r == nil {
		return w
	}
	var applied llms.ReasoningConfig
	if r.BudgetTokens > 0 {
		w.thinking = anthropic.ThinkingConfigParamOfEnabled(int64(r.BudgetTokens))
		applied.BudgetTokens = r.BudgetTokens
	}
	switch r.Effort {
	case llms.ReasoningEffortLow, llms.ReasoningEffortMedium, llms.ReasoningEffortHigh,
		llms.ReasoningEffortXHigh, llms.ReasoningEffortMax:
		w.effort = anthropic.OutputConfigEffort(r.Effort)
		applied.Effort = r.Effort
	case llms.ReasoningEffortNone:
		if r.BudgetTokens <= 0 {
			w.thinking = anthropic.ThinkingConfigParamUnion{OfDisabled: &anthropic.ThinkingConfigDisabledParam{}}
			applied.Effort = r.Effort
		}
	}
	if applied != (llms.ReasoningConfig{}) {
		w.applied = &applied
	}
	return w
}
