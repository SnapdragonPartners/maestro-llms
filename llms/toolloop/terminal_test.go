package toolloop_test

import (
	"context"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

const submitToolName = "submit"

// submitTool is a terminal tool: it validates that the arguments contain
// "ok" and sets Stop on a valid submission, or returns a model-visible
// IsError on an invalid one so the model can resubmit (ADR-0017).
func submitTool(executions *int) toolloop.Tool {
	return toolloop.Tool{
		Definition: llms.ToolDefinition{Name: submitToolName},
		Execute: func(_ context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
			*executions++
			if !strings.Contains(string(call.Parameters), "ok") {
				return toolloop.ToolResult{Content: "invalid submission", IsError: true}, nil
			}
			return toolloop.ToolResult{Content: "accepted", Stop: true}, nil
		},
	}
}

// multiCallResp builds one assistant turn containing several tool calls.
func multiCallResp(calls ...llms.ToolCall) llms.ChatResponse {
	parts := make([]llms.ContentPart, len(calls))
	for i := range calls {
		parts[i] = llms.ContentPart{Type: llms.ContentToolCall, ToolCall: &calls[i]}
	}
	return llms.ChatResponse{
		Message:    llms.Message{Role: llms.RoleAssistant, Content: parts},
		ToolCalls:  calls,
		StopReason: "tool_use",
	}
}

func lastToolResults(t *testing.T, out toolloop.Outcome) []llms.ContentPart {
	t.Helper()
	last := out.Messages[len(out.Messages)-1]
	if last.Role != llms.RoleTool {
		t.Fatalf("last message role = %q, want tool (well-formed transcript)", last.Role)
	}
	return last.Content
}

func TestRun_Terminal_AloneEndsLoop(t *testing.T) {
	var submits int
	client := scriptedResponses(
		toolCallResp("s1", submitToolName, `{"v":"ok"}`, llms.Usage{InputTokens: 4}),
		finalTextResp("should-not-be-seen", llms.Usage{}),
	)
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  client,
		Request: userReq("submit"),
		Tools:   []toolloop.Tool{submitTool(&submits)},
	})
	if out.Kind != toolloop.OutcomeTerminalTool {
		t.Fatalf("kind = %q, want terminal_tool; err=%v", out.Kind, out.Err)
	}
	if out.Err != nil {
		t.Fatalf("Err = %v, want nil", out.Err)
	}
	if got := len(client.Calls()); got != 1 {
		t.Fatalf("provider called %d times, want 1 (no call after terminal)", got)
	}
	if out.TerminalCall.ID != "s1" || string(out.TerminalCall.Parameters) != `{"v":"ok"}` {
		t.Fatalf("TerminalCall = %+v, want s1 with submitted params", out.TerminalCall)
	}
	if !out.TerminalResult.Stop || out.TerminalResult.Content != "accepted" {
		t.Fatalf("TerminalResult = %+v, want Stop + accepted", out.TerminalResult)
	}
	if out.Iterations != 1 || out.TotalUsage.InputTokens != 4 {
		t.Fatalf("Iterations=%d TotalUsage=%+v, want 1 / input=4", out.Iterations, out.TotalUsage)
	}
	if len(out.Response.ToolCalls) != 1 || out.Response.ToolCalls[0].ID != "s1" {
		t.Fatalf("Response is not the terminating turn: %+v", out.Response)
	}
	// user, asst(s1), tool(s1)
	if len(out.Messages) != 3 {
		t.Fatalf("Messages len = %d, want 3", len(out.Messages))
	}
	res := lastToolResults(t, out)
	if res[0].ToolResult.ToolCallID != "s1" || res[0].ToolResult.IsError {
		t.Fatalf("terminal result in transcript = %+v", res[0].ToolResult)
	}
}

func TestRun_Terminal_MixedTurn_ReturnSemantics(t *testing.T) {
	// echo, submit, echo in one turn: the first echo runs, submit stops,
	// the trailing echo is NOT executed but gets a synthesized IsError
	// result so every call stays paired.
	var echoes, submits int
	echo := toolloop.Tool{
		Definition: llms.ToolDefinition{Name: echoToolName},
		Execute: func(_ context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
			echoes++
			return toolloop.ToolResult{Content: string(call.Parameters)}, nil
		},
	}
	client := scriptedResponses(multiCallResp(
		llms.ToolCall{ID: "e1", Name: echoToolName, Parameters: []byte(`{"i":1}`)},
		llms.ToolCall{ID: "s1", Name: submitToolName, Parameters: []byte(`{"v":"ok"}`)},
		llms.ToolCall{ID: "e2", Name: echoToolName, Parameters: []byte(`{"i":2}`)},
	))
	var events []toolloop.ToolCallEvent
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:     client,
		Request:    userReq("go"),
		Tools:      []toolloop.Tool{echo, submitTool(&submits)},
		OnToolCall: func(e toolloop.ToolCallEvent) { events = append(events, e) },
	})
	if out.Kind != toolloop.OutcomeTerminalTool {
		t.Fatalf("kind = %q, want terminal_tool; err=%v", out.Kind, out.Err)
	}
	if echoes != 1 || submits != 1 {
		t.Fatalf("executions echo=%d submit=%d, want 1/1 (trailing echo must not run)", echoes, submits)
	}
	if out.TerminalCall.ID != "s1" {
		t.Fatalf("TerminalCall.ID = %q, want s1", out.TerminalCall.ID)
	}
	res := lastToolResults(t, out)
	if len(res) != 3 {
		t.Fatalf("tool results = %d, want 3 (one per call)", len(res))
	}
	ids := []string{res[0].ToolResult.ToolCallID, res[1].ToolResult.ToolCallID, res[2].ToolResult.ToolCallID}
	if ids[0] != "e1" || ids[1] != "s1" || ids[2] != "e2" {
		t.Fatalf("result order = %v, want [e1 s1 e2]", ids)
	}
	if res[0].ToolResult.IsError || res[1].ToolResult.IsError {
		t.Fatal("executed results should not be errors")
	}
	if !res[2].ToolResult.IsError || !strings.Contains(res[2].ToolResult.Content, "not executed") {
		t.Fatalf("skipped result = %+v, want synthesized not-executed IsError", res[2].ToolResult)
	}
	// Events only for attempted calls; the terminal call's event shows Stop.
	if len(events) != 2 || events[0].Call.ID != "e1" || events[1].Call.ID != "s1" || !events[1].Result.Stop {
		t.Fatalf("events = %+v, want [e1, s1(Stop)]", events)
	}
}

