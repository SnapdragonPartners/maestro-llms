package google

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
			llms.ProviderGoogle:    {"generationConfig": map[string]any{"maxOutputTokens": 50, "thinkingConfig": map[string]any{"thinkingLevel": "LOW"}}},
			llms.ProviderAnthropic: {"zzz": 1},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, ok := body["zzz"]; ok {
		t.Error("another provider's params reached the wire")
	}
	gc, _ := body["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"] != float64(50) || gc["temperature"] == nil {
		t.Errorf("generationConfig = %v, want maxOutputTokens 50 with the mapped temperature kept (deep merge)", gc)
	}
	if tc, _ := gc["thinkingConfig"].(map[string]any); tc["thinkingLevel"] != "LOW" {
		t.Errorf("thinkingConfig = %v, want thinkingLevel LOW", gc["thinkingConfig"])
	}
	if resp.AppliedReasoning != nil {
		t.Errorf("AppliedReasoning = %+v, want nil: the mapped effort was overridden", resp.AppliedReasoning)
	}

	bad := newClient(t, func(http.ResponseWriter, *http.Request) { t.Fatal("must fail before any HTTP call") })
	_, err = bad.Complete(context.Background(), llms.ChatRequest{
		Messages:       []llms.Message{llms.UserText("q")},
		ProviderParams: map[llms.ProviderID]map[string]any{llms.ProviderGoogle: {"bad": make(chan int)}},
	})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
		t.Fatalf("non-marshalable value: err = %v, want bad_request", err)
	}
}
