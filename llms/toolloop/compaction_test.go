package toolloop_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

// ADR-0030: the Compact hook.

// recordingCompactor records every CompactInput (with a private copy of
// Messages) and answers with fn.
type recordingCompactor struct {
	fn     func(in toolloop.CompactInput) (toolloop.CompactResult, error)
	inputs []toolloop.CompactInput
}

func (r *recordingCompactor) compact(_ context.Context, in toolloop.CompactInput) (toolloop.CompactResult, error) {
	in.Messages = append([]llms.Message(nil), in.Messages...)
	r.inputs = append(r.inputs, in)
	if r.fn == nil {
		return toolloop.CompactResult{}, nil
	}
	return r.fn(in)
}

func (r *recordingCompactor) reasons() []toolloop.CompactReason {
	out := make([]toolloop.CompactReason, len(r.inputs))
	for i := range r.inputs {
		out[i] = r.inputs[i].Reason
	}
	return out
}

// firstOnly compacts to the transcript's first message.
func firstOnly(in toolloop.CompactInput) (toolloop.CompactResult, error) {
	return toolloop.CompactResult{Messages: in.Messages[:1]}, nil
}

// sequence returns scripted results in order: each element is a response or
// an error.
func sequence(calls *atomic.Int32, steps ...any) *testllm.FakeChatClient {
	return &testllm.FakeChatClient{
		Func: func(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
			i := int(calls.Add(1)) - 1
			if i >= len(steps) {
				i = len(steps) - 1
			}
			switch s := steps[i].(type) {
			case llms.ChatResponse:
				return s, nil
			case error:
				return llms.ChatResponse{}, s
			}
			panic("bad step")
		},
	}
}

func equalReasons(t *testing.T, got []toolloop.CompactReason, want ...toolloop.CompactReason) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("compactor reasons = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("compactor reasons = %v, want %v", got, want)
		}
	}
}

func TestCompact_BeforeTurnSeesLastUsage_NilIsNoChange(t *testing.T) {
	rc := &recordingCompactor{}
	client := scriptedResponses(
		toolCallResp("c1", echoToolName, `{}`, llms.Usage{InputTokens: 10, OutputTokens: 2}),
		finalTextResp("done", llms.Usage{InputTokens: 20, OutputTokens: 3}),
	)
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client: client, Request: userReq("go"), Tools: []toolloop.Tool{echoTool()}, Compact: rc.compact,
	})
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("kind = %q, err=%v", out.Kind, out.Err)
	}
	equalReasons(t, rc.reasons(), toolloop.CompactBeforeTurn, toolloop.CompactBeforeTurn)
	if u := rc.inputs[0].LastUsage; u != (llms.Usage{}) {
		t.Errorf("first LastUsage = %+v, want zero", u)
	}
	if u := rc.inputs[1].LastUsage; u.InputTokens != 10 || u.OutputTokens != 2 {
		t.Errorf("second LastUsage = %+v, want the first response's usage", u)
	}
	if len(rc.inputs[1].Messages) != 3 || rc.inputs[0].Err != nil {
		t.Errorf("second input = %+v, want the 3-message transcript and nil Err", rc.inputs[1])
	}
	if out.Compactions != 0 || len(out.Messages) != 4 {
		t.Errorf("Compactions = %d, Messages = %d; want 0 and the full transcript", out.Compactions, len(out.Messages))
	}
}

func TestCompact_BeforeTurnAdopted(t *testing.T) {
	rc := &recordingCompactor{fn: func(in toolloop.CompactInput) (toolloop.CompactResult, error) {
		if len(in.Messages) < 3 {
			return toolloop.CompactResult{}, nil
		}
		return firstOnly(in)
	}}
	client := scriptedResponses(
		toolCallResp("c1", echoToolName, `{}`, llms.Usage{}),
		testllm.TextResponse("done"),
	)
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client: client, Request: userReq("go"), Tools: []toolloop.Tool{echoTool()}, Compact: rc.compact,
	})
	if out.Kind != toolloop.OutcomeFinalAnswer || out.Compactions != 1 {
		t.Fatalf("kind = %q, Compactions = %d; want final_answer, 1 (err=%v)", out.Kind, out.Compactions, out.Err)
	}
	calls := client.Calls()
	if len(calls) != 2 || len(calls[1].Messages) != 1 {
		t.Fatalf("second request has %d messages, want the compacted 1", len(calls[1].Messages))
	}
	// Messages continues from the compacted transcript.
	if len(out.Messages) != 2 || out.Messages[1].Role != llms.RoleAssistant {
		t.Fatalf("Messages = %+v, want user + final answer", out.Messages)
	}
	if out.Iterations != 1 {
		t.Errorf("Iterations = %d, want 1 (compaction does not reset it)", out.Iterations)
	}
}

