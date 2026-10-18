package vllm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// ADR-0022: every neutral effort level maps 1:1 to reasoning_effort; other
// values and BudgetTokens are omitted.
func TestReasoningMapping(t *testing.T) {
	effort := func(e llms.ReasoningEffort) *llms.ReasoningConfig { return &llms.ReasoningConfig{Effort: e} }
	cases := []struct {
		name        string
		in          *llms.ReasoningConfig
		wantWire    any // nil: absent
		wantApplied *llms.ReasoningConfig
	}{
		{name: "unset", in: nil},
		{name: "unknown value omitted", in: effort("turbo")},
		{name: "budget has no control", in: &llms.ReasoningConfig{BudgetTokens: 2048}},
		{
			name: "budget dropped, effort kept", in: &llms.ReasoningConfig{Effort: llms.ReasoningEffortLow, BudgetTokens: 2048},
			wantWire: "low", wantApplied: effort(llms.ReasoningEffortLow),
		},
	}
	for _, e := range []llms.ReasoningEffort{
		llms.ReasoningEffortNone, llms.ReasoningEffortMinimal, llms.ReasoningEffortLow, llms.ReasoningEffortMedium,
		llms.ReasoningEffortHigh, llms.ReasoningEffortXHigh, llms.ReasoningEffortMax,
	} {
		cases = append(cases, struct {
			name        string
			in          *llms.ReasoningConfig
			wantWire    any
			wantApplied *llms.ReasoningConfig
		}{name: string(e), in: effort(e), wantWire: string(e), wantApplied: effort(e)})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, textCompletionJSON)
			})
			resp, err := c.Complete(context.Background(), llms.ChatRequest{
				Messages: []llms.Message{llms.UserText("q")}, Reasoning: tc.in,
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if got := body["reasoning_effort"]; got != tc.wantWire {
				t.Errorf("wire effort = %v, want %v (body %v)", got, tc.wantWire, body)
			}
			if !reflect.DeepEqual(resp.AppliedReasoning, tc.wantApplied) {
				t.Errorf("AppliedReasoning = %+v, want %+v", resp.AppliedReasoning, tc.wantApplied)
			}
		})
	}
}
