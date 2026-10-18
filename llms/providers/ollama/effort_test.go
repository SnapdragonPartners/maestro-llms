package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// ADR-0022: none → think:false, low/medium/high → think level; the rest omitted.
func TestReasoningMapping(t *testing.T) {
	effort := func(e llms.ReasoningEffort) *llms.ReasoningConfig { return &llms.ReasoningConfig{Effort: e} }
	for _, tc := range []struct {
		name        string
		in          *llms.ReasoningConfig
		want        any // think; nil: absent
		wantApplied *llms.ReasoningConfig
	}{
		{name: "unset", in: nil},
		{name: "none", in: effort(llms.ReasoningEffortNone), want: false, wantApplied: effort(llms.ReasoningEffortNone)},
		{name: "low", in: effort(llms.ReasoningEffortLow), want: "low", wantApplied: effort(llms.ReasoningEffortLow)},
		{name: "medium", in: effort(llms.ReasoningEffortMedium), want: "medium", wantApplied: effort(llms.ReasoningEffortMedium)},
		{name: "high", in: effort(llms.ReasoningEffortHigh), want: "high", wantApplied: effort(llms.ReasoningEffortHigh)},
		{name: "minimal has no level", in: effort(llms.ReasoningEffortMinimal)},
		{name: "xhigh has no level", in: effort(llms.ReasoningEffortXHigh)},
		{name: "max has no level", in: effort(llms.ReasoningEffortMax)},
		{name: "budget has no control", in: &llms.ReasoningConfig{BudgetTokens: 512}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, respTextJSON)
			})
			resp, err := c.Complete(context.Background(), llms.ChatRequest{
				Messages: []llms.Message{llms.UserText("q")}, Reasoning: tc.in,
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			got, present := body["think"]
			if tc.want == nil && present {
				t.Errorf("think = %v, want absent", got)
			} else if tc.want != nil && got != tc.want {
				t.Errorf("think = %v, want %v", got, tc.want)
			}
			if !reflect.DeepEqual(resp.AppliedReasoning, tc.wantApplied) {
				t.Errorf("AppliedReasoning = %+v, want %+v", resp.AppliedReasoning, tc.wantApplied)
			}
		})
	}
}
