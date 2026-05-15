package testllm

import (
	"context"
	"fmt"
	"sync"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// FakeChatClient is a deterministic, concurrency-safe llms.ChatClient for
// tests. Configure it with scripted responses and/or a scripted error, or take
// full control with Func. It records every request for assertions.
// Fields are grouped pointer-bearing first (fieldalignment); construct with
// keyed literals.
type FakeChatClient struct {
	// Func, if set, fully overrides the scripted behavior below.
	Func func(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error)
	// Err, if set (and Func is nil), is returned from every Complete call.
	Err error
	// ModelRef is reported by Model.
	ModelRef llms.ModelRef
	// Responses are returned in order across successive Complete calls. When
	// exhausted the last response is repeated; if empty, a zero response with
	// the text from Text is returned.
	Responses []llms.ChatResponse
	// Text is a convenience: when Responses is empty, every Complete returns
	// an assistant message with this text.
	Text string

	calls []llms.ChatRequest
	mu    sync.Mutex
	idx   int
}

// Model returns the configured model reference.
func (f *FakeChatClient) Model() llms.ModelRef { return f.ModelRef }

// Complete records the request and returns the next scripted response.
func (f *FakeChatClient) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
	// Record before the context check so a canceled call is still observable
	// (the doc promises every request is recorded).
	f.mu.Lock()
	f.calls = append(f.calls, copyChatRequest(req))
	f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return llms.ChatResponse{}, fmt.Errorf("testllm: context done: %w", err)
	}

	f.mu.Lock()
	idx := f.idx
	if idx < len(f.Responses) {
		f.idx++
	}
	f.mu.Unlock()

	if f.Func != nil {
		return f.Func(ctx, req)
	}
	if f.Err != nil {
		return llms.ChatResponse{}, f.Err
	}

	switch {
	case len(f.Responses) == 0:
		msg := llms.AssistantText(f.Text)
		return llms.ChatResponse{Message: msg, Text: f.Text}, nil
	case idx < len(f.Responses):
		return f.Responses[idx], nil
	default:
		return f.Responses[len(f.Responses)-1], nil
	}
}

// Calls returns a copy of the recorded requests, in order.
func (f *FakeChatClient) Calls() []llms.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]llms.ChatRequest, len(f.calls))
	for i := range f.calls {
		out[i] = copyChatRequest(f.calls[i])
	}
	return out
}

// Reset clears recorded calls and rewinds the response cursor.
func (f *FakeChatClient) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
	f.idx = 0
}

// TextResponse builds a spec-faithful ChatResponse: Message is the source of
// truth and Text mirrors it, as providers must populate. Prefer this over
// hand-setting only Text so scripted fakes do not drift from the contract.
func TextResponse(s string) llms.ChatResponse {
	return llms.ChatResponse{
		Message:    llms.AssistantText(s),
		Text:       s,
		StopReason: "end_turn",
	}
}

// ToolCallResponse builds a ChatResponse whose assistant turn invokes a single
// tool, useful for scripting a tool-calling exchange.
func ToolCallResponse(id, name, paramsJSON string) llms.ChatResponse {
	tc := llms.ToolCall{ID: id, Name: name, Parameters: []byte(paramsJSON)}
	return llms.ChatResponse{
		Message: llms.Message{
			Role:    llms.RoleAssistant,
			Content: []llms.ContentPart{{Type: llms.ContentToolCall, ToolCall: &tc}},
		},
		ToolCalls:  []llms.ToolCall{tc},
		StopReason: "tool_use",
	}
}
