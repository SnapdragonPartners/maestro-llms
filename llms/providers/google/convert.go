package google

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"google.golang.org/genai"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// jsonObjectType is the JSON Schema object type; tool input schemas must be
// objects (the genai function-declaration parameters are an object schema).
const jsonObjectType = "object"

func badRequest(model, msg string) *llms.ProviderError {
	return &llms.ProviderError{
		Provider: providerName, Model: model,
		Kind: llms.ErrorKindBadRequest, Message: msg,
	}
}

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

// roleFor maps app-neutral roles to Gemini content roles. Tool results are
// carried in a user-role content (Gemini models function responses as
// user-provided).
func roleFor(r llms.Role) (string, bool) {
	switch r {
	case llms.RoleUser, llms.RoleTool:
		return "user", true
	case llms.RoleAssistant:
		return "model", true
	default:
		return "", false
	}
}

func (c *Client) buildContents(req llms.ChatRequest) ([]*genai.Content, *llms.ProviderError) {
	var contents []*genai.Content
	for i := range req.Messages {
		m := req.Messages[i]
		role, ok := roleFor(m.Role)
		if !ok {
			return nil, badRequest(c.model, "unknown message role: "+string(m.Role))
		}
		var parts []*genai.Part
		for j := range m.Content {
			p := m.Content[j]
			switch p.Type {
			case llms.ContentText:
				if p.Text == "" {
					return nil, badRequest(c.model, "empty text content part")
				}
				parts = append(parts, &genai.Part{Text: p.Text})
			case llms.ContentToolCall:
				if p.ToolCall == nil {
					return nil, badRequest(c.model, "tool_call content part with nil ToolCall")
				}
				args, err := rawToMap(p.ToolCall.Parameters)
				if err != nil {
					return nil, badRequest(c.model, "tool_call "+p.ToolCall.Name+": invalid parameters JSON: "+err.Error())
				}
				parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{
					ID: p.ToolCall.ID, Name: p.ToolCall.Name, Args: args,
				}})
			case llms.ContentToolResult:
				if p.ToolResult == nil {
					return nil, badRequest(c.model, "tool_result content part with nil ToolResult")
				}
				parts = append(parts, &genai.Part{FunctionResponse: &genai.FunctionResponse{
					// Gemini correlates by function name, not an opaque id.
					Name: p.ToolResult.ToolCallID,
					Response: map[string]any{
						"content":  p.ToolResult.Content,
						"is_error": p.ToolResult.IsError,
					},
				}})
			default:
				return nil, badRequest(c.model, "unknown content part type: "+string(p.Type))
			}
		}
		if len(parts) > 0 {
			contents = append(contents, &genai.Content{Role: role, Parts: parts})
		}
	}
	if len(contents) == 0 {
		return nil, badRequest(c.model, "request has no messages")
	}
	return contents, nil
}

func rawToMap(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return m, nil
}

func genaiType(t string) genai.Type {
	switch t {
	case "string":
		return genai.TypeString
	case "number":
		return genai.TypeNumber
	case "integer":
		return genai.TypeInteger
	case "boolean":
		return genai.TypeBoolean
	case "array":
		return genai.TypeArray
	case jsonObjectType:
		return genai.TypeObject
	default:
		return genai.TypeString
	}
}

// jsonSchemaToGenai recursively converts a parsed JSON Schema to a
// *genai.Schema. genai uses an uppercase Type enum, so a raw JSON Schema
// cannot be unmarshaled directly.
func jsonSchemaToGenai(raw map[string]any) *genai.Schema {
	s := &genai.Schema{}
	if d, ok := raw["description"].(string); ok {
		s.Description = d
	}
	if t, ok := raw["type"].(string); ok {
		s.Type = genaiType(t)
	}
	if props, ok := raw["properties"].(map[string]any); ok {
		s.Properties = make(map[string]*genai.Schema, len(props))
		for name, pv := range props {
			if pm, ok := pv.(map[string]any); ok {
				s.Properties[name] = jsonSchemaToGenai(pm)
			}
		}
	}
	if items, ok := raw["items"].(map[string]any); ok {
		s.Items = jsonSchemaToGenai(items)
	}
	if req, ok := raw["required"].([]any); ok {
		for _, r := range req {
			if rs, ok := r.(string); ok {
				s.Required = append(s.Required, rs)
			}
		}
	}
	if en, ok := raw["enum"].([]any); ok {
		for _, e := range en {
			s.Enum = append(s.Enum, fmt.Sprint(e))
		}
	}
	return s
}

