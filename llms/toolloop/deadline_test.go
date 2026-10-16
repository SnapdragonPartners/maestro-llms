package toolloop_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

// ADR-0019: OutcomeDeadline and NoIterationLimit.

// shortDeadline is long enough that the first scripted provider call
// completes before it, and short enough to keep the tests fast. Every
// deadline test blocks on ctx.Done() rather than racing a timer, so the
// outcome does not depend on scheduling.
const shortDeadline = 50 * time.Millisecond

// timeoutProviderError mirrors what apierr.Classify returns for a tripped
// deadline: a retryable timeout *llms.ProviderError wrapping ctx.Err().
func timeoutProviderError(cause error) error {
	return &llms.ProviderError{
		Provider: "fake", Model: "fake",
		Kind: llms.ErrorKindTimeout, Message: "request deadline exceeded", Cause: cause,
	}
}

// blockingClient returns the scripted responses in order, then blocks
// until ctx is done and returns a provider timeout error wrapping ctx.Err().
func blockingClient(resps ...llms.ChatResponse) *testllm.FakeChatClient {
	var n atomic.Int32
	return &testllm.FakeChatClient{
		Func: func(ctx context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
			if i := int(n.Add(1)) - 1; i < len(resps) {
				return resps[i], nil
			}
			<-ctx.Done()
			return llms.ChatResponse{}, timeoutProviderError(ctx.Err())
		},
	}
}

func assertDeadline(t *testing.T, out toolloop.Outcome) {
	t.Helper()
	if out.Kind != toolloop.OutcomeDeadline {
		t.Fatalf("kind = %q, want deadline; err=%v", out.Kind, out.Err)
	}
	if !errors.Is(out.Err, context.DeadlineExceeded) {
		t.Fatalf("Err = %v, want errors.Is(context.DeadlineExceeded)", out.Err)
	}
}

func TestRun_DeadlineDuringProviderCall_OutcomeDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), shortDeadline)
	defer cancel()
	client := blockingClient(toolCallResp("c1", echoToolName, `{}`, llms.Usage{InputTokens: 7}))
	out := toolloop.Run(ctx, toolloop.Config{
		Client:  client,
		Request: userReq("work"),
		Tools:   []toolloop.Tool{echoTool()},
	})
	assertDeadline(t, out)
	// The provider's own error is preserved, not replaced.
	var pe *llms.ProviderError
	if !errors.As(out.Err, &pe) || pe.Kind != llms.ErrorKindTimeout {
		t.Fatalf("Err = %v, want the provider's timeout *llms.ProviderError preserved", out.Err)
	}
	// Partial state is carried as for the other non-final outcomes.
	if out.Iterations != 1 {
		t.Fatalf("Iterations = %d, want 1", out.Iterations)
	}
	if out.TotalUsage.InputTokens != 7 {
		t.Fatalf("TotalUsage.InputTokens = %d, want 7", out.TotalUsage.InputTokens)
	}
	// user, asst(c1), tool(c1)
	if len(out.Messages) != 3 || out.Messages[2].Role != llms.RoleTool {
		t.Fatalf("Messages = %+v, want user/assistant/tool", out.Messages)
	}
	if len(out.Response.ToolCalls) != 1 {
		t.Fatalf("Response = %+v, want the last successful (tool-calling) response", out.Response)
	}
}

func TestRun_DeadlineDuringExecutor_OutcomeDeadline(t *testing.T) {
	cases := []struct {
		ret  func(ctx context.Context) error
		name string
	}{
		// The executor honors ctx and returns its error.
		{name: "returns ctx.Err", ret: func(ctx context.Context) error { return ctx.Err() }},
		// The executor fails with an unrelated error after the deadline;
		// the loop's context still decides, and both causes survive.
		{name: "returns unrelated error", ret: func(context.Context) error { return errUnrelated }},
		// An error wrapping context.Canceled (e.g. a child context the
		// executor derived) still yields Deadline: the loop's ctx wins.
		{name: "returns canceled", ret: func(context.Context) error { return fmt.Errorf("child: %w", context.Canceled) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), shortDeadline)
			defer cancel()
			slow := toolloop.Tool{
				Definition: llms.ToolDefinition{Name: "slow"},
				Execute: func(ctx context.Context, _ llms.ToolCall) (toolloop.ToolResult, error) {
					<-ctx.Done()
					return toolloop.ToolResult{}, tc.ret(ctx)
				},
			}
			out := toolloop.Run(ctx, toolloop.Config{
				Client:  scriptedResponses(toolCallResp("c1", "slow", `{}`, llms.Usage{})),
				Request: userReq("work"),
				Tools:   []toolloop.Tool{slow},
			})
			assertDeadline(t, out)
			if tc.name == "returns unrelated error" && !errors.Is(out.Err, errUnrelated) {
				t.Fatalf("Err = %v, want the executor's error preserved", out.Err)
			}
			if out.Iterations != 1 || len(out.Response.ToolCalls) != 1 {
				t.Fatalf("Iterations = %d, Response = %+v; want the tool-calling turn", out.Iterations, out.Response)
			}
		})
	}
}

