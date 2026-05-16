package openai

import (
	"encoding/json"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/responses"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

func chatBadRequest(model, msg string) *llms.ProviderError {
	return &llms.ProviderError{
		Provider: providerName, Model: model,
		Kind: llms.ErrorKindBadRequest, Message: msg,
	}
}

// systemText joins the (text-only, in v0) system parts.
func (c *ChatClient) systemText(req llms.ChatRequest) (string, *llms.ProviderError) {
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

func roleFor(r llms.Role) (responses.EasyInputMessageRole, bool) {
	switch r {
	case llms.RoleUser:
		return responses.EasyInputMessageRoleUser, true
	case llms.RoleAssistant:
		return responses.EasyInputMessageRoleAssistant, true
	default:
		return "", false
	}
}

// buildInput maps the app-neutral conversation to Responses input items.
// Unlike Anthropic, the Responses API takes an ordered item list with no
// strict alternation requirement: text -> message item; assistant tool_call
// -> function_call item; tool result -> function_call_output item.
func (c *ChatClient) buildInput(req llms.ChatRequest) ([]responses.ResponseInputItemUnionParam, *llms.ProviderError) {
	var items []responses.ResponseInputItemUnionParam
	for i := range req.Messages {
		m := req.Messages[i]
		for j := range m.Content {
			p := m.Content[j]
			switch p.Type {
			case llms.ContentText:
				if p.Text == "" {
					return nil, chatBadRequest(c.model, "empty text content part")
				}
				role, ok := roleFor(m.Role)
				if !ok {
					return nil, chatBadRequest(c.model, "text content not valid for role "+string(m.Role))
				}
				items = append(items, responses.ResponseInputItemParamOfMessage(p.Text, role))
			case llms.ContentToolCall:
				if p.ToolCall == nil {
					return nil, chatBadRequest(c.model, "tool_call content part with nil ToolCall")
				}
				items = append(items, responses.ResponseInputItemParamOfFunctionCall(
					string(p.ToolCall.Parameters), p.ToolCall.ID, p.ToolCall.Name))
			case llms.ContentToolResult:
				if p.ToolResult == nil {
					return nil, chatBadRequest(c.model, "tool_result content part with nil ToolResult")
				}
				items = append(items, responses.ResponseInputItemParamOfFunctionCallOutput(
					p.ToolResult.ToolCallID, p.ToolResult.Content))
			default:
				return nil, chatBadRequest(c.model, "unknown content part type: "+string(p.Type))
			}
		}
	}
	if len(items) == 0 {
		return nil, chatBadRequest(c.model, "request has no messages")
	}
	return items, nil
}

func (c *ChatClient) buildTools(req llms.ChatRequest) ([]responses.ToolUnionParam, *llms.ProviderError) {
	if len(req.Tools) == 0 {
		return nil, nil
	}
	tools := make([]responses.ToolUnionParam, 0, len(req.Tools))
	for i := range req.Tools {
		t := req.Tools[i]
		params := map[string]any{"type": "object"}
		if len(t.InputSchema) > 0 {
			var raw map[string]any
			if err := json.Unmarshal(t.InputSchema, &raw); err != nil {
				return nil, chatBadRequest(c.model, "tool "+t.Name+": invalid input schema JSON: "+err.Error())
			}
			if tv, ok := raw["type"]; ok {
				if s, _ := tv.(string); s != "object" {
					return nil, chatBadRequest(c.model, "tool "+t.Name+`: input schema type must be "object"`)
				}
			}
			params = raw
			params["type"] = "object"
			// OpenAI rejects a null "required"; it must be an array.
			if _, ok := params["required"]; !ok {
				params["required"] = []string{}
			}
		}
		fn := responses.FunctionToolParam{
			Name:       t.Name,
			Parameters: params,
			Strict:     openai.Bool(false),
		}
		if t.Description != "" {
			fn.Description = openai.String(t.Description)
		}
		tools = append(tools, responses.ToolUnionParam{OfFunction: &fn})
	}
	return tools, nil
}

func (c *ChatClient) toolChoice(tc llms.ToolChoice) (responses.ResponseNewParamsToolChoiceUnion, bool, *llms.ProviderError) {
	switch tc.Type {
	case llms.ToolChoiceAuto:
		return responses.ResponseNewParamsToolChoiceUnion{
			OfToolChoiceMode: openai.Opt(responses.ToolChoiceOptionsAuto),
		}, true, nil
	case llms.ToolChoiceNone:
		return responses.ResponseNewParamsToolChoiceUnion{
			OfToolChoiceMode: openai.Opt(responses.ToolChoiceOptionsNone),
		}, true, nil
	case llms.ToolChoiceTool:
		if tc.Name == "" {
			return responses.ResponseNewParamsToolChoiceUnion{}, false,
				chatBadRequest(c.model, `tool choice type "tool" requires a tool name`)
		}
		return responses.ResponseNewParamsToolChoiceUnion{
			OfFunctionTool: &responses.ToolChoiceFunctionParam{Name: tc.Name},
		}, true, nil
	default:
		return responses.ResponseNewParamsToolChoiceUnion{}, false, nil
	}
}

func (c *ChatClient) toParams(req llms.ChatRequest) (responses.ResponseNewParams, error) {
	sys, perr := c.systemText(req)
	if perr != nil {
		return responses.ResponseNewParams{}, perr
	}
	items, perr := c.buildInput(req)
	if perr != nil {
		return responses.ResponseNewParams{}, perr
	}
	tools, perr := c.buildTools(req)
	if perr != nil {
		return responses.ResponseNewParams{}, perr
	}

	params := responses.ResponseNewParams{
		Model: c.model,
		Input: responses.ResponseNewParamsInputUnion{OfInputItemList: items},
	}
	if sys != "" {
		params.Instructions = openai.String(sys)
	}
	if req.MaxTokens > 0 {
		params.MaxOutputTokens = openai.Int(int64(req.MaxTokens))
	}
	if req.Temperature != nil {
		params.Temperature = openai.Float(float64(*req.Temperature))
	}
	if len(tools) > 0 {
		params.Tools = tools
	}
	ch, ok, perr := c.toolChoice(req.ToolChoice)
	if perr != nil {
		return responses.ResponseNewParams{}, perr
	}
	if ok {
		params.ToolChoice = ch
	}
	return params, nil
}

// toChatResponse maps a Responses API result to the app-neutral
// ChatResponse. Message is the source of truth; Text and ToolCalls mirror it.
// ToolCall.ID carries the Responses call_id so a subsequent tool result
// round-trips to the correct function_call_output.
func toChatResponse(resp *responses.Response) llms.ChatResponse {
	var parts []llms.ContentPart
	var toolCalls []llms.ToolCall

	text := resp.OutputText()
	if text != "" {
		parts = append(parts, llms.ContentPart{Type: llms.ContentText, Text: text})
	}
	for i := range resp.Output {
		item := resp.Output[i]
		if item.Type == "function_call" {
			fc := item.AsFunctionCall()
			tc := llms.ToolCall{
				ID:         fc.CallID,
				Name:       fc.Name,
				Parameters: json.RawMessage(fc.Arguments),
			}
			toolCalls = append(toolCalls, tc)
			parts = append(parts, llms.ContentPart{Type: llms.ContentToolCall, ToolCall: &tc})
		}
	}

	return llms.ChatResponse{
		Message:    llms.Message{Role: llms.RoleAssistant, Content: parts},
		Text:       text,
		ToolCalls:  toolCalls,
		StopReason: llms.StopReason(resp.Status),
		Usage: llms.Usage{
			InputTokens:       int(resp.Usage.InputTokens),
			OutputTokens:      int(resp.Usage.OutputTokens),
			TotalTokens:       int(resp.Usage.TotalTokens),
			ProviderRequestID: resp.ID,
		},
		Raw: resp,
	}
}
