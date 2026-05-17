package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/internal/apierr"
)

// Ollama /api/chat wire types. Tool args and tool input schemas are kept as
// json.RawMessage so they round-trip with full fidelity (no map/orderedmap
// intermediary).

type wireToolFunc struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type wireToolCall struct {
	ID       string       `json:"id,omitempty"`
	Function wireToolFunc `json:"function"`
}

//nolint:govet // fieldalignment: internal JSON wire DTO, layout irrelevant.
type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wireTool struct {
	Type     string      `json:"type"`
	Function wireToolDef `json:"function"`
}

//nolint:govet // fieldalignment: internal JSON wire DTO, layout irrelevant.
type wireRequest struct {
	Model    string         `json:"model"`
	Messages []wireMessage  `json:"messages"`
	Stream   bool           `json:"stream"`
	Tools    []wireTool     `json:"tools,omitempty"`
	Options  map[string]any `json:"options,omitempty"`
}

type wireResponse struct {
	Model           string      `json:"model"`
	Message         wireMessage `json:"message"`
	DoneReason      string      `json:"done_reason,omitempty"`
	Error           string      `json:"error,omitempty"`
	PromptEvalCount int         `json:"prompt_eval_count,omitempty"`
	EvalCount       int         `json:"eval_count,omitempty"`
	Done            bool        `json:"done"`
}

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

func roleFor(r llms.Role) (string, bool) {
	switch r {
	case llms.RoleUser:
		return "user", true
	case llms.RoleAssistant:
		return "assistant", true
	case llms.RoleTool:
		return "tool", true
	default:
		return "", false
	}
}

// partAllowed enforces the app-neutral contract's role↔part-type rules: text
// only on user/assistant, tool_call only on assistant, tool_result only on
// RoleTool. Anything else (incl. unknown part types) is rejected rather than
// silently producing an invalid/mislabeled Ollama message.
func partAllowed(r llms.Role, t llms.ContentPartType) bool {
	switch t {
	case llms.ContentText:
		return r == llms.RoleUser || r == llms.RoleAssistant
	case llms.ContentToolCall:
		return r == llms.RoleAssistant
	case llms.ContentToolResult:
		return r == llms.RoleTool
	default:
		return false
	}
}

// messageParts converts one app message into wire messages (a tool result
// becomes its own role:"tool" message, so one app message can yield several).
func (c *Client) messageParts(m llms.Message) ([]wireMessage, *llms.ProviderError) {
	role, ok := roleFor(m.Role)
	if !ok {
		return nil, badRequest(c.model, "unknown message role: "+string(m.Role))
	}
	var out []wireMessage
	var text strings.Builder
	var toolCalls []wireToolCall
	for j := range m.Content {
		p := m.Content[j]
		if !partAllowed(m.Role, p.Type) {
			return nil, badRequest(c.model,
				fmt.Sprintf("%q content part is not valid in a %q message", p.Type, m.Role))
		}
		switch p.Type {
		case llms.ContentText:
			if p.Text == "" {
				return nil, badRequest(c.model, "empty text content part")
			}
			text.WriteString(p.Text)
		case llms.ContentToolCall:
			if p.ToolCall == nil {
				return nil, badRequest(c.model, "tool_call content part with nil ToolCall")
			}
			toolCalls = append(toolCalls, wireToolCall{
				ID: p.ToolCall.ID,
				Function: wireToolFunc{
					Name:      p.ToolCall.Name,
					Arguments: p.ToolCall.Parameters,
				},
			})
		case llms.ContentToolResult:
			if p.ToolResult == nil {
				return nil, badRequest(c.model, "tool_result content part with nil ToolResult")
			}
			out = append(out, wireMessage{
				Role:       "tool",
				Content:    p.ToolResult.Content,
				ToolCallID: p.ToolResult.ToolCallID,
			})
		}
	}
	if text.Len() > 0 || len(toolCalls) > 0 {
		out = append(out, wireMessage{Role: role, Content: text.String(), ToolCalls: toolCalls})
	}
	return out, nil
}

func (c *Client) buildTools(req llms.ChatRequest) ([]wireTool, *llms.ProviderError) {
	if len(req.Tools) == 0 {
		return nil, nil
	}
	tools := make([]wireTool, 0, len(req.Tools))
	for i := range req.Tools {
		t := req.Tools[i]
		params := json.RawMessage(`{"type":"object"}`)
		if len(t.InputSchema) > 0 {
			// Must be a JSON object: `null`, arrays, and scalars unmarshal
			// into a struct without error and would slip past a type-only
			// check, then be sent as an invalid `parameters`.
			var obj map[string]any
			if err := json.Unmarshal(t.InputSchema, &obj); err != nil || obj == nil {
				return nil, badRequest(c.model, "tool "+t.Name+": input schema must be a JSON object")
			}
			if tv, ok := obj["type"].(string); ok && tv != "object" {
				return nil, badRequest(c.model, "tool "+t.Name+`: input schema type must be "object"`)
			}
			params = t.InputSchema
		}
		tools = append(tools, wireTool{
			Type:     "function",
			Function: wireToolDef{Name: t.Name, Description: t.Description, Parameters: params},
		})
	}
	return tools, nil
}

