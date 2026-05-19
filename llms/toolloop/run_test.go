package toolloop_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

// helpers ----------------------------------------------------------------

func userReq(text string) llms.ChatRequest {
	return llms.ChatRequest{Messages: []llms.Message{llms.UserText(text)}}
}

// echoTool returns the call's parameters verbatim as the result content. It's
// the simplest non-trivial executor and is the only tool name (echoToolName)
// used by these tests, which keeps the duplicate-name config test honest.
const echoToolName = "echo"

func echoTool() toolloop.Tool {
	return toolloop.Tool{
		Definition: llms.ToolDefinition{Name: echoToolName},
		Execute: func(_ context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
			return toolloop.ToolResult{Content: string(call.Parameters)}, nil
		},
	}
}

// scriptedResponses builds a FakeChatClient that returns responses in order
// and then repeats the last one (testllm semantics).
func scriptedResponses(resps ...llms.ChatResponse) *testllm.FakeChatClient {
	return &testllm.FakeChatClient{Responses: resps}
}

// toolCallResp produces an assistant tool-call response with one call. It
// mirrors testllm.ToolCallResponse but lets us set a custom Usage so we can
// test TotalUsage aggregation.
func toolCallResp(id, name, paramsJSON string, usage llms.Usage) llms.ChatResponse {
	r := testllm.ToolCallResponse(id, name, paramsJSON)
	r.Usage = usage
	return r
}

// finalTextResp produces a final-answer response with optional Usage.
func finalTextResp(text string, usage llms.Usage) llms.ChatResponse {
	r := testllm.TextResponse(text)
	r.Usage = usage
	return r
}

// outcomeReason returns the *toolloop.ConfigError reason or the raw error
// string, for substring assertions in config-error tests.
func outcomeReason(t *testing.T, out toolloop.Outcome) string {
	t.Helper()
	if out.Err == nil {
		t.Fatalf("expected an error, got nil; outcome kind=%q", out.Kind)
	}
	var ce *toolloop.ConfigError
	if errors.As(out.Err, &ce) {
		return ce.Reason
	}
	return out.Err.Error()
}

// happy-path scenarios ---------------------------------------------------

func TestRun_FinalAnswerOnFirstCall(t *testing.T) {
	client := scriptedResponses(finalTextResp("hi", llms.Usage{InputTokens: 3, OutputTokens: 1, ProviderRequestID: "req-1"}))
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  client,
		Request: userReq("hi"),
	})

	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("kind = %q, want final_answer; err=%v", out.Kind, out.Err)
	}
	if out.Iterations != 0 {
		t.Fatalf("Iterations = %d, want 0 (no tool calls)", out.Iterations)
	}
	if out.Response.Text != "hi" {
		t.Fatalf("Response.Text = %q, want %q", out.Response.Text, "hi")
	}
	if len(out.Messages) != 2 {
		t.Fatalf("Messages len = %d, want 2 (user + assistant)", len(out.Messages))
	}
	if out.TotalUsage.InputTokens != 3 || out.TotalUsage.OutputTokens != 1 {
		t.Fatalf("TotalUsage = %+v, want input=3 output=1", out.TotalUsage)
	}
	if out.TotalUsage.ProviderRequestID != "req-1" {
		t.Fatalf("TotalUsage.ProviderRequestID = %q, want %q (single contributor)", out.TotalUsage.ProviderRequestID, "req-1")
	}
	if out.Err != nil {
		t.Fatalf("Err = %v, want nil", out.Err)
	}
}

