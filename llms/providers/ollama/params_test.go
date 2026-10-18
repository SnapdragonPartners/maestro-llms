package ollama

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
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
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
			llms.ProviderOllama:    {"think": false, "options": map[string]any{"num_predict": 50}, "keep_alive": "1m"},
			llms.ProviderAnthropic: {"zzz": 1},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, ok := body["zzz"]; ok {
		t.Error("another provider's params reached the wire")
	}
	opts, _ := body["options"].(map[string]any)
	if opts["num_predict"] != float64(50) || opts["temperature"] == nil || body["think"] != false || body["keep_alive"] != "1m" {
		t.Errorf("body = %v, want num_predict 50 with temperature kept, think false, keep_alive", body)
	}
	if resp.AppliedReasoning != nil {
		t.Errorf("AppliedReasoning = %+v, want nil: the mapped effort was overridden", resp.AppliedReasoning)
	}

	bad := newClient(t, func(http.ResponseWriter, *http.Request) { t.Fatal("must fail before any HTTP call") })
	_, err = bad.Complete(context.Background(), llms.ChatRequest{
		Messages:       []llms.Message{llms.UserText("q")},
		ProviderParams: map[llms.ProviderID]map[string]any{llms.ProviderOllama: {"bad": make(chan int)}},
	})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
		t.Fatalf("non-marshalable value: err = %v, want bad_request", err)
	}
}
