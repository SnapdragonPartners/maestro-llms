package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// ADR-0024: ProviderParams deep-merge into the wire body after mapping.
func TestProviderParams(t *testing.T) {
	capture := func(t *testing.T, req llms.ChatRequest) (map[string]any, llms.ChatResponse) {
		t.Helper()
		var body map[string]any
		c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, textMsgJSON)
		})
		resp, err := c.Complete(context.Background(), req)
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		return body, resp
	}
	msgs := []llms.Message{llms.UserText("q")}

	t.Run("override replaces mapped values, other providers ignored", func(t *testing.T) {
		body, resp := capture(t, llms.ChatRequest{
			Messages: msgs, MaxTokens: 4096,
			Reasoning: &llms.ReasoningConfig{Effort: llms.ReasoningEffortHigh},
			ProviderParams: map[llms.ProviderID]map[string]any{
				llms.ProviderAnthropic: {"max_tokens": 50, "output_config": map[string]any{"effort": "low"}},
				llms.ProviderGoogle:    {"zzz": 1},
			},
		})
		if body["max_tokens"] != float64(50) {
			t.Errorf("max_tokens = %v, want the override 50", body["max_tokens"])
		}
		if oc, _ := body["output_config"].(map[string]any); oc["effort"] != "low" {
			t.Errorf("output_config = %v, want effort low", body["output_config"])
		}
		if _, ok := body["zzz"]; ok {
			t.Error("another provider's params reached the wire")
		}
		if resp.AppliedReasoning != nil {
			t.Errorf("AppliedReasoning = %+v, want nil: the mapped effort was overridden", resp.AppliedReasoning)
		}
	})

	t.Run("nested params merge with mapped siblings", func(t *testing.T) {
		body, resp := capture(t, llms.ChatRequest{
			Messages: msgs, MaxTokens: 4096,
			Reasoning: &llms.ReasoningConfig{BudgetTokens: 2048},
			ProviderParams: map[llms.ProviderID]map[string]any{
				llms.ProviderAnthropic: {"thinking": map[string]any{"display": "summarized"}},
			},
		})
		want := map[string]any{"type": "enabled", "budget_tokens": float64(2048), "display": "summarized"}
		if !reflect.DeepEqual(body["thinking"], want) {
			t.Errorf("thinking = %v, want %v", body["thinking"], want)
		}
		if resp.AppliedReasoning == nil || resp.AppliedReasoning.BudgetTokens != 2048 {
			t.Errorf("AppliedReasoning = %+v, want the budget kept (not overridden)", resp.AppliedReasoning)
		}
	})

	t.Run("non-marshalable value is a local bad_request", func(t *testing.T) {
		c := newClient(t, func(http.ResponseWriter, *http.Request) { t.Fatal("must fail before any HTTP call") })
		_, err := c.Complete(context.Background(), llms.ChatRequest{
			Messages:       msgs,
			ProviderParams: map[llms.ProviderID]map[string]any{llms.ProviderAnthropic: {"bad": make(chan int)}},
		})
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
			t.Fatalf("err = %v, want bad_request", err)
		}
	})
}