func (c *Client) buildTools(req llms.ChatRequest) ([]*genai.Tool, *llms.ProviderError) {
	if len(req.Tools) == 0 {
		return nil, nil
	}
	decls := make([]*genai.FunctionDeclaration, 0, len(req.Tools))
	for i := range req.Tools {
		t := req.Tools[i]
		schema := &genai.Schema{Type: genai.TypeObject}
		if len(t.InputSchema) > 0 {
			var raw map[string]any
			if err := json.Unmarshal(t.InputSchema, &raw); err != nil {
				return nil, badRequest(c.model, "tool "+t.Name+": invalid input schema JSON: "+err.Error())
			}
			if tv, ok := raw["type"].(string); ok && tv != jsonObjectType {
				return nil, badRequest(c.model, "tool "+t.Name+`: input schema type must be "object"`)
			}
			raw["type"] = jsonObjectType
			schema = jsonSchemaToGenai(raw)
		}
		decls = append(decls, &genai.FunctionDeclaration{
			Name: t.Name, Description: t.Description, Parameters: schema,
		})
	}
	return []*genai.Tool{{FunctionDeclarations: decls}}, nil
}

func (c *Client) toolConfig(tc llms.ToolChoice) (*genai.ToolConfig, *llms.ProviderError) {
	mk := func(mode genai.FunctionCallingConfigMode, names []string) *genai.ToolConfig {
		return &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{
			Mode: mode, AllowedFunctionNames: names,
		}}
	}
	switch tc.Type {
	case llms.ToolChoiceAuto:
		return mk(genai.FunctionCallingConfigModeAuto, nil), nil
	case llms.ToolChoiceNone:
		return mk(genai.FunctionCallingConfigModeNone, nil), nil
	case llms.ToolChoiceTool:
		if tc.Name == "" {
			return nil, badRequest(c.model, `tool choice type "tool" requires a tool name`)
		}
		return mk(genai.FunctionCallingConfigModeAny, []string{tc.Name}), nil
	default:
		return nil, nil
	}
}

func (c *Client) toParams(req llms.ChatRequest) ([]*genai.Content, *genai.GenerateContentConfig, *llms.ProviderError) {
	sys, perr := c.systemText(req)
	if perr != nil {
		return nil, nil, perr
	}
	contents, perr := c.buildContents(req)
	if perr != nil {
		return nil, nil, perr
	}
	tools, perr := c.buildTools(req)
	if perr != nil {
		return nil, nil, perr
	}
	tcfg, perr := c.toolConfig(req.ToolChoice)
	if perr != nil {
		return nil, nil, perr
	}

	cfg := &genai.GenerateContentConfig{}
	if sys != "" {
		cfg.SystemInstruction = &genai.Content{Parts: []*genai.Part{{Text: sys}}}
	}
	if req.Temperature != nil {
		cfg.Temperature = req.Temperature
	}
	if req.MaxTokens > 0 {
		mt := req.MaxTokens
		if mt > math.MaxInt32 {
			mt = math.MaxInt32
		}
		//nolint:gosec // G115: mt is clamped to math.MaxInt32 immediately above
		cfg.MaxOutputTokens = int32(mt)
	}
	if tools != nil {
		cfg.Tools = tools
	}
	if tcfg != nil {
		cfg.ToolConfig = tcfg
	}
	return contents, cfg, nil
}

// toChatResponse maps a genai result to the app-neutral ChatResponse.
// Candidate parts are iterated in order so Message preserves interleaving of
// text and tool calls (thought parts are excluded from content). Message is
// the source of truth; Text and ToolCalls mirror it.
func toChatResponse(result *genai.GenerateContentResponse) llms.ChatResponse {
	var parts []llms.ContentPart
	var toolCalls []llms.ToolCall
	var textMirror strings.Builder
	var stop llms.StopReason

	if len(result.Candidates) > 0 && result.Candidates[0] != nil {
		cand := result.Candidates[0]
		stop = llms.StopReason(cand.FinishReason)
		if cand.Content != nil {
			for _, p := range cand.Content.Parts {
				switch {
				case p == nil || p.Thought:
					continue
				case p.FunctionCall != nil:
					fc := p.FunctionCall
					id := fc.ID
					if id == "" {
						id = fc.Name // Gemini correlates by name
					}
					args, _ := json.Marshal(fc.Args)
					tc := llms.ToolCall{ID: id, Name: fc.Name, Parameters: args}
					toolCalls = append(toolCalls, tc)
					parts = append(parts, llms.ContentPart{Type: llms.ContentToolCall, ToolCall: &tc})
				case p.Text != "":
					parts = append(parts, llms.ContentPart{Type: llms.ContentText, Text: p.Text})
					textMirror.WriteString(p.Text)
				}
			}
		}
	}

	usage := llms.Usage{}
	if u := result.UsageMetadata; u != nil {
		usage = llms.Usage{
			InputTokens:  int(u.PromptTokenCount),
			OutputTokens: int(u.CandidatesTokenCount),
			TotalTokens:  int(u.TotalTokenCount),
		}
	}

	usage.ProviderRequestID = result.ResponseID

	return llms.ChatResponse{
		Message:    llms.Message{Role: llms.RoleAssistant, Content: parts},
		Text:       textMirror.String(),
		ToolCalls:  toolCalls,
		StopReason: stop,
		Usage:      usage,
		Raw:        result,
	}
}
