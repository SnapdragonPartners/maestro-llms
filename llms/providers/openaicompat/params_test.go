package openaicompat

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
		_, _ = io.WriteString(w, textCompletionJSON)
	})
	temp := float32(0.3)
	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages:    []llms.Message{llms.UserText("q")},
		MaxTokens:   4096,
		Temperature: &temp,
		Reasoning:   &llms.ReasoningConfig{Effort: llms.ReasoningEffortHigh},
		ProviderParams: map[llms.ProviderID]map[string]any{
			llms.ProviderOpenAICompat: {"max_tokens": 50, "reasoning_effort": "none", "chat_template_kwargs": map[string]any{"enable_thinking": false}},
			llms.ProviderAnthropic:    {"zzz": 1},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, ok := body["zzz"]; ok {
		t.Error("another provider's params reached the wire")
	}
	if body["max_tokens"] != float64(50) || body["reasoning_effort"] != "none" || body["temperature"] == nil {
		t.Errorf("body = %v, want max_tokens 50, reasoning_effort none, temperature kept", body)
	}
	if k, _ := body["chat_template_kwargs"].(map[string]any); k["enable_thinking"] != false {
		t.Errorf("chat_template_kwargs = %v", body["chat_template_kwargs"])
	}
	if resp.AppliedReasoning != nil {
		t.Errorf("AppliedReasoning = %+v, want nil: the mapped effort was overridden", resp.AppliedReasoning)
	}

	bad := newClient(t, func(http.ResponseWriter, *http.Request) { t.Fatal("must fail before any HTTP call") })
	_, err = bad.Complete(context.Background(), llms.ChatRequest{
		Messages:       []llms.Message{llms.UserText("q")},
		ProviderParams: map[llms.ProviderID]map[string]any{llms.ProviderOpenAICompat: {"bad": make(chan int)}},
	})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
		t.Fatalf("non-marshalable value: err = %v, want bad_request", err)
	}
}
