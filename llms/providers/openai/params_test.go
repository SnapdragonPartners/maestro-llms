package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// ADR-0024: ProviderParams deep-merge into the wire body after mapping.
func TestProviderParams(t *testing.T) {
	var body map[string]any
	c := newChat(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respTextJSON)
	})
	temp := float32(0.3)
	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages:    []llms.Message{llms.UserText("q")},
		MaxTokens:   4096,
		Temperature: &temp,
		Reasoning:   &llms.ReasoningConfig{Effort: llms.ReasoningEffortHigh},
		ProviderParams: map[llms.ProviderID]map[string]any{
			llms.ProviderOpenAI:    {"max_output_tokens": 50, "reasoning": map[string]any{"effort": "low", "summary": "auto"}},
			llms.ProviderAnthropic: {"zzz": 1},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, ok := body["zzz"]; ok {
		t.Error("another provider's params reached the wire")
	}
	if body["max_output_tokens"] != float64(50) || body["temperature"] == nil {
		t.Errorf("max_output_tokens = %v, temperature = %v; want the override and the mapped temperature kept", body["max_output_tokens"], body["temperature"])
	}
	if r, _ := body["reasoning"].(map[string]any); r["effort"] != "low" || r["summary"] != "auto" {
		t.Errorf("reasoning = %v, want effort low + summary auto", body["reasoning"])
	}
	if resp.AppliedReasoning != nil {
		t.Errorf("AppliedReasoning = %+v, want nil: the mapped effort was overridden", resp.AppliedReasoning)
	}

	bad := newChat(t, func(http.ResponseWriter, *http.Request) { t.Fatal("must fail before any HTTP call") })
	_, err = bad.Complete(context.Background(), llms.ChatRequest{
		Messages:       []llms.Message{llms.UserText("q")},
		ProviderParams: map[llms.ProviderID]map[string]any{llms.ProviderOpenAI: {"bad": make(chan int)}},
	})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
		t.Fatalf("non-marshalable value: err = %v, want bad_request", err)
	}
}