var errUnrelated = errors.New("unrelated io failure")

func TestRun_DeadlineAlreadyPassed_OutcomeDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	out := toolloop.Run(ctx, toolloop.Config{
		Client:  &testllm.FakeChatClient{},
		Request: userReq("hi"),
	})
	assertDeadline(t, out)
	if out.Iterations != 0 {
		t.Fatalf("Iterations = %d, want 0", out.Iterations)
	}
}

func TestRun_ExplicitCancelWithDeadline_OutcomeCanceled(t *testing.T) {
	// A context that has a deadline but is canceled before it is reached
	// is a cancellation, not a deadline.
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	tool := toolloop.Tool{
		Definition: llms.ToolDefinition{Name: "cancels"},
		Execute: func(ctx context.Context, _ llms.ToolCall) (toolloop.ToolResult, error) {
			cancel()
			return toolloop.ToolResult{}, ctx.Err()
		},
	}
	out := toolloop.Run(ctx, toolloop.Config{
		Client:  scriptedResponses(toolCallResp("c1", "cancels", `{}`, llms.Usage{})),
		Request: userReq("work"),
		Tools:   []toolloop.Tool{tool},
	})
	if out.Kind != toolloop.OutcomeCanceled {
		t.Fatalf("kind = %q, want canceled; err=%v", out.Kind, out.Err)
	}
	if !errors.Is(out.Err, context.Canceled) {
		t.Fatalf("Err = %v, want errors.Is(context.Canceled)", out.Err)
	}
}

func TestRun_CanceledCtxUnrelatedError_ErrMatchesCanceled(t *testing.T) {
	// The loop's ctx is canceled but the provider returns an error that
	// does not wrap it: the outcome is Canceled and Err still matches
	// context.Canceled, with the provider's error preserved.
	ctx, cancel := context.WithCancel(context.Background())
	client := &testllm.FakeChatClient{
		Func: func(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
			cancel()
			return llms.ChatResponse{}, errUnrelated
		},
	}
	out := toolloop.Run(ctx, toolloop.Config{Client: client, Request: userReq("hi")})
	if out.Kind != toolloop.OutcomeCanceled {
		t.Fatalf("kind = %q, want canceled; err=%v", out.Kind, out.Err)
	}
	if !errors.Is(out.Err, context.Canceled) || !errors.Is(out.Err, errUnrelated) {
		t.Fatalf("Err = %v, want both context.Canceled and the provider error", out.Err)
	}
}

func TestRun_PerAttemptTimeoutMiddleware_StaysLLMError(t *testing.T) {
	// The Timeout middleware's per-attempt deadline expires but the
	// loop's own context is live: that is a provider timeout, not the
	// loop's deadline.
	inner := &testllm.FakeChatClient{
		Func: func(ctx context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
			<-ctx.Done()
			return llms.ChatResponse{}, timeoutProviderError(ctx.Err())
		},
	}
	client := middleware.TimeoutChat(10 * time.Millisecond)(inner)
	out := toolloop.Run(context.Background(), toolloop.Config{Client: client, Request: userReq("hi")})
	if out.Kind != toolloop.OutcomeLLMError {
		t.Fatalf("kind = %q, want llm_error; err=%v", out.Kind, out.Err)
	}
	var pe *llms.ProviderError
	if !errors.As(out.Err, &pe) || pe.Kind != llms.ErrorKindTimeout {
		t.Fatalf("Err = %v, want a timeout *llms.ProviderError", out.Err)
	}
}

func TestRun_ExecutorSubDeadline_StaysToolError(t *testing.T) {
	// An executor's own sub-deadline leaves the loop's context live, so
	// its DeadlineExceeded is a tool failure of the executor's choosing.
	tool := toolloop.Tool{
		Definition: llms.ToolDefinition{Name: "sub"},
		Execute: func(ctx context.Context, _ llms.ToolCall) (toolloop.ToolResult, error) {
			sub, cancel := context.WithTimeout(ctx, time.Millisecond)
			defer cancel()
			<-sub.Done()
			return toolloop.ToolResult{}, sub.Err()
		},
	}
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  scriptedResponses(toolCallResp("c1", "sub", `{}`, llms.Usage{})),
		Request: userReq("work"),
		Tools:   []toolloop.Tool{tool},
	})
	if out.Kind != toolloop.OutcomeToolError {
		t.Fatalf("kind = %q, want tool_error; err=%v", out.Kind, out.Err)
	}
	if !errors.Is(out.Err, context.DeadlineExceeded) {
		t.Fatalf("Err = %v, want the executor's DeadlineExceeded", out.Err)
	}
}

