package livetest

import (
	"context"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

// boundChecker wraps a client and checks DefaultEstimator's input bound
// (ADR-0043) against the billed input of every call: a bounded request whose
// Usage.InputTokens exceeds its bound fails the test. Calls the bounder
// declines are counted separately, and so are checked calls that replay
// reasoning (a request carries reasoning only as replay).
type boundChecker struct {
	next      llms.ChatClient
	t         *testing.T
	checked   int
	unbounded int
	replayed  int
}

func (b *boundChecker) Model() llms.ModelRef { return b.next.Model() }

func (b *boundChecker) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
	bound, ok, err := middleware.DefaultEstimator{}.BoundChat(b.next.Model(), req)
	if err != nil {
		b.t.Errorf("BoundChat: %v", err)
	}
	resp, err := b.next.Complete(ctx, req)
	if err != nil || !ok {
		if err == nil {
			b.unbounded++
		}
		return resp, err //nolint:wrapcheck // pass through for the scenario to classify
	}
	b.checked++
	if replaysReasoning(req.Messages) {
		b.replayed++
	}
	in := resp.Usage.InputTokens
	b.t.Logf("bound check: billed input %d <= bound %d? (%d messages, %d tools; bound is %.1fx billed)",
		in, bound.InputTokens, len(req.Messages), len(req.Tools), float64(bound.InputTokens)/float64(max(in, 1)))
	if in > bound.InputTokens {
		b.t.Errorf("billed input %d exceeds the bound %d (%d messages, %d tools)", in, bound.InputTokens, len(req.Messages), len(req.Tools))
	}
	return resp, nil
}

func replaysReasoning(msgs []llms.Message) bool {
	for i := range msgs {
		if hasReasoning(&msgs[i]) {
			return true
		}
	}
	return false
}

// InputBound checks DefaultEstimator's input bound against billed usage for
// text-only requests: a minimal one, where the framing margins dominate, and
// a multi-turn one with a system prompt. Each must be bounded, and its billed
// input must not exceed the bound.
func InputBound(t *testing.T, c llms.ChatClient) {
	t.Helper()
	if testing.Short() {
		t.Skip("live bound check skipped under -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	chk := &boundChecker{next: withRetry(c), t: t}
	reqs := []llms.ChatRequest{
		{Messages: []llms.Message{llms.UserText("Hi.")}, MaxTokens: 64},
		{
			System: []llms.ContentPart{llms.Text("You are a terse assistant. Answer in one short sentence.")},
			Messages: []llms.Message{
				llms.UserText("Name a primary color."),
				llms.AssistantText("Red."),
				llms.UserText("Name another, and say why it is primary."),
			},
			MaxTokens: 256,
		},
	}
	for i := range reqs {
		if _, err := chk.Complete(ctx, reqs[i]); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if chk.unbounded > 0 || chk.checked != len(reqs) {
		t.Fatalf("checked %d of %d requests (%d unbounded), want all bounded", chk.checked, len(reqs), chk.unbounded)
	}
}

// InputBoundToolLoop checks the bound across the notes-chain tool loop. Every
// iteration resends the tool definition and earlier tool calls and results,
// so the check covers the tool overhead allowance. It is for a provider whose
// requests with tools are bounded (Anthropic): every iteration must be
// bounded.
//
// With requireReplay (a thinking model), at least one checked request must
// replay reasoning, so the check covers replayed reasoning's byte count.
// Adaptive thinking may skip thinking, so the loop is retried a few times
// before failing.
func InputBoundToolLoop(t *testing.T, c llms.ChatClient, requireReplay bool) {
	t.Helper()
	if testing.Short() {
		t.Skip("live bound check skipped under -short")
	}
	const attempts = 3
	for attempt := 1; ; attempt++ {
		chk := &boundChecker{next: c, t: t}
		out, _ := runReasoningLoop(t, chk, notesSystem(), []toolloop.Tool{notesTool()})
		if out.Kind != toolloop.OutcomeFinalAnswer || out.Iterations < 2 {
			t.Fatalf("tool loop: kind=%q iterations=%d err=%v, want a final answer after >= 2 iterations", out.Kind, out.Iterations, out.Err)
		}
		if chk.unbounded > 0 || chk.checked < out.Iterations {
			t.Fatalf("checked %d of %d iterations (%d unbounded), want all bounded", chk.checked, out.Iterations, chk.unbounded)
		}
		if requireReplay && chk.replayed == 0 {
			if attempt < attempts {
				t.Logf("attempt %d: no checked request replayed reasoning; retrying", attempt)
				continue
			}
			t.Fatalf("no checked request replayed reasoning in %d attempts", attempts)
		}
		t.Logf("bound held on %d iteration(s); %d replayed reasoning", chk.checked, chk.replayed)
		return
	}
}