// validateToolChoice enforces that a forced tool choice names a tool that is
// actually offered. Ollama cannot enforce tool_choice, but caller intent must
// not be silently lost.
func (c *Client) validateToolChoice(req llms.ChatRequest) *llms.ProviderError {
	if req.ToolChoice.Type != llms.ToolChoiceTool {
		return nil
	}
	if req.ToolChoice.Name == "" {
		return badRequest(c.model, `tool choice type "tool" requires a tool name`)
	}
	for i := range req.Tools {
		if req.Tools[i].Name == req.ToolChoice.Name {
			return nil
		}
	}
	return badRequest(c.model, "tool choice references tool not in Tools: "+req.ToolChoice.Name)
}

func (c *Client) toWire(req llms.ChatRequest) (*wireRequest, *llms.ProviderError) {
	sys, perr := c.systemText(req)
	if perr != nil {
		return nil, perr
	}
	var msgs []wireMessage
	if sys != "" {
		msgs = append(msgs, wireMessage{Role: "system", Content: sys})
	}
	for i := range req.Messages {
		mp, mperr := c.messageParts(req.Messages[i])
		if mperr != nil {
			return nil, mperr
		}
		msgs = append(msgs, mp...)
	}
	if len(msgs) == 0 || (len(msgs) == 1 && msgs[0].Role == "system") {
		return nil, badRequest(c.model, "request has no messages")
	}

	tools, perr := c.buildTools(req)
	if perr != nil {
		return nil, perr
	}

	if perr := c.validateToolChoice(req); perr != nil {
		return nil, perr
	}

	w := &wireRequest{Model: c.model, Messages: msgs, Stream: false}
	// Ollama has no tool_choice. ToolChoiceNone disables tools by omitting
	// them; otherwise tools are offered and the model decides. Neither a
	// forced "tool" choice nor "required" can be enforced — both are
	// best-effort here (see MAESTRO_DIVERGENCES OL2).
	if req.ToolChoice.Type != llms.ToolChoiceNone {
		w.Tools = tools
	}

	opts := map[string]any{}
	if req.Temperature != nil {
		opts["temperature"] = *req.Temperature
	}
	if req.MaxTokens > 0 {
		opts["num_predict"] = req.MaxTokens
	}
	if len(opts) > 0 {
		w.Options = opts
	}
	return w, nil
}

// httpStatusErr lets the shared apierr classifier consume a non-2xx HTTP
// response we produced ourselves (real status + headers).
type httpStatusErr struct {
	header http.Header
	msg    string
	status int
}

func (e *httpStatusErr) Error() string { return fmt.Sprintf("ollama: status %d: %s", e.status, e.msg) }

func (c *Client) classify(err error) error {
	return apierr.Classify(providerName, c.model, err, func(e error) (int, http.Header, string, bool) {
		var he *httpStatusErr
		if !errors.As(e, &he) {
			return 0, nil, "", false
		}
		return he.status, he.header, he.msg, true
	})
}

func (c *Client) do(ctx context.Context, wr *wireRequest) (*wireResponse, error) {
	body, err := json.Marshal(wr)
	if err != nil {
		return nil, badRequest(c.model, "marshal request: "+err.Error())
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, c.classify(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, c.classify(err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, c.classify(err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return nil, c.classify(&httpStatusErr{status: httpResp.StatusCode, header: httpResp.Header, msg: msg})
	}

	var wresp wireResponse
	if err := json.Unmarshal(raw, &wresp); err != nil {
		return nil, c.classify(&httpStatusErr{status: httpResp.StatusCode, header: httpResp.Header, msg: "invalid response JSON: " + err.Error()})
	}
	if wresp.Error != "" {
		return nil, c.classify(&httpStatusErr{status: httpResp.StatusCode, header: httpResp.Header, msg: wresp.Error})
	}
	return &wresp, nil
}

// toChatResponse maps a wire response to the app-neutral ChatResponse.
// Message is the source of truth; Text and ToolCalls mirror it. Ollama does
// not return tool-call IDs, so a synthetic stable index id is assigned.
func toChatResponse(resp *wireResponse) llms.ChatResponse {
	parts := make([]llms.ContentPart, 0, 1+len(resp.Message.ToolCalls))
	toolCalls := make([]llms.ToolCall, 0, len(resp.Message.ToolCalls))

	if resp.Message.Content != "" {
		parts = append(parts, llms.ContentPart{Type: llms.ContentText, Text: resp.Message.Content})
	}
	for i := range resp.Message.ToolCalls {
		tc := resp.Message.ToolCalls[i]
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		call := llms.ToolCall{ID: id, Name: tc.Function.Name, Parameters: tc.Function.Arguments}
		toolCalls = append(toolCalls, call)
		parts = append(parts, llms.ContentPart{Type: llms.ContentToolCall, ToolCall: &call})
	}

	stop := resp.DoneReason
	if stop == "" && resp.Done {
		stop = "stop"
	}

	return llms.ChatResponse{
		Message:    llms.Message{Role: llms.RoleAssistant, Content: parts},
		Text:       resp.Message.Content,
		ToolCalls:  toolCalls,
		StopReason: llms.StopReason(stop),
		Usage: llms.Usage{
			InputTokens:  resp.PromptEvalCount,
			OutputTokens: resp.EvalCount,
			TotalTokens:  resp.PromptEvalCount + resp.EvalCount,
		},
		Raw: resp,
	}
}
