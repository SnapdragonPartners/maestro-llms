package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// ADR-0022: ReasoningConfig maps 1:1 to Anthropic's controls or is omitted.
func TestReasoningMapping(t *testing.T) {
	effort := func(e llms.ReasoningEffort) *llms.ReasoningConfig { return &llms.ReasoningConfig{Effort: e} }
	for _, tc := range []struct {
		name         string
		in           *llms.ReasoningConfig
		wantThinking any // nil: field absent
		wantEffort   any // nil: output_config absent
		wantApplied  *llms.ReasoningConfig
	}{
		{name: "unset", in: nil},
		{name: "empty config", in: &llms.ReasoningConfig{}},
		{name: "low", in: effort(llms.ReasoningEffortLow), wantEffort: "low", wantApplied: effort(llms.ReasoningEffortLow)},
		{name: "medium", in: effort(llms.ReasoningEffortMedium), wantEffort: "medium", wantApplied: effort(llms.ReasoningEffortMedium)},
		{name: "high", in: effort(llms.ReasoningEffortHigh), wantEffort: "high", wantApplied: effort(llms.ReasoningEffortHigh)},
		{name: "xhigh", in: effort(llms.ReasoningEffortXHigh), wantEffort: "xhigh", wantApplied: effort(llms.ReasoningEffortXHigh)},
		{name: "max", in: effort(llms.ReasoningEffortMax), wantEffort: "max", wantApplied: effort(llms.ReasoningEffortMax)},
		{name: "minimal has no level", in: effort(llms.ReasoningEffortMinimal)},
		{name: "unknown value omitted", in: effort("turbo")},
		{
			name: "none disables thinking", in: effort(llms.ReasoningEffortNone),
			wantThinking: map[string]any{"type": "disabled"}, wantApplied: effort(llms.ReasoningEffortNone),
		},
		{
			name: "budget", in: &llms.ReasoningConfig{BudgetTokens: 2048},
			wantThinking: map[string]any{"type": "enabled", "budget_tokens": float64(2048)},
			wantApplied:  &llms.ReasoningConfig{BudgetTokens: 2048},
		},
		{
			name: "budget wins over none", in: &llms.ReasoningConfig{Effort: llms.ReasoningEffortNone, BudgetTokens: 2048},
			wantThinking: map[string]any{"type": "enabled", "budget_tokens": float64(2048)},
			wantApplied:  &llms.ReasoningConfig{BudgetTokens: 2048},
		},
		{
			name: "budget and effort", in: &llms.ReasoningConfig{Effort: llms.ReasoningEffortHigh, BudgetTokens: 2048},
			wantThinking: map[string]any{"type": "enabled", "budget_tokens": float64(2048)}, wantEffort: "high",
			wantApplied: &llms.ReasoningConfig{Effort: llms.ReasoningEffortHigh, BudgetTokens: 2048},
		},
		{name: "negative budget is unset", in: &llms.ReasoningConfig{BudgetTokens: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, textMsgJSON)
			})
			resp, err := c.Complete(context.Background(), llms.ChatRequest{
				Messages: []llms.Message{llms.UserText("q")}, MaxTokens: 4096, Reasoning: tc.in,
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if got := body["thinking"]; !reflect.DeepEqual(got, tc.wantThinking) {
				t.Errorf("thinking = %v, want %v", got, tc.wantThinking)
			}
			var gotEffort any
			if oc, ok := body["output_config"].(map[string]any); ok {
				gotEffort = oc["effort"]
			} else if _, present := body["output_config"]; present {
				t.Errorf("output_config = %v, want absent or an object", body["output_config"])
			}
			if gotEffort != tc.wantEffort {
				t.Errorf("output_config.effort = %v, want %v", gotEffort, tc.wantEffort)
			}
			if !reflect.DeepEqual(resp.AppliedReasoning, tc.wantApplied) {
				t.Errorf("AppliedReasoning = %+v, want %+v", resp.AppliedReasoning, tc.wantApplied)
			}
		})
	}
}
