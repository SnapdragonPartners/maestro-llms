package vllm

import (
	"encoding/json"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

func chatBadRequest(model, msg string) *llms.ProviderError {
	return &llms.ProviderError{
		Provider: providerName, Model: model,
		Kind: llms.ErrorKindBadRequest, Message: msg,
	}
}

// systemText joins the (text-only, in v0) system parts into a single string
// suitable for prepending as a system-role message.
func (c *Client) systemText(req llms.ChatRequest) (string, *llms.ProviderError) {
	var b strings.Builder
	for i := range req.System {
		p := req.System[i]
		if p.Type != llms.ContentText {
			return "", chatBadRequest(c.model, "system parts must be text-only in v0")
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

// buildMessages maps llms.Messages to the Chat Completions message union
// list. Unlike the Responses API used by the openai package, Chat
// Completions takes a strictly alternating role-based message array where:
//   - user/assistant text messages are flat text content.
//   - assistant tool-call turns carry tool_calls alongside (optional) text.
//   - tool results come back as one tool-role message per tool_call_id (so
//     a multi-result llms.RoleTool message splits into N tool messages).
//
//nolint:cyclop // explicit case-per-content-type kept linear for readability
func (c *Client) buildMessages(req llms.ChatRequest, system string) ([]openai.ChatCompletionMessageParamUnion, *llms.ProviderError) {
	// Drop other providers' reasoning parts (ADR-0021); none is ours to replay.
	req.Messages = llms.ReasoningFrom(providerName, req.Messages)
	var out []openai.ChatCompletionMessageParamUnion
	if system != "" {
		out = append(out, openai.SystemMessage(system))
	}
	for i := range req.Messages {
		m := req.Messages[i]
		switch m.Role {
		case llms.RoleUser:
			text, perr := joinTextParts(c.model, m.Content, "user")
			if perr != nil {
				return nil, perr
			}
			out = append(out, openai.UserMessage(text))
		case llms.RoleAssistant:
			msg, perr := c.assistantMessage(m.Content)
			if perr != nil {
				return nil, perr
			}
			out = append(out, msg)
		case llms.RoleTool:
			for j := range m.Content {
				p := m.Content[j]
				if p.Type != llms.ContentToolResult || p.ToolResult == nil {
					return nil, chatBadRequest(c.model, "tool messages must contain only tool_result parts")
				}
				out = append(out, openai.ToolMessage(p.ToolResult.Content, p.ToolResult.ToolCallID))
			}
		default:
			return nil, chatBadRequest(c.model, "unknown message role: "+string(m.Role))
		}
	}
	if len(out) == 0 || (system != "" && len(out) == 1) {
		return nil, chatBadRequest(c.model, "request has no messages")
	}
	return out, nil
}

// joinTextParts collects all text parts in a single role-tagged message
// into one string. Returns a bad-request error if any non-text part shows
// up on a role that doesn't allow it, if any text part is empty, or if
// the part list is empty. Empty user/assistant messages cause confusing
// server-side errors on Chat Completions and other adapters reject them
// here too.
func joinTextParts(model string, parts []llms.ContentPart, role string) (string, *llms.ProviderError) {
	if len(parts) == 0 {
		return "", chatBadRequest(model, role+" message has empty content")
	}
	var b strings.Builder
	for i := range parts {
		p := parts[i]
		if p.Type != llms.ContentText {
			return "", chatBadRequest(model, role+" message content must be text-only")
		}
		if p.Text == "" {
			return "", chatBadRequest(model, role+" message has an empty text part")
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

// assistantMessage builds the assistant ChatCompletionMessageParamUnion,
// combining any text parts with any tool_call parts into a single message.
// In Chat Completions, an assistant turn can carry both `content` text and
// `tool_calls` simultaneously, but must carry at least one of the two —
// an empty content slice with no tool calls is rejected up front to avoid
// emitting an empty assistant message on the wire (other adapters do the
// same).
func (c *Client) assistantMessage(parts []llms.ContentPart) (openai.ChatCompletionMessageParamUnion, *llms.ProviderError) {
	if len(parts) == 0 {
		return openai.ChatCompletionMessageParamUnion{},
			chatBadRequest(c.model, "assistant message has empty content")
	}
	var (
		text      strings.Builder
		toolCalls []openai.ChatCompletionMessageToolCallUnionParam
	)
	for i := range parts {
		p := parts[i]
		switch p.Type {
		case llms.ContentText:
			if p.Text == "" {
				return openai.ChatCompletionMessageParamUnion{},
					chatBadRequest(c.model, "assistant message has an empty text part")
			}
			if text.Len() > 0 {
				text.WriteString("\n\n")
			}
			text.WriteString(p.Text)
		case llms.ContentToolCall:
			if p.ToolCall == nil {
				return openai.ChatCompletionMessageParamUnion{},
					chatBadRequest(c.model, "tool_call content part with nil ToolCall")
			}
			toolCalls = append(toolCalls, openai.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
					ID: p.ToolCall.ID,
					Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
						Name:      p.ToolCall.Name,
						Arguments: string(p.ToolCall.Parameters),
					},
				},
			})
		default:
			return openai.ChatCompletionMessageParamUnion{},
				chatBadRequest(c.model, "assistant content part type not supported: "+string(p.Type))
		}
	}
	if text.Len() == 0 && len(toolCalls) == 0 {
		// Possible if every part typed Text was non-empty in shape but
		// content stripped during build. Belt-and-suspenders.
		return openai.ChatCompletionMessageParamUnion{},
			chatBadRequest(c.model, "assistant message has neither text content nor tool calls")
	}
	asst := openai.ChatCompletionAssistantMessageParam{}
	if text.Len() > 0 {
		asst.Content = openai.ChatCompletionAssistantMessageParamContentUnion{
			OfString: param.NewOpt(text.String()),
		}
	}
	if len(toolCalls) > 0 {
		asst.ToolCalls = toolCalls
	}
	return openai.ChatCompletionMessageParamUnion{OfAssistant: &asst}, nil
}

// buildTools maps llms.ToolDefinitions to Chat Completions tool params.
// Empty schema is coerced to {"type":"object"}; missing "required" becomes
// an empty array (some servers reject null).
func (c *Client) buildTools(req llms.ChatRequest) ([]openai.ChatCompletionToolUnionParam, *llms.ProviderError) {
	if len(req.Tools) == 0 {
		return nil, nil
	}
	tools := make([]openai.ChatCompletionToolUnionParam, 0, len(req.Tools))
	for i := range req.Tools {
		t := req.Tools[i]
		params := map[string]any{"type": "object"}
		if len(t.InputSchema) > 0 {
			var raw map[string]any
			if err := json.Unmarshal(t.InputSchema, &raw); err != nil {
				return nil, chatBadRequest(c.model, "tool "+t.Name+": invalid input schema JSON: "+err.Error())
			}
			// Valid JSON (e.g. literal `null`, arrays unmarshalled into
			// the wrong target, etc.) can leave raw nil; writing into a
			// nil map panics. Reject up front.
			if raw == nil {
				return nil, chatBadRequest(c.model, "tool "+t.Name+`: input schema must be a JSON object, got non-object value`)
			}
			if tv, ok := raw["type"]; ok {
				if s, _ := tv.(string); s != "object" {
					return nil, chatBadRequest(c.model, "tool "+t.Name+`: input schema type must be "object"`)
				}
			}
			params = raw
			params["type"] = "object"
			if v, ok := params["required"]; !ok || v == nil {
				params["required"] = []string{}
			}
		}
		fn := shared.FunctionDefinitionParam{
			Name:       t.Name,
			Parameters: params,
		}
		if t.Description != "" {
			fn.Description = param.NewOpt(t.Description)
		}
		tools = append(tools, openai.ChatCompletionFunctionTool(fn))
	}
	return tools, nil
}

// toolChoice maps llms.ToolChoice to the Chat Completions tool_choice union.
// "auto"/"none"/"required" are the string-mode variants; ToolChoiceTool
// becomes a named-function choice. With no tools offered it rejects a
// choice that requires tools and otherwise sends none (#88).
func (c *Client) toolChoice(req llms.ChatRequest) (openai.ChatCompletionToolChoiceOptionUnionParam, bool, *llms.ProviderError) {
	tc := req.ToolChoice
	if len(req.Tools) == 0 {
		if tc.RequiresTools() {
			return openai.ChatCompletionToolChoiceOptionUnionParam{}, false,
				chatBadRequest(c.model, "tool choice "+string(tc.Type)+" requires at least one tool")
		}
		// With no tools offered, auto/none are equivalent to omitting the
		// tool choice, and strict servers reject one sent without tools (#88).
		return openai.ChatCompletionToolChoiceOptionUnionParam{}, false, nil
	}
	switch tc.Type {
	case llms.ToolChoiceAuto:
		return openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: param.NewOpt("auto")}, true, nil
	case llms.ToolChoiceNone:
		return openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: param.NewOpt("none")}, true, nil
	case llms.ToolChoiceRequired:
		return openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: param.NewOpt("required")}, true, nil
	case llms.ToolChoiceTool:
		if tc.Name == "" {
			return openai.ChatCompletionToolChoiceOptionUnionParam{}, false,
				chatBadRequest(c.model, `tool choice type "tool" requires a tool name`)
		}
		return openai.ChatCompletionToolChoiceOptionUnionParam{
			OfFunctionToolChoice: &openai.ChatCompletionNamedToolChoiceParam{
				Function: openai.ChatCompletionNamedToolChoiceFunctionParam{Name: tc.Name},
			},
		}, true, nil
	default:
		return openai.ChatCompletionToolChoiceOptionUnionParam{}, false, nil
	}
}

// toParams builds the Chat Completions request from the app-neutral
// ChatRequest.
func (c *Client) toParams(req llms.ChatRequest) (openai.ChatCompletionNewParams, error) {
	sys, perr := c.systemText(req)
	if perr != nil {
		return openai.ChatCompletionNewParams{}, perr
	}
	msgs, perr := c.buildMessages(req, sys)
	if perr != nil {
		return openai.ChatCompletionNewParams{}, perr
	}
	tools, perr := c.buildTools(req)
	if perr != nil {
		return openai.ChatCompletionNewParams{}, perr
	}

	params := openai.ChatCompletionNewParams{
		Model:    c.model,
		Messages: msgs,
	}
	if req.MaxTokens > 0 {
		params.MaxTokens = param.NewOpt(int64(req.MaxTokens))
	}
	if req.Temperature != nil {
		params.Temperature = param.NewOpt(float64(*req.Temperature))
	}
	if len(tools) > 0 {
		params.Tools = tools
	}
	ch, ok, perr := c.toolChoice(req)
	if perr != nil {
		return openai.ChatCompletionNewParams{}, perr
	}
	if ok {
		params.ToolChoice = ch
	}
	if effort, _ := mapEffort(req.Reasoning); effort != "" {
		params.ReasoningEffort = effort
	}
	return params, nil
}

// buildUsage maps a ChatCompletion's wire usage to llms.Usage with the
// ADR-0016 normalization: OpenAI Chat Completions' wire CompletionTokens
// is total (visible + reasoning) and CompletionTokensDetails.ReasoningTokens
// is the subset. We surface OutputTokens as visible-only by subtracting
// reasoning, and the wire total as BillableOutputTokens. Most vLLM-served
// models will have ReasoningTokens=0 because they aren't reasoning models;
// a future reasoning-capable model served via vLLM benefits from this
// without a code change.
func buildUsage(resp *openai.ChatCompletion) llms.Usage {
	billable := int(resp.Usage.CompletionTokens)
	// Clamp reasoning to the billed total: some OpenAI-compatible servers
	// report reasoning_tokens > completion_tokens on length-truncated
	// responses (LunaRoute glm-5.3: 82 > 80). Clamping keeps
	// Output + Reasoning == Billable, as the Anthropic adapter does.
	reasoning := min(max(int(resp.Usage.CompletionTokensDetails.ReasoningTokens), 0), billable)
	visible := billable - reasoning
	// prompt_tokens already includes cached tokens; the details are
	// subsets (ADR-0023). Servers that don't cache leave them zero.
	input := int(resp.Usage.PromptTokens)
	cacheRead, cacheWrite := llms.CacheSplit(input,
		int(resp.Usage.PromptTokensDetails.CachedTokens), int(resp.Usage.PromptTokensDetails.CacheWriteTokens))
	return llms.Usage{
		InputTokens:          input,
		OutputTokens:         visible,
		ReasoningTokens:      reasoning,
		BillableOutputTokens: billable,
		TotalTokens:          int(resp.Usage.TotalTokens),
		CacheReadTokens:      cacheRead,
		CacheWriteTokens:     cacheWrite,
		ProviderRequestID:    resp.ID,
	}
}

// toResponse maps a Chat Completion result to the app-neutral ChatResponse.
// Message is the source of truth; Text and ToolCalls mirror it.
// FinishReason passes through verbatim (vLLM uses OpenAI's set:
// "stop"/"length"/"tool_calls"/"content_filter").
func (c *Client) toResponse(resp *openai.ChatCompletion) llms.ChatResponse {
	usage := buildUsage(resp)
	if len(resp.Choices) == 0 {
		return llms.ChatResponse{
			Message: llms.Message{Role: llms.RoleAssistant},
			Usage:   usage,
			Raw:     resp,
		}
	}
	choice := resp.Choices[0]

	// Sized for one text part + one per tool call (a conservative upper
	// bound — typical responses are 1-2 parts).
	parts := make([]llms.ContentPart, 0, 1+len(choice.Message.ToolCalls))
	toolCalls := make([]llms.ToolCall, 0, len(choice.Message.ToolCalls))
	if choice.Message.Content != "" {
		parts = append(parts, llms.ContentPart{Type: llms.ContentText, Text: choice.Message.Content})
	}
	for i := range choice.Message.ToolCalls {
		tc := choice.Message.ToolCalls[i]
		call := llms.ToolCall{
			ID:         tc.ID,
			Name:       tc.Function.Name,
			Parameters: json.RawMessage(tc.Function.Arguments),
		}
		toolCalls = append(toolCalls, call)
		parts = append(parts, llms.ContentPart{Type: llms.ContentToolCall, ToolCall: &call})
	}

	return llms.ChatResponse{
		Message:    llms.Message{Role: llms.RoleAssistant, Content: parts},
		Text:       choice.Message.Content,
		ToolCalls:  toolCalls,
		StopReason: llms.StopReason(choice.FinishReason),
		Usage:      usage,
		Raw:        resp,
	}
}

// mapEffort maps a ReasoningConfig's effort 1:1 to the OpenAI-compatible
// reasoning_effort field, which has all seven neutral levels. How strongly a
// server and model honor it varies (GLM on LunaRoute treats it as soft,
// DeepSeek honors none; see #86); any other value is omitted.
// BudgetTokens has no Chat Completions control and is never mapped (ADR-0022).
// It returns the wire value ("" when omitted) and what was applied.
func mapEffort(r *llms.ReasoningConfig) (shared.ReasoningEffort, *llms.ReasoningConfig) {
	if r == nil {
		return "", nil
	}
	switch r.Effort {
	case llms.ReasoningEffortNone, llms.ReasoningEffortMinimal, llms.ReasoningEffortLow,
		llms.ReasoningEffortMedium, llms.ReasoningEffortHigh, llms.ReasoningEffortXHigh,
		llms.ReasoningEffortMax:
		return shared.ReasoningEffort(r.Effort), &llms.ReasoningConfig{Effort: r.Effort}
	default:
		return "", nil
	}
}