func TestRun_SingleToolRoundTrip(t *testing.T) {
	client := scriptedResponses(
		toolCallResp("c1", "echo", `{"x":1}`, llms.Usage{InputTokens: 5, OutputTokens: 2, ProviderRequestID: "req-1"}),
		finalTextResp("done", llms.Usage{InputTokens: 7, OutputTokens: 3, ProviderRequestID: "req-2"}),
	)
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  client,
		Request: userReq("call echo"),
		Tools:   []toolloop.Tool{echoTool()},
	})

	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("kind = %q, want final_answer; err=%v", out.Kind, out.Err)
	}
	if out.Iterations != 1 {
		t.Fatalf("Iterations = %d, want 1", out.Iterations)
	}
	// user, assistant tool-call, tool result, assistant final.
	if len(out.Messages) != 4 {
		t.Fatalf("Messages len = %d, want 4", len(out.Messages))
	}
	// Multi-contributor: ProviderRequestID collapses to empty.
	if out.TotalUsage.ProviderRequestID != "" {
		t.Fatalf("TotalUsage.ProviderRequestID = %q, want empty (multi-contributor)", out.TotalUsage.ProviderRequestID)
	}
	if out.TotalUsage.InputTokens != 12 || out.TotalUsage.OutputTokens != 5 {
		t.Fatalf("TotalUsage = %+v, want input=12 output=5", out.TotalUsage)
	}

	// The tool result message should carry the echoed parameters keyed to
	// the call ID.
	toolMsg := out.Messages[2]
	if toolMsg.Role != llms.RoleTool || len(toolMsg.Content) != 1 {
		t.Fatalf("tool message shape wrong: %+v", toolMsg)
	}
	tr := toolMsg.Content[0].ToolResult
	if tr == nil || tr.ToolCallID != "c1" || tr.Content != `{"x":1}` {
		t.Fatalf("tool result = %+v, want id=c1 content=%q", tr, `{"x":1}`)
	}
}

func TestRun_MultipleToolsInOneTurn_ResultsPreserveOrder(t *testing.T) {
	// Two parallel calls in one assistant turn; results must be appended
	// in the same order as resp.ToolCalls.
	tc1 := llms.ToolCall{ID: "a", Name: "echo", Parameters: []byte(`{"i":1}`)}
	tc2 := llms.ToolCall{ID: "b", Name: "echo", Parameters: []byte(`{"i":2}`)}
	parallelTurn := llms.ChatResponse{
		Message: llms.Message{
			Role: llms.RoleAssistant,
			Content: []llms.ContentPart{
				{Type: llms.ContentToolCall, ToolCall: &tc1},
				{Type: llms.ContentToolCall, ToolCall: &tc2},
			},
		},
		ToolCalls:  []llms.ToolCall{tc1, tc2},
		StopReason: "tool_use",
	}
	client := scriptedResponses(parallelTurn, finalTextResp("done", llms.Usage{}))
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  client,
		Request: userReq("call echo twice"),
		Tools:   []toolloop.Tool{echoTool()},
	})
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("kind = %q, want final_answer; err=%v", out.Kind, out.Err)
	}
	// user, assistant 2-tool-call, tool message w/ 2 results, assistant final
	if len(out.Messages) != 4 {
		t.Fatalf("Messages len = %d, want 4", len(out.Messages))
	}
	toolMsg := out.Messages[2]
	if len(toolMsg.Content) != 2 {
		t.Fatalf("tool message has %d results, want 2", len(toolMsg.Content))
	}
	if toolMsg.Content[0].ToolResult.ToolCallID != "a" || toolMsg.Content[1].ToolResult.ToolCallID != "b" {
		t.Fatalf("result order = [%s, %s], want [a, b]",
			toolMsg.Content[0].ToolResult.ToolCallID,
			toolMsg.Content[1].ToolResult.ToolCallID)
	}
}

// MaxIterations ----------------------------------------------------------

func TestRun_MaxIterations_PreExecuteStop(t *testing.T) {
	// Two tool-call responses in a row; with MaxIterations=2 we should
	// observe both, execute the first one's tools, and append the second
	// as unresolved diagnostic state without executing its tools.
	var executions int32
	tool := toolloop.Tool{
		Definition: llms.ToolDefinition{Name: "echo"},
		Execute: func(_ context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
			atomic.AddInt32(&executions, 1)
			return toolloop.ToolResult{Content: string(call.Parameters)}, nil
		},
	}
	client := scriptedResponses(
		toolCallResp("a", "echo", `{}`, llms.Usage{}),
		toolCallResp("b", "echo", `{}`, llms.Usage{}),
		// A third response that should NEVER be requested.
		finalTextResp("should-not-be-seen", llms.Usage{}),
	)
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:        client,
		Request:       userReq("loop"),
		Tools:         []toolloop.Tool{tool},
		MaxIterations: 2,
	})
	if out.Kind != toolloop.OutcomeMaxIterations {
		t.Fatalf("kind = %q, want max_iterations; err=%v", out.Kind, out.Err)
	}
	if out.Iterations != 2 {
		t.Fatalf("Iterations = %d, want 2 (both tool-call responses observed)", out.Iterations)
	}
	if got := atomic.LoadInt32(&executions); got != 1 {
		t.Fatalf("tool executed %d times, want 1 (second turn's tools must NOT execute)", got)
	}
	if got := len(client.Calls()); got != 2 {
		t.Fatalf("provider called %d times, want 2 (no extra Complete after limit)", got)
	}
	// Transcript should end with an unresolved assistant tool-call message
	// (no matching tool result for the second turn).
	if len(out.Messages) == 0 {
		t.Fatal("empty transcript")
	}
	last := out.Messages[len(out.Messages)-1]
	if last.Role != llms.RoleAssistant {
		t.Fatalf("last message role = %q, want assistant (unresolved tool-call turn)", last.Role)
	}
	hasUnresolvedCall := false
	for _, p := range last.Content {
		if p.Type == llms.ContentToolCall {
			hasUnresolvedCall = true
			break
		}
	}
	if !hasUnresolvedCall {
		t.Fatal("last message has no tool_call part; expected unresolved diagnostic state")
	}
	if out.Err != nil {
		t.Fatalf("Err = %v, want nil for MaxIterations", out.Err)
	}
}