// NoIterationLimit -----------------------------------------------------------

// toolTurns returns n single-call echo turns followed by a final answer.
func toolTurns(n int) []llms.ChatResponse {
	resps := make([]llms.ChatResponse, 0, n+1)
	for i := range n {
		resps = append(resps, toolCallResp(fmt.Sprintf("c%d", i), echoToolName, `{}`, llms.Usage{}))
	}
	return append(resps, finalTextResp("done", llms.Usage{}))
}

func TestRun_NoIterationLimit_RunsPastDefaultToFinalAnswer(t *testing.T) {
	for _, limit := range []int{toolloop.NoIterationLimit, -5} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			const turns = 20
			client := scriptedResponses(toolTurns(turns)...)
			out := toolloop.Run(context.Background(), toolloop.Config{
				Client:        client,
				Request:       userReq("loop"),
				Tools:         []toolloop.Tool{echoTool()},
				MaxIterations: limit,
			})
			if out.Kind != toolloop.OutcomeFinalAnswer {
				t.Fatalf("kind = %q, want final_answer; err=%v", out.Kind, out.Err)
			}
			if out.Iterations != turns {
				t.Fatalf("Iterations = %d, want %d", out.Iterations, turns)
			}
			if got := len(client.Calls()); got != turns+1 {
				t.Fatalf("provider called %d times, want %d", got, turns+1)
			}
		})
	}
}

func TestRun_NoIterationLimit_StopsOnTerminalTool(t *testing.T) {
	const stopAt = 12
	var n atomic.Int32
	tool := toolloop.Tool{
		Definition: llms.ToolDefinition{Name: echoToolName},
		Execute: func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) {
			return toolloop.ToolResult{Content: "ok", Stop: n.Add(1) == stopAt}, nil
		},
	}
	// The fake repeats the tool-call turn forever; only Stop ends the run.
	client := scriptedResponses(toolCallResp("x", echoToolName, `{}`, llms.Usage{}))
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:        client,
		Request:       userReq("loop"),
		Tools:         []toolloop.Tool{tool},
		MaxIterations: toolloop.NoIterationLimit,
	})
	if out.Kind != toolloop.OutcomeTerminalTool {
		t.Fatalf("kind = %q, want terminal_tool; err=%v", out.Kind, out.Err)
	}
	if out.Iterations != stopAt {
		t.Fatalf("Iterations = %d, want %d", out.Iterations, stopAt)
	}
}

func TestRun_NoIterationLimit_StopsOnDeadline(t *testing.T) {
	// An unlimited run bounded only by wall-clock time ends in
	// OutcomeDeadline, the pattern #73 needed a huge MaxIterations for.
	ctx, cancel := context.WithTimeout(context.Background(), shortDeadline)
	defer cancel()
	client := &testllm.FakeChatClient{
		Func: func(ctx context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
			if err := ctx.Err(); err != nil {
				return llms.ChatResponse{}, timeoutProviderError(err)
			}
			return toolCallResp("x", echoToolName, `{}`, llms.Usage{}), nil
		},
	}
	// The first 10 turns execute immediately; the 11th blocks until the
	// deadline, so the run passes the default ceiling deterministically.
	const fastTurns = 10
	var n atomic.Int32
	echoThenBlock := toolloop.Tool{
		Definition: llms.ToolDefinition{Name: echoToolName},
		Execute: func(ctx context.Context, _ llms.ToolCall) (toolloop.ToolResult, error) {
			if n.Add(1) > fastTurns {
				<-ctx.Done()
				return toolloop.ToolResult{}, ctx.Err()
			}
			return toolloop.ToolResult{Content: "ok"}, nil
		},
	}
	out := toolloop.Run(ctx, toolloop.Config{
		Client:        client,
		Request:       userReq("loop"),
		Tools:         []toolloop.Tool{echoThenBlock},
		MaxIterations: toolloop.NoIterationLimit,
	})
	assertDeadline(t, out)
	if out.Iterations != fastTurns+1 {
		t.Fatalf("Iterations = %d, want %d (no default ceiling)", out.Iterations, fastTurns+1)
	}
}

func TestRun_NoIterationLimit_BoundedPrealloc(t *testing.T) {
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:        scriptedResponses(finalTextResp("done", llms.Usage{})),
		Request:       userReq("hi"),
		MaxIterations: toolloop.NoIterationLimit,
	})
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("kind = %q, want final_answer; err=%v", out.Kind, out.Err)
	}
	if c := cap(out.Messages); c > 1+2*16 {
		t.Fatalf("cap(Messages) = %d, want <= %d", c, 1+2*16)
	}
}
