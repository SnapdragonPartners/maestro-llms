package ollama

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

func TestMediaImages(t *testing.T) {
	var captured map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respTextJSON)
	})
	a, b := []byte("img-a"), []byte("img-b")
	_, err := c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{
		{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Image("image/png", a), llms.Text("compare"), llms.Image("image/jpeg", b)}},
		// An image-only user message is still sent.
		{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Image("image/png", a)}},
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	msgs, _ := captured["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v, want 2", msgs)
	}
	m0, _ := msgs[0].(map[string]any)
	imgs, _ := m0["images"].([]any)
	if m0["content"] != "compare" || len(imgs) != 2 ||
		imgs[0] != base64.StdEncoding.EncodeToString(a) || imgs[1] != base64.StdEncoding.EncodeToString(b) {
		t.Fatalf("message 0 = %v", m0)
	}
	m1, _ := msgs[1].(map[string]any)
	if imgs1, _ := m1["images"].([]any); len(imgs1) != 1 {
		t.Fatalf("image-only message = %v", m1)
	}
}

func TestMediaRejectedBeforeCall(t *testing.T) {
	cases := map[string][]llms.Message{
		"document":   {{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Document("application/pdf", []byte("x"))}}},
		"empty data": {{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Image("image/png", nil)}}},
		"media on assistant": {
			llms.UserText("q"),
			{Role: llms.RoleAssistant, Content: []llms.ContentPart{llms.Image("image/png", []byte("x"))}},
		},
	}
	for name, msgs := range cases {
		t.Run(name, func(t *testing.T) {
			called := false
			c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, respTextJSON)
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