func TestRun_MaxIterations_DefaultBound(t *testing.T) {
	// With the default bound (8) and the fake returning the same tool-call
	// response forever, Run must stop at iteration 8 and not run away.
	loop := toolCallResp("x", "echo", `{}`, llms.Usage{})
	client := scriptedResponses(loop) // FakeChatClient repeats the last response forever.
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  client,
		Request: userReq("loop forever"),
		Tools:   []toolloop.Tool{echoTool()},
	})
	if out.Kind != toolloop.OutcomeMaxIterations {
		t.Fatalf("kind = %q, want max_iterations", out.Kind)
	}
	if out.Iterations != 8 {
		t.Fatalf("Iterations = %d, want 8 (defaultMaxIterations)", out.Iterations)
	}
}

// Error paths ------------------------------------------------------------

func TestRun_ProviderError_OutcomeLLMError(t *testing.T) {
	wantErr := errors.New("boom")
	client := &testllm.FakeChatClient{Err: wantErr}
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  client,
		Request: userReq("hi"),
	})
	if out.Kind != toolloop.OutcomeLLMError {
		t.Fatalf("kind = %q, want llm_error", out.Kind)
	}
	if !errors.Is(out.Err, wantErr) {
		t.Fatalf("Err = %v, want wraps %v", out.Err, wantErr)
	}
	// No successful response was received.
	if out.Response.Text != "" || out.Response.Message.Role != "" {
		t.Fatalf("Response = %+v, want zero", out.Response)
	}
	if out.Iterations != 0 {
		t.Fatalf("Iterations = %d, want 0", out.Iterations)
	}
}

func TestRun_ProviderCancellation_OutcomeCanceled(t *testing.T) {
	// The fake wraps context.Canceled in its own error per testllm.chat.go;
	// errors.Is must still see it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := toolloop.Run(ctx, toolloop.Config{
		Client:  &testllm.FakeChatClient{},
		Request: userReq("hi"),
	})
	if out.Kind != toolloop.OutcomeCanceled {
		t.Fatalf("kind = %q, want canceled; err=%v", out.Kind, out.Err)
	}
	if !errors.Is(out.Err, context.Canceled) {
		t.Fatalf("Err = %v, want errors.Is(context.Canceled)", out.Err)
	}
}

func TestRun_ExecutorError_OutcomeToolError(t *testing.T) {
	wantErr := errors.New("disk full")
	failingTool := toolloop.Tool{
		Definition: llms.ToolDefinition{Name: "fail"},
		Execute: func(_ context.Context, _ llms.ToolCall) (toolloop.ToolResult, error) {
			return toolloop.ToolResult{}, wantErr
		},
	}
	client := scriptedResponses(
		toolCallResp("c1", "fail", `{}`, llms.Usage{}),
	)
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  client,
		Request: userReq("call fail"),
		Tools:   []toolloop.Tool{failingTool},
	})
	if out.Kind != toolloop.OutcomeToolError {
		t.Fatalf("kind = %q, want tool_error; err=%v", out.Kind, out.Err)
	}
	if !errors.Is(out.Err, wantErr) {
		t.Fatalf("Err = %v, want wraps %v", out.Err, wantErr)
	}
	// Response is the tool-calling turn that triggered the failing executor.
	if len(out.Response.ToolCalls) != 1 {
		t.Fatalf("Response.ToolCalls len = %d, want 1", len(out.Response.ToolCalls))
	}
	// Transcript should NOT contain a tool-result message (we aborted
	// before appending one).
	for i, m := range out.Messages {
		if m.Role == llms.RoleTool {
			t.Fatalf("Messages[%d] is a tool message; loop should have aborted before appending one", i)
		}
	}
}