func TestCompact_ResultIsCopied(t *testing.T) {
	// A hook that returns a sub-slice of its input must not have the loop's
	// later appends write through into memory it handed out.
	var handed []llms.Message
	// Fingerprint by tool-call ID: an aliased append would overwrite the
	// handed-out messages with ones of the same roles but later IDs.
	snapshot := func() []string {
		ids := make([]string, len(handed))
		for i := range handed {
			for _, p := range handed[i].Content {
				switch {
				case p.ToolCall != nil:
					ids[i] = p.ToolCall.ID
				case p.ToolResult != nil:
					ids[i] = p.ToolResult.ToolCallID
				}
			}
		}
		return ids
	}
	client := scriptedResponses(
		toolCallResp("c1", echoToolName, `{}`, llms.Usage{}),
		toolCallResp("c2", echoToolName, `{}`, llms.Usage{}),
		testllm.TextResponse("done"),
	)
	var before []string
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client: client, Request: userReq("go"), Tools: []toolloop.Tool{echoTool()},
		Compact: func(_ context.Context, in toolloop.CompactInput) (toolloop.CompactResult, error) {
			if len(in.Messages) < 3 || handed != nil {
				return toolloop.CompactResult{}, nil
			}
			handed = in.Messages
			before = snapshot()
			return toolloop.CompactResult{Messages: in.Messages[:1]}, nil
		},
	})
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("kind = %q, err=%v", out.Kind, out.Err)
	}
	after := snapshot()
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("handed-out transcript changed: %v -> %v", before, after)
		}
	}
}

func TestCompact_OverflowRetriedOnce_Succeeds(t *testing.T) {
	var calls atomic.Int32
	rc := &recordingCompactor{fn: func(in toolloop.CompactInput) (toolloop.CompactResult, error) {
		if in.Reason != toolloop.CompactOverflow {
			return toolloop.CompactResult{}, nil
		}
		return firstOnly(in)
	}}
	client := sequence(&calls,
		toolCallResp("c1", echoToolName, `{}`, llms.Usage{InputTokens: 7}),
		overflowError(),
		testllm.TextResponse("done"),
	)
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client: client, Request: userReq("go"), Tools: []toolloop.Tool{echoTool()}, Compact: rc.compact,
	})
	if out.Kind != toolloop.OutcomeFinalAnswer || out.Compactions != 1 {
		t.Fatalf("kind = %q, Compactions = %d; want final_answer, 1 (err=%v)", out.Kind, out.Compactions, out.Err)
	}
	// No before-turn call precedes the retry.
	equalReasons(t, rc.reasons(), toolloop.CompactBeforeTurn, toolloop.CompactBeforeTurn, toolloop.CompactOverflow)
	ov := rc.inputs[2]
	var pe *llms.ProviderError
	if !errors.As(ov.Err, &pe) || pe.Kind != llms.ErrorKindContextLength {
		t.Errorf("overflow input Err = %v, want the context_length error", ov.Err)
	}
	if len(ov.Messages) != 3 || ov.LastUsage.InputTokens != 7 {
		t.Errorf("overflow input = %d messages, LastUsage %+v; want the rejected 3 and the last usage", len(ov.Messages), ov.LastUsage)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("provider calls = %d, want 3", n)
	}
}

func TestCompact_OverflowTwice_ContextExceeded(t *testing.T) {
	var calls atomic.Int32
	rc := &recordingCompactor{fn: func(in toolloop.CompactInput) (toolloop.CompactResult, error) {
		if in.Reason != toolloop.CompactOverflow {
			return toolloop.CompactResult{}, nil
		}
		return firstOnly(in)
	}}
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  overflowAfter(&calls, toolCallResp("c1", echoToolName, `{}`, llms.Usage{})),
		Request: userReq("go"), Tools: []toolloop.Tool{echoTool()}, Compact: rc.compact,
	})
	if out.Kind != toolloop.OutcomeContextExceeded {
		t.Fatalf("kind = %q, want context_exceeded; err=%v", out.Kind, out.Err)
	}
	if len(out.Messages) != 1 || out.Compactions != 1 {
		t.Fatalf("Messages = %d, Compactions = %d; want the compacted 1 that was sent, 1", len(out.Messages), out.Compactions)
	}
	equalReasons(t, rc.reasons(), toolloop.CompactBeforeTurn, toolloop.CompactBeforeTurn, toolloop.CompactOverflow)
	if n := calls.Load(); n != 3 {
		t.Errorf("provider calls = %d, want 3 (one retry)", n)
	}
}

