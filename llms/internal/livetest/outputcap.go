package livetest

import (
	"context"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// Output cap scenarios (#149, ADR-0044). A hard budget admits an attempt on
// its input bound plus MaxTokens, so it relies on MaxTokens capping billable
// output, reasoning included. Each scenario sends a request that must stop at
// the cap (the provider's length stop reason) and checks that
// Usage.BillableOutputTokens is positive and at most MaxTokens.

// capAttempts retries a run that finished under the cap, or (for the
// reasoning scenario) didn't reason: neither says anything about the cap.
const capAttempts = 3

// OutputCapText checks the cap on a request with the provider's default
// reasoning, truncated while writing a long answer. lengthStop is the
// provider's length stop reason (for example "max_tokens").
func OutputCapText(t *testing.T, c llms.ChatClient, lengthStop llms.StopReason) {
	t.Helper()
	req := llms.ChatRequest{
		Messages:  []llms.Message{llms.UserText("List the integers from 1 to 2000, one per line, with no other text.")},
		MaxTokens: 64,
	}
	outputCap(t, c, req, lengthStop, false)
}

// OutputCapReasoning checks the cap on a reasoning request truncated while
// the model reasons: billable output, reasoning tokens included, must stay
// within MaxTokens, and the response must report reasoning tokens, so the
// check covers them. reasoning is the setting that makes the model reason
// hard (nil for a model that reasons by default).
func OutputCapReasoning(t *testing.T, c llms.ChatClient, lengthStop llms.StopReason, reasoning *llms.ReasoningConfig) {
	t.Helper()
	req := llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("Find every pair of positive integers (a, b) with a <= b and " +
			"a^3 + b^3 = 1729 * k for some k between 1 and 50. Show your reasoning, then list the pairs.")},
		MaxTokens: 512,
		Reasoning: reasoning,
	}
	outputCap(t, c, req, lengthStop, true)
}

func outputCap(t *testing.T, c llms.ChatClient, req llms.ChatRequest, lengthStop llms.StopReason, wantReasoning bool) {
	t.Helper()
	if testing.Short() {
		t.Skip("live output cap check skipped under -short")
	}
	rc := withRetry(c)
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
		resp, err := rc.Complete(ctx, req)
		cancel()
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		u := resp.Usage
		t.Logf("attempt %d: stop=%q billable output %d (visible %d, reasoning %d) <= MaxTokens %d?",
			attempt, resp.StopReason, u.BillableOutputTokens, u.OutputTokens, u.ReasoningTokens, req.MaxTokens)
		// The cap check comes first: a breach fails whether or not the
		// run otherwise qualifies.
		if u.BillableOutputTokens > req.MaxTokens {
			t.Fatalf("billable output %d exceeds MaxTokens %d (visible %d, reasoning %d)",
				u.BillableOutputTokens, req.MaxTokens, u.OutputTokens, u.ReasoningTokens)
		}
		if u.BillableOutputTokens <= 0 {
			t.Fatalf("no billable output reported (%d), so the cap can't be checked", u.BillableOutputTokens)
		}
		truncated := resp.StopReason == lengthStop
		reasoned := !wantReasoning || u.ReasoningTokens > 0
		if truncated && reasoned {
			return
		}
		if attempt == capAttempts {
			t.Fatalf("in %d attempts no run stopped at the cap (stop %q) with reasoning tokens when required (%v); last: stop=%q, reasoning %d",
				capAttempts, lengthStop, wantReasoning, resp.StopReason, u.ReasoningTokens)
		}
		t.Logf("attempt %d didn't qualify (truncated %v, reasoned %v); retrying", attempt, truncated, reasoned)
	}
}
