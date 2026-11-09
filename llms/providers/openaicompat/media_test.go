package openaicompat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

func TestMediaImageURLParts(t *testing.T) {
	var captured map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, textCompletionJSON)
	})
	img := []byte("\x89PNG fake")
	_, err := c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{
		{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Text("what is"), llms.Image("image/png", img)}},
		llms.AssistantText("a cat"),
		llms.UserText("thanks"),
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	msgs, _ := captured["messages"].([]any)
	m0, _ := msgs[0].(map[string]any)
	content, _ := m0["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("media message content = %v, want 2 parts", m0["content"])
	}
	p0, _ := content[0].(map[string]any)
	p1, _ := content[1].(map[string]any)
	iu, _ := p1["image_url"].(map[string]any)
	if p0["type"] != "text" || p0["text"] != "what is" || p1["type"] != "image_url" ||
		iu["url"] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(img) {
		t.Fatalf("content parts = %v", content)
	}
	// A text-only user message keeps the plain-string form.
	m2, _ := msgs[2].(map[string]any)
	if m2["content"] != "thanks" {
		t.Fatalf("text-only user message changed shape: %v", m2)
	}
}

func TestMediaRejectedBeforeCall(t *testing.T) {
	cases := map[string][]llms.Message{
		"document":   {{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Document("application/pdf", []byte("x"))}}},
		"empty data": {{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Image("image/png", nil)}}},
		"media on assistant": {
			llms.UserText("q"),
			{Role: llms.RoleAssistant, Content: []llms.ContentPart{llms.Text("a"), llms.Image("image/png", []byte("x"))}},
		},
	}
	for name, msgs := range cases {
		t.Run(name, func(t *testing.T) {
			called := false
			c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, textCompletionJSON)
			})
			_, err := c.Complete(context.Background(), llms.ChatRequest{Messages: msgs})
			var pe *llms.ProviderError
			if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
				t.Fatalf("err = %v, want bad_request ProviderError", err)
			}
			if called {
				t.Fatal("provider was called")
			}
		})
	}
}