func TestRun_ExecutorContextCanceled_OutcomeCanceled(t *testing.T) {
	// An executor that returns context.Canceled directly should map to
	// OutcomeCanceled, not OutcomeToolError, so callers don't confuse
	// caller-cancellation with a tool failure.
	cancelTool := toolloop.Tool{
		Definition: llms.ToolDefinition{Name: "cancel"},
		Execute: func(_ context.Context, _ llms.ToolCall) (toolloop.ToolResult, error) {
			return toolloop.ToolResult{}, context.Canceled
		},
	}
	client := scriptedResponses(
		toolCallResp("c1", "cancel", `{}`, llms.Usage{}),
	)
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  client,
		Request: userReq("call cancel"),
		Tools:   []toolloop.Tool{cancelTool},
	})
	if out.Kind != toolloop.OutcomeCanceled {
		t.Fatalf("kind = %q, want canceled; err=%v", out.Kind, out.Err)
	}
	if !errors.Is(out.Err, context.Canceled) {
		t.Fatalf("Err = %v, want errors.Is(context.Canceled)", out.Err)
	}
}

// Recovery semantics -----------------------------------------------------

func TestRun_UnknownToolRecovery_LoopContinues(t *testing.T) {
	// Model calls a tool that isn't registered; the loop should append an
	// IsError tool result with a recovery message and continue.
	client := scriptedResponses(
		toolCallResp("c1", "ghost", `{}`, llms.Usage{}),
		finalTextResp("recovered", llms.Usage{}),
	)
	var toolEvents []toolloop.ToolCallEvent
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:     client,
		Request:    userReq("call ghost"),
		Tools:      []toolloop.Tool{echoTool()}, // registered, but the model called "ghost"
		OnToolCall: func(e toolloop.ToolCallEvent) { toolEvents = append(toolEvents, e) },
	})
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("kind = %q, want final_answer (loop should have recovered); err=%v", out.Kind, out.Err)
	}
	if out.Response.Text != "recovered" {
		t.Fatalf("Response.Text = %q, want %q", out.Response.Text, "recovered")
	}
	if len(toolEvents) != 1 {
		t.Fatalf("OnToolCall fired %d times, want 1", len(toolEvents))
	}
	ev := toolEvents[0]
	if ev.Err != nil {
		t.Fatalf("ToolCallEvent.Err = %v, want nil (loop continued)", ev.Err)
	}
	if !ev.Result.IsError {
		t.Fatalf("ToolCallEvent.Result.IsError = false, want true (synthetic recovery result)")
	}
	if !strings.Contains(ev.Result.Content, `"ghost"`) {
		t.Fatalf("recovery content %q does not mention the tool name", ev.Result.Content)
	}
}