func TestCompact_OverflowDeclined_ContextExceeded(t *testing.T) {
	var calls atomic.Int32
	rc := &recordingCompactor{}
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  overflowAfter(&calls, toolCallResp("c1", echoToolName, `{}`, llms.Usage{})),
		Request: userReq("go"), Tools: []toolloop.Tool{echoTool()}, Compact: rc.compact,
	})
	if out.Kind != toolloop.OutcomeContextExceeded || len(out.Messages) != 3 || out.Compactions != 0 {
		t.Fatalf("kind = %q, Messages = %d, Compactions = %d; want context_exceeded, the 3 sent, 0", out.Kind, len(out.Messages), out.Compactions)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("provider calls = %d, want 2 (no retry)", n)
	}
}

func TestCompact_OverflowRetryResetsAfterSuccess(t *testing.T) {
	// Each successful call earns the next overflow its own compaction.
	var calls atomic.Int32
	rc := &recordingCompactor{fn: func(in toolloop.CompactInput) (toolloop.CompactResult, error) {
		if in.Reason != toolloop.CompactOverflow {
			return toolloop.CompactResult{}, nil
		}
		return firstOnly(in)
	}}
	client := sequence(&calls,
		toolCallResp("c1", echoToolName, `{}`, llms.Usage{}),
		overflowError(),
		toolCallResp("c2", echoToolName, `{}`, llms.Usage{}),
		overflowError(),
		testllm.TextResponse("done"),
	)
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client: client, Request: userReq("go"), Tools: []toolloop.Tool{echoTool()}, Compact: rc.compact,
	})
	if out.Kind != toolloop.OutcomeFinalAnswer || out.Compactions != 2 {
		t.Fatalf("kind = %q, Compactions = %d; want final_answer, 2 (err=%v)", out.Kind, out.Compactions, out.Err)
	}
}

func TestCompact_HookError_CompactionError(t *testing.T) {
	boom := errors.New("summarizer down")
	for _, reason := range []toolloop.CompactReason{toolloop.CompactBeforeTurn, toolloop.CompactOverflow} {
		t.Run(string(reason), func(t *testing.T) {
			var calls atomic.Int32
			hook := func(_ context.Context, in toolloop.CompactInput) (toolloop.CompactResult, error) {
				if in.Reason != reason || len(in.Messages) < 3 {
					return toolloop.CompactResult{}, nil
				}
				return toolloop.CompactResult{Usage: llms.Usage{InputTokens: 4, ProviderRequestID: "sum-1"}}, boom
			}
			out := toolloop.Run(context.Background(), toolloop.Config{
				Client:  overflowAfter(&calls, toolCallResp("c1", echoToolName, `{}`, llms.Usage{InputTokens: 9})),
				Request: userReq("go"), Tools: []toolloop.Tool{echoTool()}, Compact: hook,
			})
			if out.Kind != toolloop.OutcomeCompactionError {
				t.Fatalf("kind = %q, want compaction_error; err=%v", out.Kind, out.Err)
			}
			var ce *toolloop.CompactionError
			if !errors.As(out.Err, &ce) || ce.Reason != reason || !errors.Is(out.Err, boom) {
				t.Fatalf("Err = %v, want a *CompactionError (%s) wrapping the hook error", out.Err, reason)
			}
			if len(out.Messages) != 3 || out.Response.Message.Role != llms.RoleAssistant || out.Iterations != 1 {
				t.Errorf("Messages = %d, Response = %+v, Iterations = %d; want the pre-compaction 3, the last response, 1", len(out.Messages), out.Response, out.Iterations)
			}
			// Usage the failed hook spent is still counted, apart from the loop's.
			if out.CompactionUsage.InputTokens != 4 || out.CompactionUsage.ProviderRequestID != "sum-1" || out.TotalUsage.InputTokens != 9 {
				t.Errorf("CompactionUsage = %+v, TotalUsage = %+v", out.CompactionUsage, out.TotalUsage)
			}
			if out.Compactions != 0 {
				t.Errorf("Compactions = %d, want 0", out.Compactions)
			}
		})
	}
}

