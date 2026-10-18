package toolloop_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

// ADR-0026: OutcomeContextExceeded.

func overflowError() error {
	return &llms.ProviderError{
		Provider: "fake", Model: "fake", StatusCode: 400,
		Kind: llms.ErrorKindContextLength, Message: "prompt is too long",
	}
}

// overflowAfter returns the scripted responses in order, then fails every
// further call with a context-length error. It counts calls.
func overflowAfter(calls *atomic.Int32, resps ...llms.ChatResponse) *testllm.FakeChatClient {
	return &testllm.FakeChatClient{
		Func: func(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
			if i := int(calls.Add(1)) - 1; i < len(resps) {
				return resps[i], nil
			}
			return llms.ChatResponse{}, overflowError()
		},
	}
}

func TestRun_ContextLength_OutcomeContextExceeded(t *testing.T) {
	var calls atomic.Int32
	first := toolCallResp("c1", echoToolName, `{"x":1}`, llms.Usage{InputTokens: 5})
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  overflowAfter(&calls, first),
		Request: userReq("work"),
		Tools:   []toolloop.Tool{echoTool()},
	})
	if out.Kind != toolloop.OutcomeContextExceeded {
		t.Fatalf("kind = %q, want context_exceeded; err=%v", out.Kind, out.Err)
	}
	var pe *llms.ProviderError
	if !errors.As(out.Err, &pe) || pe.Kind != llms.ErrorKindContextLength {
		t.Fatalf("Err = %v, want the context_length *llms.ProviderError", out.Err)
	}
	// Messages is the transcript that overflowed: user, assistant tool
	// call, tool result. Response is the last successful one.
	if len(out.Messages) != 3 || out.Messages[2].Role != llms.RoleTool {
		t.Fatalf("Messages = %+v, want user/assistant/tool", out.Messages)
	}
	if out.Response.Message.Role != llms.RoleAssistant || out.Iterations != 1 {
		t.Fatalf("Response = %+v, Iterations = %d; want the tool-call response, 1", out.Response, out.Iterations)
	}
	if out.TotalUsage.InputTokens != 5 {
		t.Fatalf("TotalUsage.InputTokens = %d, want 5", out.TotalUsage.InputTokens)
	}

	// The transcript is well-formed: after compaction (here: none) it can be
	// fed back in as Request.Messages and the run continues.
	resumed := toolloop.Run(context.Background(), toolloop.Config{
		Client:  scriptedResponses(testllm.TextResponse("done")),
		Request: llms.ChatRequest{Messages: out.Messages},
		Tools:   []toolloop.Tool{echoTool()},
	})
	if resumed.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("resumed kind = %q, want final_answer; err=%v", resumed.Kind, resumed.Err)
	}
}

func TestRun_ContextLength_NotRetried(t *testing.T) {
	// context_length is non-retryable, so the Retry middleware passes it
	// straight through after one attempt.
	var calls atomic.Int32
	cfg := middleware.DefaultRetryConfig()
	cfg.InitialDelay = time.Millisecond
	client := middleware.RetryChat(cfg)(overflowAfter(&calls))
	out := toolloop.Run(context.Background(), toolloop.Config{Client: client, Request: userReq("hi")})
	if out.Kind != toolloop.OutcomeContextExceeded {
		t.Fatalf("kind = %q, want context_exceeded; err=%v", out.Kind, out.Err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("provider calls = %d, want 1", n)
	}
}

func TestRun_ContextLength_LoopContextDoneWins(t *testing.T) {
	// The loop's own context stays authoritative (ADR-0019): an overflow
	// reported after the caller canceled is OutcomeCanceled.
	ctx, cancel := context.WithCancel(context.Background())
	client := &testllm.FakeChatClient{
		Func: func(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
			cancel()
			return llms.ChatResponse{}, overflowError()
		},
	}
	out := toolloop.Run(ctx, toolloop.Config{Client: client, Request: userReq("hi")})
	if out.Kind != toolloop.OutcomeCanceled {
		t.Fatalf("kind = %q, want canceled; err=%v", out.Kind, out.Err)
	}
}

func TestRun_ContextLengthFromExecutor_StaysToolError(t *testing.T) {
	// A tool whose own model call overflows is a tool failure, not this
	// loop's transcript overflowing.
	tool := toolloop.Tool{
		Definition: llms.ToolDefinition{Name: "subagent"},
		Execute: func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) {
			return toolloop.ToolResult{}, overflowError()
		},
	}
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  scriptedResponses(toolCallResp("c1", "subagent", `{}`, llms.Usage{})),
		Request: userReq("work"),
		Tools:   []toolloop.Tool{tool},
	})
	if out.Kind != toolloop.OutcomeToolError {
		t.Fatalf("kind = %q, want tool_error; err=%v", out.Kind, out.Err)
	}
}

func TestRun_BadRequest_StaysLLMError(t *testing.T) {
	client := &testllm.FakeChatClient{Err: &llms.ProviderError{Kind: llms.ErrorKindBadRequest, StatusCode: 400}}
	out := toolloop.Run(context.Background(), toolloop.Config{Client: client, Request: userReq("hi")})
	if out.Kind != toolloop.OutcomeLLMError {
		t.Fatalf("kind = %q, want llm_error; err=%v", out.Kind, out.Err)
	}
}