func TestRun_ModelVisibleToolFailure_LoopContinues(t *testing.T) {
	// ToolResult{IsError: true} is model-visible; the loop must continue
	// rather than aborting with OutcomeToolError.
	failingButRecoverable := toolloop.Tool{
		Definition: llms.ToolDefinition{Name: "fail"},
		Execute: func(_ context.Context, _ llms.ToolCall) (toolloop.ToolResult, error) {
			return toolloop.ToolResult{Content: "not found", IsError: true}, nil
		},
	}
	client := scriptedResponses(
		toolCallResp("c1", "fail", `{}`, llms.Usage{}),
		finalTextResp("ok", llms.Usage{}),
	)
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  client,
		Request: userReq("call fail"),
		Tools:   []toolloop.Tool{failingButRecoverable},
	})
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("kind = %q, want final_answer (IsError is model-visible, not loop-fatal)", out.Kind)
	}
	// The tool result with IsError should be in the transcript.
	found := false
	for _, m := range out.Messages {
		if m.Role != llms.RoleTool {
			continue
		}
		for _, p := range m.Content {
			if p.ToolResult != nil && p.ToolResult.IsError {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("transcript missing tool message with IsError result")
	}
}

// ProviderSignature round-trip ------------------------------------------

func TestRun_ToolCallProviderSignatureRoundTrip(t *testing.T) {
	// Verify the assistant message is appended verbatim so per-tool-call
	// provider metadata (ADR-0010 ProviderSignature) survives into the
	// next request the loop sends.
	tc := llms.ToolCall{
		ID:                "c1",
		Name:              "echo",
		Parameters:        []byte(`{}`),
		ProviderSignature: []byte{0xDE, 0xAD, 0xBE, 0xEF},
	}
	asstTurn := llms.ChatResponse{
		Message: llms.Message{
			Role:    llms.RoleAssistant,
			Content: []llms.ContentPart{{Type: llms.ContentToolCall, ToolCall: &tc}},
		},
		ToolCalls:  []llms.ToolCall{tc},
		StopReason: "tool_use",
	}
	client := scriptedResponses(asstTurn, finalTextResp("ok", llms.Usage{}))
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  client,
		Request: userReq("hi"),
		Tools:   []toolloop.Tool{echoTool()},
	})
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("kind = %q, want final_answer", out.Kind)
	}

	// On the second Complete call, the request's messages must contain the
	// assistant turn with the ProviderSignature preserved.
	calls := client.Calls()
	if len(calls) != 2 {
		t.Fatalf("provider call count = %d, want 2", len(calls))
	}
	second := calls[1]
	if len(second.Messages) < 2 {
		t.Fatalf("second request messages = %d, want >= 2", len(second.Messages))
	}
	asst := second.Messages[len(second.Messages)-2] // last is the tool-result message; assistant is the one before.
	if asst.Role != llms.RoleAssistant {
		t.Fatalf("expected assistant message at -2, got role %q", asst.Role)
	}
	if len(asst.Content) != 1 || asst.Content[0].ToolCall == nil {
		t.Fatalf("assistant content shape wrong: %+v", asst.Content)
	}
	got := asst.Content[0].ToolCall.ProviderSignature
	if !bytesEqual(got, tc.ProviderSignature) {
		t.Fatalf("ProviderSignature = %x, want %x", got, tc.ProviderSignature)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Events -----------------------------------------------------------------

func TestRun_Events_IndexAndCounts(t *testing.T) {
	client := scriptedResponses(
		toolCallResp("c1", "echo", `{}`, llms.Usage{}),
		toolCallResp("c2", "echo", `{}`, llms.Usage{}),
		finalTextResp("done", llms.Usage{}),
	)
	var iters []toolloop.IterationEvent
	var tcalls []toolloop.ToolCallEvent
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:      client,
		Request:     userReq("hi"),
		Tools:       []toolloop.Tool{echoTool()},
		OnIteration: func(e toolloop.IterationEvent) { iters = append(iters, e) },
		OnToolCall:  func(e toolloop.ToolCallEvent) { tcalls = append(tcalls, e) },
	})
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("kind = %q, want final_answer", out.Kind)
	}
	if len(iters) != 3 {
		t.Fatalf("IterationEvent count = %d, want 3", len(iters))
	}
	for i, ev := range iters {
		if ev.Index != i {
			t.Fatalf("IterationEvent[%d].Index = %d, want %d", i, ev.Index, i)
		}
	}
	if iters[0].NumToolCalls != 1 || iters[1].NumToolCalls != 1 || iters[2].NumToolCalls != 0 {
		t.Fatalf("NumToolCalls = [%d,%d,%d], want [1,1,0]",
			iters[0].NumToolCalls, iters[1].NumToolCalls, iters[2].NumToolCalls)
	}
	if len(tcalls) != 2 {
		t.Fatalf("ToolCallEvent count = %d, want 2", len(tcalls))
	}
	// Iteration on the ToolCallEvent should match the IterationEvent.Index
	// of the assistant turn that emitted the call.
	if tcalls[0].Iteration != 0 || tcalls[1].Iteration != 1 {
		t.Fatalf("ToolCallEvent.Iteration = [%d,%d], want [0,1]",
			tcalls[0].Iteration, tcalls[1].Iteration)
	}
}

// Config validation ------------------------------------------------------

