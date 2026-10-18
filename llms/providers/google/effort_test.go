package google

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// ADR-0022: Effort maps to thinkingLevel and BudgetTokens to thinkingBudget;
// the caller picks the field, the adapter does not guess the generation.
func TestReasoningMapping(t *testing.T) {
	effort := func(e llms.ReasoningEffort) *llms.ReasoningConfig { return &llms.ReasoningConfig{Effort: e} }
	for _, tc := range []struct {
		name        string
		in          *llms.ReasoningConfig
		want        map[string]any // thinkingConfig; nil: absent
		wantApplied *llms.ReasoningConfig
	}{
		{name: "unset", in: nil},
		{name: "minimal", in: effort(llms.ReasoningEffortMinimal), want: map[string]any{"thinkingLevel": "MINIMAL"}, wantApplied: effort(llms.ReasoningEffortMinimal)},
		{name: "low", in: effort(llms.ReasoningEffortLow), want: map[string]any{"thinkingLevel": "LOW"}, wantApplied: effort(llms.ReasoningEffortLow)},
		{name: "medium", in: effort(llms.ReasoningEffortMedium), want: map[string]any{"thinkingLevel": "MEDIUM"}, wantApplied: effort(llms.ReasoningEffortMedium)},
		{name: "high", in: effort(llms.ReasoningEffortHigh), want: map[string]any{"thinkingLevel": "HIGH"}, wantApplied: effort(llms.ReasoningEffortHigh)},
		{name: "none has no level", in: effort(llms.ReasoningEffortNone)},
		{name: "xhigh has no level", in: effort(llms.ReasoningEffortXHigh)},
		{name: "max has no level", in: effort(llms.ReasoningEffortMax)},
		{name: "unknown value omitted", in: effort("turbo")},
		{
			name: "budget", in: &llms.ReasoningConfig{BudgetTokens: 512},
			want: map[string]any{"thinkingBudget": float64(512)}, wantApplied: &llms.ReasoningConfig{BudgetTokens: 512},
		},
		{
			name: "both sent as given", in: &llms.ReasoningConfig{Effort: llms.ReasoningEffortLow, BudgetTokens: 512},
			want:        map[string]any{"thinkingLevel": "LOW", "thinkingBudget": float64(512)},
			wantApplied: &llms.ReasoningConfig{Effort: llms.ReasoningEffortLow, BudgetTokens: 512},
		},
		{
			name: "budget kept, unmappable effort dropped", in: &llms.ReasoningConfig{Effort: llms.ReasoningEffortMax, BudgetTokens: 512},
			want: map[string]any{"thinkingBudget": float64(512)}, wantApplied: &llms.ReasoningConfig{BudgetTokens: 512},
		},
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
			var got map[string]any
			if gc, ok := body["generationConfig"].(map[string]any); ok {
				got, _ = gc["thinkingConfig"].(map[string]any)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("thinkingConfig = %v, want %v (body %v)", got, tc.want, body)
			}
			if !reflect.DeepEqual(resp.AppliedReasoning, tc.wantApplied) {
				t.Errorf("AppliedReasoning = %+v, want %+v", resp.AppliedReasoning, tc.wantApplied)
			}
		})
	}
}
