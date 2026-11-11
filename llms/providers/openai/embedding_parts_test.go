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

// ADR-0045 item 7: text Parts are joined by a blank line into one input
// string; Text inputs are unchanged.
func TestEmbedPartsJoinText(t *testing.T) {
	var rawBody []byte
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","model":"m","data":[
 {"object":"embedding","index":0,"embedding":[1]},
 {"object":"embedding","index":1,"embedding":[2]}],"usage":{"prompt_tokens":3,"total_tokens":3}}`)
	})
	resp, err := c.Embed(context.Background(), llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{
		{ID: "p", Parts: []llms.ContentPart{llms.Text("a"), llms.Text("b")}},
		{ID: "t", Text: "c"},
	}})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	var got struct {
		Input []string `json:"input"`
	}
	if err := json.Unmarshal(rawBody, &got); err != nil {
		t.Fatalf("decode request: %v (%s)", err, rawBody)
	}
	if len(got.Input) != 2 || got.Input[0] != "a\n\nb" || got.Input[1] != "c" {
		t.Fatalf("wire input = %q, want [\"a\\n\\nb\" \"c\"]", got.Input)
	}
	if len(resp.Vectors) != 2 || resp.Vectors[0].ID != "p" {
		t.Fatalf("vectors = %+v", resp.Vectors)
	}
}

// Media and malformed Parts are bad_request before any network call.
func TestEmbedPartsRejectedPreCall(t *testing.T) {
	cases := map[string]llms.EmbeddingInput{
		"image":    {Parts: []llms.ContentPart{llms.Text("a"), llms.Image("image/png", []byte("x"))}},
		"document": {Parts: []llms.ContentPart{llms.Document("application/pdf", []byte("x"))}},
		"both":     {Text: "a", Parts: []llms.ContentPart{llms.Text("b")}},
		"empty":    {Parts: []llms.ContentPart{llms.Text("")}},
	}
	for name, in := range cases {
		called := false
		c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusInternalServerError)
		})
		_, err := c.Embed(context.Background(), llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{{Text: "ok"}, in}})
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
			t.Fatalf("%s: want bad_request, got %v", name, err)
		}
		if called {
			t.Fatalf("%s: provider called", name)
		}
	}
}