func TestRun_ConfigErrors(t *testing.T) {
	baseTool := echoTool()
	baseReq := userReq("hi")
	cases := []struct {
		name string
		cfg  toolloop.Config
		want string
	}{
		{
			name: "nil client",
			cfg:  toolloop.Config{Request: baseReq, Tools: []toolloop.Tool{baseTool}},
			want: "Client is required",
		},
		{
			name: "empty messages",
			cfg: toolloop.Config{
				Client:  &testllm.FakeChatClient{},
				Request: llms.ChatRequest{},
				Tools:   []toolloop.Tool{baseTool},
			},
			want: "Messages must contain at least one message",
		},
		{
			name: "Request.Tools set",
			cfg: toolloop.Config{
				Client: &testllm.FakeChatClient{},
				Request: llms.ChatRequest{
					Messages: []llms.Message{llms.UserText("hi")},
					Tools:    []llms.ToolDefinition{{Name: "echo"}},
				},
				Tools: []toolloop.Tool{baseTool},
			},
			want: "Request.Tools must be empty",
		},
		{
			name: "duplicate tool name",
			cfg: toolloop.Config{
				Client:  &testllm.FakeChatClient{},
				Request: baseReq,
				Tools:   []toolloop.Tool{baseTool, echoTool()},
			},
			want: "duplicate tool name",
		},
		{
			name: "nil Execute",
			cfg: toolloop.Config{
				Client:  &testllm.FakeChatClient{},
				Request: baseReq,
				Tools:   []toolloop.Tool{{Definition: llms.ToolDefinition{Name: "x"}}},
			},
			want: "Execute is nil",
		},
		{
			name: "empty tool name",
			cfg: toolloop.Config{
				Client:  &testllm.FakeChatClient{},
				Request: baseReq,
				Tools:   []toolloop.Tool{{Execute: baseTool.Execute}},
			},
			want: "Definition.Name is empty",
		},
		{
			name: "both ToolChoices set",
			cfg: toolloop.Config{
				Client: &testllm.FakeChatClient{},
				Request: llms.ChatRequest{
					Messages:   []llms.Message{llms.UserText("hi")},
					ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceAuto},
				},
				Tools:      []toolloop.Tool{baseTool},
				ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceRequired},
			},
			want: "exactly one",
		},
		{
			name: "Required with no tools",
			cfg: toolloop.Config{
				Client:     &testllm.FakeChatClient{},
				Request:    baseReq,
				ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceRequired},
			},
			want: `requires at least one`,
		},
		{
			name: "ToolChoiceTool names unknown tool",
			cfg: toolloop.Config{
				Client:     &testllm.FakeChatClient{},
				Request:    baseReq,
				Tools:      []toolloop.Tool{baseTool},
				ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceTool, Name: "missing"},
			},
			want: `not in Config.Tools`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := toolloop.Run(context.Background(), tc.cfg)
			if out.Kind != toolloop.OutcomeToolError {
				t.Fatalf("kind = %q, want tool_error", out.Kind)
			}
			reason := outcomeReason(t, out)
			if !strings.Contains(reason, tc.want) {
				t.Fatalf("reason %q does not contain %q", reason, tc.want)
			}
			var ce *toolloop.ConfigError
			if !errors.As(out.Err, &ce) {
				t.Fatalf("Err = %v, want *ConfigError", out.Err)
			}
		})
	}
}

func TestRun_EmptyTools_AutoChoice_BehavesLikeSingleComplete(t *testing.T) {
	// Edge case I flagged in the proposal review: empty Tools + Auto must
	// be allowed and behave as a single Complete call.
	client := scriptedResponses(finalTextResp("ok", llms.Usage{}))
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  client,
		Request: userReq("hi"),
		// Tools deliberately omitted.
	})
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("kind = %q, want final_answer", out.Kind)
	}
	if got := len(client.Calls()); got != 1 {
		t.Fatalf("provider call count = %d, want 1", got)
	}
	// The provider request should carry zero tools.
	if got := len(client.Calls()[0].Tools); got != 0 {
		t.Fatalf("provider request Tools len = %d, want 0", got)
	}
}

// ToolChoice precedence --------------------------------------------------

