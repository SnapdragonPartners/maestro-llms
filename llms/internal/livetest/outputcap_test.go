package livetest

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
)

func capResp(stop llms.StopReason, visible, reasoning int) llms.ChatResponse {
	return llms.ChatResponse{StopReason: stop, Usage: llms.Usage{
		OutputTokens: visible, ReasoningTokens: reasoning, BillableOutputTokens: visible + reasoning,
	}}
}

// checkCap's rules (ADR-0044 decision 3): a breach or missing usage fails at
// once, a qualifying run passes, and a non-qualifying run is retried until
// capAttempts.
func TestCheckCap(t *testing.T) {
	const stop llms.StopReason = "max_tokens"
	tests := []struct {
		name          string
		wantErr       string // substring; empty means pass
		responses     []llms.ChatResponse
		calls         int
		wantReasoning bool
	}{
		{name: "qualifying text run passes", responses: []llms.ChatResponse{capResp(stop, 64, 0)}, calls: 1},
		{name: "qualifying reasoning run passes", responses: []llms.ChatResponse{capResp(stop, 0, 64)}, calls: 1, wantReasoning: true},
		{name: "breach fails on the first attempt", wantErr: "exceeds MaxTokens",
			responses: []llms.ChatResponse{capResp(stop, 40, 30)}, calls: 1},
		{name: "breach fails even when the run would not qualify", wantErr: "exceeds MaxTokens",
			responses: []llms.ChatResponse{capResp("end_turn", 65, 0)}, calls: 1},
		{name: "breach on a retry fails", wantErr: "exceeds MaxTokens",
			responses: []llms.ChatResponse{capResp("end_turn", 10, 0), capResp(stop, 65, 0)}, calls: 2},
		{name: "no usage fails", wantErr: "no billable output",
			responses: []llms.ChatResponse{capResp(stop, 0, 0)}, calls: 1},
		{name: "under the cap is retried and passes", responses: []llms.ChatResponse{
			capResp("end_turn", 10, 0), capResp(stop, 64, 0),
		}, calls: 2},
		{name: "under the cap every time fails after capAttempts", wantErr: fmt.Sprintf("in %d attempts", capAttempts),
			responses: []llms.ChatResponse{capResp("end_turn", 10, 0)}, calls: capAttempts},
		{name: "no reasoning when required fails after capAttempts", wantErr: fmt.Sprintf("in %d attempts", capAttempts),
			responses: []llms.ChatResponse{capResp(stop, 64, 0)}, calls: capAttempts, wantReasoning: true},
		{name: "reasoning on a retry passes", responses: []llms.ChatResponse{
			capResp(stop, 64, 0), capResp(stop, 10, 54),
		}, calls: 2, wantReasoning: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &testllm.FakeChatClient{Responses: tt.responses}
			req := llms.ChatRequest{Messages: []llms.Message{llms.UserText("x")}, MaxTokens: 64}
			err := checkCap(fake, req, stop, tt.wantReasoning, t.Logf)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("checkCap: %v, want pass", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("checkCap: %v, want error containing %q", err, tt.wantErr)
			}
			if got := len(fake.Calls()); got != tt.calls {
				t.Fatalf("%d calls, want %d", got, tt.calls)
			}
		})
	}
}

func TestCheckCapCallError(t *testing.T) {
	boom := errors.New("boom")
	fake := &testllm.FakeChatClient{Err: boom}
	err := checkCap(fake, llms.ChatRequest{MaxTokens: 64}, "max_tokens", false, t.Logf)
	if !errors.Is(err, boom) {
		t.Fatalf("checkCap: %v, want the call error", err)
	}
}
