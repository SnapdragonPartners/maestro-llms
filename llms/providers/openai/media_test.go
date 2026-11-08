package openai

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

// captureInput sends msgs and returns the wire "input" items.
func captureInput(t *testing.T, msgs []llms.Message) []map[string]any {
	t.Helper()
	var captured map[string]any
	c := newChat(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respTextJSON)
	})
	if _, err := c.Complete(context.Background(), llms.ChatRequest{Messages: msgs}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	raw, _ := captured["input"].([]any)
	out := make([]map[string]any, len(raw))
	for i := range raw {
		out[i], _ = raw[i].(map[string]any)
	}
	return out
}

// onlyContent returns the single content item of a user message input item.
func onlyContent(t *testing.T, item map[string]any) map[string]any {
	t.Helper()
	if item["role"] != "user" {
		t.Fatalf("item role = %v, want user: %v", item["role"], item)
	}
	content, _ := item["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v, want one item", item["content"])
	}
	c, _ := content[0].(map[string]any)
	return c
}

func TestMediaInputImageAndFile(t *testing.T) {
	img := []byte("\x89PNG fake")
	pdf := []byte("%PDF-1.7 fake")
	items := captureInput(t, []llms.Message{{Role: llms.RoleUser, Content: []llms.ContentPart{
		llms.Image("image/PNG", img),
		llms.Document("application/pdf", pdf),
		llms.Text("summarize"),
	}}})
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3 (order kept)", len(items))
	}
	ci := onlyContent(t, items[0])
	if ci["type"] != "input_image" || ci["detail"] != "auto" ||
		ci["image_url"] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(img) {
		t.Fatalf("input_image = %v", ci)
	}
	cf := onlyContent(t, items[1])
	if cf["type"] != "input_file" || cf["filename"] != documentFilename ||
		cf["file_data"] != "data:application/pdf;base64,"+base64.StdEncoding.EncodeToString(pdf) {
		t.Fatalf("input_file = %v", cf)
	}
	if items[2]["content"] != "summarize" {
		t.Fatalf("text item = %v", items[2])
	}
}

func TestMediaRejectedBeforeCall(t *testing.T) {
	cases := map[string][]llms.Message{
		"unsupported image type": {{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Image("image/heic", []byte("x"))}}},
		"text document":          {{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Document("text/plain", []byte("x"))}}},
		"empty data":             {{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Document("application/pdf", nil)}}},
		"media on assistant": {
			llms.UserText("q"),
			{Role: llms.RoleAssistant, Content: []llms.ContentPart{llms.Image("image/png", []byte("x"))}},
		},
	}
	for name, msgs := range cases {
		t.Run(name, func(t *testing.T) {
			called := false
			c := newChat(t, func(w http.ResponseWriter, _ *http.Request) {
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
