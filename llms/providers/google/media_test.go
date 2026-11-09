package google

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

func TestMediaInlineData(t *testing.T) {
	var captured map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respTextJSON)
	})
	img := []byte("\x89PNG fake")
	pdf := []byte("%PDF-1.7 fake")
	_, err := c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{{
		Role: llms.RoleUser,
		Content: []llms.ContentPart{
			llms.Image("image/png", img),
			llms.Document("Application/PDF; v=1", pdf),
			llms.Text("q"),
		},
	}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	contents, _ := captured["contents"].([]any)
	c0, _ := contents[0].(map[string]any)
	parts, _ := c0["parts"].([]any)
	if len(parts) != 3 {
		t.Fatalf("parts = %v, want 3", parts)
	}
	for i, want := range []struct {
		mime string
		data []byte
	}{{"image/png", img}, {"application/pdf", pdf}} {
		p, _ := parts[i].(map[string]any)
		inline, _ := p["inlineData"].(map[string]any)
		if inline["mimeType"] != want.mime || inline["data"] != base64.StdEncoding.EncodeToString(want.data) {
			t.Fatalf("part %d inlineData = %v", i, p)
		}
	}
}

func TestMediaRejectedBeforeCall(t *testing.T) {
	cases := map[string][]llms.Message{
		"empty data":           {{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Image("image/png", nil)}}},
		"bad media type":       {{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Document("not a type", []byte("x"))}}},
		"non-image image part": {{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Image("application/pdf", []byte("x"))}}},
		"media on tool message": {
			llms.UserText("q"),
			{Role: llms.RoleAssistant, Content: []llms.ContentPart{{Type: llms.ContentToolCall, ToolCall: &llms.ToolCall{ID: "1", Name: "f"}}}},
			{Role: llms.RoleTool, Content: []llms.ContentPart{llms.Image("image/png", []byte("x"))}},
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
