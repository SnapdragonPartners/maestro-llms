package middleware

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
)

func countingEmbedFake(calls *int32) *testllm.FakeEmbeddingClient {
	return &testllm.FakeEmbeddingClient{
		ModelRef: llms.ModelRef{Provider: "p", Name: "e"},
		Dims:     3,
		Func: func(_ context.Context, req llms.EmbeddingRequest) (llms.EmbeddingResponse, error) {
			atomic.AddInt32(calls, 1)
			return llms.EmbeddingResponse{Vectors: make([]llms.EmbeddingVector, len(req.Inputs))}, nil
		},
	}
}

func TestValidationEmbeddingsValid(t *testing.T) {
	var calls int32
	c := ValidationEmbeddings()(countingEmbedFake(&calls))
	if c.Model().Name != "e" || c.DefaultDimensions() != 3 {
		t.Fatalf("Model/DefaultDimensions not forwarded: %+v %d", c.Model(), c.DefaultDimensions())
	}
	req := llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{
		{ID: "t", Text: "plain"},
		{ID: "e"}, // neither Text nor Parts: the provider decides
		{ID: "m", Title: "T", Parts: []llms.ContentPart{
			llms.Text("caption"), llms.Image("image/png", []byte("x")), llms.Document("application/pdf; v=1", []byte("x")),
		}},
	}}
	if _, err := c.Embed(context.Background(), req); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if calls != 1 {
		t.Fatalf("inner client calls = %d, want 1", calls)
	}
}

func TestValidationEmbeddingsRejections(t *testing.T) {
	cases := map[string]llms.EmbeddingInput{
		"both":       {Text: "x", Parts: []llms.ContentPart{llms.Text("y")}},
		"empty text": {Parts: []llms.ContentPart{llms.Text("")}},
		"bad media":  {Parts: []llms.ContentPart{llms.Image("application/pdf", []byte("x"))}},
		"no media":   {Parts: []llms.ContentPart{{Type: llms.ContentImage}}},
		"reasoning":  {Parts: []llms.ContentPart{{Type: llms.ContentReasoning, Reasoning: &llms.Reasoning{}}}},
	}
	for name, in := range cases {
		var calls int32
		c := ValidationEmbeddings()(countingEmbedFake(&calls))
		_, err := c.Embed(context.Background(), llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{{Text: "ok"}, in}})
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("%s: want *ValidationError, got %v", name, err)
		}
		if !strings.HasPrefix(err.Error(), "invalid embedding request: input 1: ") {
			t.Errorf("%s: message %q lacks the embedding prefix and input index", name, err)
		}
		if llms.Retryable(err) {
			t.Errorf("%s: a validation error must not be retryable", name)
		}
		if calls != 0 {
			t.Errorf("%s: inner client called", name)
		}
	}
}

// A ValidationError built by a caller keeps the chat prefix (ADR-0045 item 6).
func TestValidationErrorPrefix(t *testing.T) {
	if got := (&ValidationError{Reason: "r"}).Error(); got != "invalid chat request: r" {
		t.Fatalf("caller-built ValidationError = %q", got)
	}
}