func TestCompact_InvalidResult_CompactionError(t *testing.T) {
	orphan := llms.ToolResultMessage(llms.ToolResult{ToolCallID: "nope", Content: "x"})
	cases := []struct {
		name       string
		msgs       []llms.Message
		validation bool
	}{
		{"empty", []llms.Message{}, true},
		{"orphaned tool result", []llms.Message{llms.UserText("go"), orphan}, true},
		{"unanswered tool call", []llms.Message{llms.UserText("go"), testllm.ToolCallResponse("c9", echoToolName, `{}`).Message}, true},
		{"ends with assistant", []llms.Message{llms.UserText("go"), llms.AssistantText("hi")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hook := func(context.Context, toolloop.CompactInput) (toolloop.CompactResult, error) {
				return toolloop.CompactResult{Messages: tc.msgs}, nil
			}
			client := scriptedResponses(testllm.TextResponse("unreached"))
			out := toolloop.Run(context.Background(), toolloop.Config{Client: client, Request: userReq("go"), Compact: hook})
			if out.Kind != toolloop.OutcomeCompactionError {
				t.Fatalf("kind = %q, want compaction_error; err=%v", out.Kind, out.Err)
			}
			var ve *middleware.ValidationError
			if got := errors.As(out.Err, &ve); got != tc.validation {
				t.Fatalf("errors.As ValidationError = %v, want %v (err=%v)", got, tc.validation, out.Err)
			}
			if len(client.Calls()) != 0 || len(out.Messages) != 1 {
				t.Errorf("calls = %d, Messages = %d; want no provider call and the original transcript", len(client.Calls()), len(out.Messages))
			}
		})
	}
}

func TestCompact_LoopContextDoneWins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	hook := func(ctx context.Context, _ toolloop.CompactInput) (toolloop.CompactResult, error) {
		cancel()
		return toolloop.CompactResult{}, ctx.Err()
	}
	out := toolloop.Run(ctx, toolloop.Config{Client: scriptedResponses(testllm.TextResponse("x")), Request: userReq("go"), Compact: hook})
	if out.Kind != toolloop.OutcomeCanceled || !errors.Is(out.Err, context.Canceled) {
		t.Fatalf("kind = %q, err = %v; want canceled", out.Kind, out.Err)
	}
}

func TestCompact_UsageKeptApart(t *testing.T) {
	n := 0
	hook := func(context.Context, toolloop.CompactInput) (toolloop.CompactResult, error) {
		n++
		return toolloop.CompactResult{Usage: llms.Usage{InputTokens: 100, OutputTokens: 10, ProviderRequestID: "s"}}, nil
	}
	client := scriptedResponses(
		toolCallResp("c1", echoToolName, `{}`, llms.Usage{InputTokens: 1, ProviderRequestID: "a"}),
		finalTextResp("done", llms.Usage{InputTokens: 2, ProviderRequestID: "b"}),
	)
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client: client, Request: userReq("go"), Tools: []toolloop.Tool{echoTool()}, Compact: hook,
	})
	if out.TotalUsage.InputTokens != 3 {
		t.Errorf("TotalUsage.InputTokens = %d, want 3 (the loop model's calls only)", out.TotalUsage.InputTokens)
	}
	if out.CompactionUsage.InputTokens != 200 || out.CompactionUsage.OutputTokens != 20 || out.CompactionUsage.ProviderRequestID != "" {
		t.Errorf("CompactionUsage = %+v, want 2 calls summed, ProviderRequestID collapsed", out.CompactionUsage)
	}
	if n != 2 || out.Compactions != 0 {
		t.Errorf("hook calls = %d, Compactions = %d; want 2, 0 (usage without a transcript is no compaction)", n, out.Compactions)
	}
}

func TestCompact_ValidationFailureReturnsBeforeAnyCall(t *testing.T) {
	// A config error is reported before the hook runs.
	called := false
	hook := func(context.Context, toolloop.CompactInput) (toolloop.CompactResult, error) {
		called = true
		return toolloop.CompactResult{}, nil
	}
	out := toolloop.Run(context.Background(), toolloop.Config{Client: scriptedResponses(), Compact: hook})
	if out.Kind != toolloop.OutcomeToolError || called {
		t.Fatalf("kind = %q, hook called = %v; want tool_error, false", out.Kind, called)
	}
}
