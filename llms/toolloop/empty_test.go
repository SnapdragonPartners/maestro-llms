package toolloop_test

import (
	"context"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

// ADR-0020 / #85: a response with no text and no tool calls.

// budgetExhausted is what a reasoning model returns when it spends its
// whole output budget on reasoning: no text and no tool calls, at most
// reasoning parts. StopReason is the provider's raw string.
func budgetExhausted(content []llms.ContentPart) llms.ChatResponse {
	return llms.ChatResponse{
		Message:    llms.Message{Role: llms.RoleAssistant, Content: content},
		StopReason: "length",
		Usage:      llms.Usage{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 50},
	}
}

func TestRun_EmptyResponse_NotAppendedAndSendable(t *testing.T) {
	cases := []struct {
		name    string
		content []llms.ContentPart
	}{
		{name: "no parts (vLLM, OpenAI, Google shape)", content: nil},
		{name: "empty text part (Anthropic shape)", content: []llms.ContentPart{{Type: llms.ContentText}}},
		// ADR-0021: Anthropic returns the thinking block it spent the
		// budget on; reasoning alone is not a reply.
		{name: "reasoning only (Anthropic with thinking)", content: []llms.ContentPart{{
			Type:      llms.ContentReasoning,
			Reasoning: &llms.Reasoning{Provider: llms.ProviderAnthropic, ProviderData: []byte(`{}`)},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := scriptedResponses(
				toolCallResp("c1", echoToolName, `{}`, llms.Usage{InputTokens: 10}),
				budgetExhausted(tc.content),
			)
			var events []toolloop.IterationEvent
			out := toolloop.Run(context.Background(), toolloop.Config{
				Client:      client,
				Request:     userReq("investigate"),
				Tools:       []toolloop.Tool{echoTool()},
				OnIteration: func(e toolloop.IterationEvent) { events = append(events, e) },
			})
			if out.Kind != toolloop.OutcomeEmptyResponse {
				t.Fatalf("kind = %q, want empty_response; err=%v", out.Kind, out.Err)
			}
			if out.Err != nil {
				t.Fatalf("Err = %v, want nil", out.Err)
			}
			if out.Response.StopReason != "length" {
				t.Fatalf("Response.StopReason = %q, want the empty response's", out.Response.StopReason)
			}
			if out.Iterations != 1 {
				t.Fatalf("Iterations = %d, want 1", out.Iterations)
			}
			if out.TotalUsage.InputTokens != 110 || out.TotalUsage.ReasoningTokens != 50 {
				t.Fatalf("TotalUsage = %+v, want the empty response's usage included", out.TotalUsage)
			}
			// user, asst(c1), tool(c1): no trailing empty assistant message.
			if len(out.Messages) != 3 || out.Messages[2].Role != llms.RoleTool {
				t.Fatalf("Messages = %+v, want user/assistant/tool with no empty assistant", out.Messages)
			}
			if len(events) != 2 || events[1].Response.StopReason != "length" {
				t.Fatalf("OnIteration events = %+v, want one per response including the empty one", events)
			}

			// The bug report's continuation: append a nudge and resend
			// through the Validation middleware.
			next := append(out.Messages, llms.UserText("call submit now"))
			resend := middleware.ValidationChat()(&testllm.FakeChatClient{Text: "ok"})
			if _, err := resend.Complete(context.Background(), llms.ChatRequest{Messages: next}); err != nil {
				t.Fatalf("continuing from Outcome.Messages: %v", err)
			}
		})
	}
}

func TestRun_EmptyFirstResponse_MessagesAreInitial(t *testing.T) {
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  scriptedResponses(budgetExhausted(nil)),
		Request: userReq("hi"),
	})
	if out.Kind != toolloop.OutcomeEmptyResponse {
		t.Fatalf("kind = %q, want empty_response; err=%v", out.Kind, out.Err)
	}
	if len(out.Messages) != 1 || out.Iterations != 0 {
		t.Fatalf("Messages = %+v, Iterations = %d; want only the initial message and 0", out.Messages, out.Iterations)
	}
}

func TestRun_ToolCallWithoutText_IsNotEmpty(t *testing.T) {
	// A tool-calling turn with no text is the normal shape, not empty.
	client := scriptedResponses(
		toolCallResp("c1", echoToolName, `{}`, llms.Usage{}),
		finalTextResp("done", llms.Usage{}),
	)
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  client,
		Request: userReq("go"),
		Tools:   []toolloop.Tool{echoTool()},
	})
	if out.Kind != toolloop.OutcomeFinalAnswer || out.Iterations != 1 {
		t.Fatalf("kind = %q, Iterations = %d; want final_answer after 1 tool turn", out.Kind, out.Iterations)
	}
}
