package vllm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

const modelsListJSON = `{
  "object":"list",
  "data":[
    {"id":"mistralai/Ministral-3-14B-Instruct-2512","object":"model","created":1780250790,"owned_by":"vllm",
     "root":"mistralai/Ministral-3-14B-Instruct-2512","parent":null,"max_model_len":32768}
  ]
}`

func TestListModels(t *testing.T) {
	c := newClient(t, respondJSON(t, 200, modelsListJSON))
	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	m := got[0]
	if m.ID != "mistralai/Ministral-3-14B-Instruct-2512" {
		t.Errorf("ID = %q", m.ID)
	}
	// Family is empty by design — see ADR-0015 (HuggingFace names have no
	// canonical family convention).
	if m.Family != "" {
		t.Errorf("Family = %q, want empty for vLLM", m.Family)
	}
	// Created is the model load time on the vLLM instance (not the
	// upstream release date), parsed from the `created` Unix-seconds field.
	want := time.Unix(1780250790, 0).UTC()
	if !m.Created.Equal(want) {
		t.Errorf("Created = %v, want %v (load time)", m.Created, want)
	}
	if m.Raw == nil {
		t.Error("Raw should carry the SDK payload")
	}
}

func TestListModelsAPIError(t *testing.T) {
	c := newClient(t, respondJSON(t, 401, `{"error":{"message":"unauthorized"}}`))
	_, err := c.ListModels(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *llms.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *llms.ProviderError, got %T: %v", err, err)
	}
	if pe.Kind != llms.ErrorKindAuth {
		t.Errorf("Kind = %q, want auth", pe.Kind)
	}
}