func TestRun_ToolChoice_ConfigOverridesRequest(t *testing.T) {
	// Config.ToolChoice non-zero, Request.ToolChoice zero — Config wins.
	client := scriptedResponses(finalTextResp("ok", llms.Usage{}))
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:     client,
		Request:    userReq("hi"),
		Tools:      []toolloop.Tool{echoTool()},
		ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceRequired},
	})
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("kind = %q, want final_answer", out.Kind)
	}
	got := client.Calls()[0].ToolChoice.Type
	if got != llms.ToolChoiceRequired {
		t.Fatalf("ToolChoice = %q, want required", got)
	}
}

func TestRun_ToolChoice_RequestUsedWhenConfigZero(t *testing.T) {
	// Request.ToolChoice non-zero, Config.ToolChoice zero — Request flows through.
	client := scriptedResponses(finalTextResp("ok", llms.Usage{}))
	req := userReq("hi")
	req.ToolChoice = llms.ToolChoice{Type: llms.ToolChoiceNone}
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  client,
		Request: req,
		Tools:   []toolloop.Tool{echoTool()},
	})
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("kind = %q, want final_answer", out.Kind)
	}
	if got := client.Calls()[0].ToolChoice.Type; got != llms.ToolChoiceNone {
		t.Fatalf("ToolChoice = %q, want none", got)
	}
}

func TestRun_ToolChoice_BothZero_DefaultsToAuto(t *testing.T) {
	client := scriptedResponses(finalTextResp("ok", llms.Usage{}))
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  client,
		Request: userReq("hi"),
		Tools:   []toolloop.Tool{echoTool()},
	})
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("kind = %q, want final_answer", out.Kind)
	}
	if got := client.Calls()[0].ToolChoice.Type; got != llms.ToolChoiceAuto {
		t.Fatalf("ToolChoice = %q, want auto", got)
	}
}

// Caller-state safety ----------------------------------------------------

func TestRun_DoesNotMutateCallerMessages(t *testing.T) {
	// The loop must copy Request.Messages before appending, so the caller's
	// slice is unchanged on return.
	client := scriptedResponses(
		toolCallResp("c1", "echo", `{}`, llms.Usage{}),
		finalTextResp("ok", llms.Usage{}),
	)
	original := []llms.Message{llms.UserText("hi")}
	req := llms.ChatRequest{Messages: original}
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  client,
		Request: req,
		Tools:   []toolloop.Tool{echoTool()},
	})
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("kind = %q", out.Kind)
	}
	if len(original) != 1 {
		t.Fatalf("caller's Messages mutated: len = %d, want 1", len(original))
	}
	if len(req.Messages) != 1 {
		t.Fatalf("caller's Request.Messages mutated: len = %d, want 1", len(req.Messages))
	}
}

// Realistic-shape doc example -------------------------------------------

func TestRun_DocExampleShape(t *testing.T) {
	// Roughly the example in docs/toolloop-proposal.md, exercised against
	// the fake to make sure the documented call site compiles and works.
	weather := toolloop.Tool{
		Definition: llms.ToolDefinition{
			Name:        "get_weather",
			Description: "Get current weather for a city.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		},
		Execute: func(_ context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
			// The doc example shows returning ToolResult{IsError: true}
			// on malformed JSON; that pattern is exercised by
			// TestRun_ModelVisibleToolFailure_LoopContinues. Here we
			// only assert the documented call shape compiles and works,
			// so the test feeds valid JSON and we propagate any
			// unmarshal error as a loop-visible failure.
			var args struct {
				City string `json:"city"`
			}
			if err := json.Unmarshal(call.Parameters, &args); err != nil {
				return toolloop.ToolResult{}, fmt.Errorf("doc example: %w", err)
			}
			return toolloop.ToolResult{Content: fmt.Sprintf(`{"city":%q,"temp_c":18}`, args.City)}, nil
		},
	}
	client := scriptedResponses(
		toolCallResp("c1", "get_weather", `{"city":"Paris"}`, llms.Usage{}),
		finalTextResp("It is 18C in Paris.", llms.Usage{}),
	)
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:     client,
		Request:    llms.ChatRequest{Messages: []llms.Message{llms.UserText("Weather in Paris?")}, MaxTokens: 512},
		Tools:      []toolloop.Tool{weather},
		ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceAuto},
	})
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("kind = %q, want final_answer; err=%v", out.Kind, out.Err)
	}
	if !strings.Contains(out.Response.Text, "Paris") {
		t.Fatalf("final answer %q does not mention Paris", out.Response.Text)
	}
}