func TestRun_Terminal_InvalidThenValidResubmission(t *testing.T) {
	var submits int
	client := scriptedResponses(
		toolCallResp("s1", submitToolName, `{"v":"bad"}`, llms.Usage{}),
		toolCallResp("s2", submitToolName, `{"v":"ok"}`, llms.Usage{}),
	)
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  client,
		Request: userReq("submit"),
		Tools:   []toolloop.Tool{submitTool(&submits)},
	})
	if out.Kind != toolloop.OutcomeTerminalTool {
		t.Fatalf("kind = %q, want terminal_tool; err=%v", out.Kind, out.Err)
	}
	if submits != 2 || out.Iterations != 2 || out.TerminalCall.ID != "s2" {
		t.Fatalf("submits=%d Iterations=%d TerminalCall=%q, want 2/2/s2", submits, out.Iterations, out.TerminalCall.ID)
	}
	// The invalid submission's IsError result was fed back to the model.
	second := client.Calls()[1]
	firstResult := second.Messages[len(second.Messages)-1].Content[0].ToolResult
	if firstResult.ToolCallID != "s1" || !firstResult.IsError {
		t.Fatalf("second request's last result = %+v, want IsError for s1", firstResult)
	}
}

func TestRun_Terminal_OnMaxIterationsBoundaryIsHonored(t *testing.T) {
	var echoes, submits int
	echo := toolloop.Tool{
		Definition: llms.ToolDefinition{Name: echoToolName},
		Execute: func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) {
			echoes++
			return toolloop.ToolResult{Content: "x"}, nil
		},
	}
	client := scriptedResponses(
		toolCallResp("e1", echoToolName, `{}`, llms.Usage{}),
		toolCallResp("s1", submitToolName, `{"v":"ok"}`, llms.Usage{}),
	)
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:        client,
		Request:       userReq("go"),
		Tools:         []toolloop.Tool{echo, submitTool(&submits)},
		MaxIterations: 2,
	})
	if out.Kind != toolloop.OutcomeTerminalTool {
		t.Fatalf("kind = %q, want terminal_tool (terminal on limit turn honored); err=%v", out.Kind, out.Err)
	}
	if out.Iterations != 2 || echoes != 1 || submits != 1 {
		t.Fatalf("Iterations=%d echoes=%d submits=%d, want 2/1/1", out.Iterations, echoes, submits)
	}
}

func TestRun_Terminal_StopWithIsErrorIsToolError(t *testing.T) {
	bad := toolloop.Tool{
		Definition: llms.ToolDefinition{Name: submitToolName},
		Execute: func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) {
			return toolloop.ToolResult{Content: "?", IsError: true, Stop: true}, nil
		},
	}
	client := scriptedResponses(toolCallResp("s1", submitToolName, `{}`, llms.Usage{}))
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  client,
		Request: userReq("go"),
		Tools:   []toolloop.Tool{bad},
	})
	if out.Kind != toolloop.OutcomeToolError {
		t.Fatalf("kind = %q, want tool_error", out.Kind)
	}
	if out.Err == nil || !strings.Contains(out.Err.Error(), "Stop and IsError") {
		t.Fatalf("Err = %v, want Stop+IsError contract violation", out.Err)
	}
}

func TestRun_Terminal_TranscriptIsRefeedable(t *testing.T) {
	// The returned transcript must pass the validation middleware's
	// structural pairing rules so callers can continue the conversation.
	var submits int
	client := scriptedResponses(multiCallResp(
		llms.ToolCall{ID: "s1", Name: submitToolName, Parameters: []byte(`{"v":"ok"}`)},
		llms.ToolCall{ID: "e1", Name: echoToolName, Parameters: []byte(`{}`)},
	))
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  client,
		Request: userReq("go"),
		Tools:   []toolloop.Tool{submitTool(&submits), echoTool()},
	})
	if out.Kind != toolloop.OutcomeTerminalTool {
		t.Fatalf("kind = %q, want terminal_tool; err=%v", out.Kind, out.Err)
	}
	msgs := append(out.Messages, llms.UserText("thanks, now summarize"))
	followUp := &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.TextResponse("summary")}}
	if _, err := validatedClient(followUp).Complete(context.Background(), llms.ChatRequest{Messages: msgs}); err != nil {
		t.Fatalf("re-feeding terminal transcript failed validation: %v", err)
	}
}

func validatedClient(c llms.ChatClient) llms.ChatClient {
	return middleware.ChainChat(c, middleware.ValidationChat())
}
