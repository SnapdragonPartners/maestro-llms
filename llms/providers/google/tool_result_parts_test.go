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

// toolResultTranscript is a user turn, one tool call and its result r.
func toolResultTranscript(r llms.ToolResult) []llms.Message {
	r.ToolCallID = "c1"
	return []llms.Message{
		llms.UserText("q"),
		{Role: llms.RoleAssistant, Content: []llms.ContentPart{{
			Type: llms.ContentToolCall, ToolCall: &llms.ToolCall{ID: "c1", Name: "get_document", Parameters: []byte(`{}`)},
		}}},
		llms.ToolResultMessage(r),
	}
}

// sentFunctionResponse returns the functionResponse sent for r.
func sentFunctionResponse(t *testing.T, r llms.ToolResult) map[string]any {
	t.Helper()
	var captured map[string]any
	c := newClient(t, func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(b, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respTextJSON)
	})
	if _, err := c.Complete(context.Background(), llms.ChatRequest{Messages: toolResultTranscript(r)}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	contents, _ := captured["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents = %v, want 3", contents)
	}
	c2, _ := contents[2].(map[string]any)
	parts, _ := c2["parts"].([]any)
	p0, _ := parts[0].(map[string]any)
	fr, _ := p0["functionResponse"].(map[string]any)
	if fr == nil || fr["name"] != "get_document" {
		t.Fatalf("part = %v, want functionResponse for get_document", p0)
	}
	return fr
}

func TestToolResultParts(t *testing.T) {
	img, pdf := []byte("\x89PNG fake"), []byte("%PDF-1.7 fake")
	fr := sentFunctionResponse(t, llms.ToolResult{
		Parts: []llms.ContentPart{
			llms.Text("first"),
			llms.Image("image/png", img),
			llms.Text("second"),
			llms.Document("Application/PDF", pdf),
		},
		IsError: true,
	})
	resp, _ := fr["response"].(map[string]any)
	if resp["content"] != "first\n\nsecond" || resp["is_error"] != true {
		t.Fatalf("response = %v, want joined text and is_error", resp)
	}
	parts, _ := fr["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("parts = %v, want 2", fr["parts"])
	}
	for i, want := range []struct {
		mime string
		data []byte
	}{{"image/png", img}, {"application/pdf", pdf}} {
		p, _ := parts[i].(map[string]any)
		inline, _ := p["inlineData"].(map[string]any)
		if inline["mimeType"] != want.mime || inline["data"] != base64.StdEncoding.EncodeToString(want.data) {
			t.Fatalf("part %d = %v, want %s", i, p, want.mime)
		}
	}
}

// TestToolResultContentUnchanged: a Content-only result has no parts.
func TestToolResultContentUnchanged(t *testing.T) {
	fr := sentFunctionResponse(t, llms.ToolResult{Content: "plain"})
	resp, _ := fr["response"].(map[string]any)
	if resp["content"] != "plain" || resp["is_error"] != false {
		t.Fatalf("response = %v", resp)
	}
	if _, ok := fr["parts"]; ok {
		t.Fatalf("parts sent for a Content-only result: %v", fr)
	}
}

func TestToolResultPartsRejected(t *testing.T) {
	for name, r := range map[string]llms.ToolResult{
		"content and parts": {Content: "x", Parts: []llms.ContentPart{llms.Text("y")}},
		"empty media":       {Parts: []llms.ContentPart{llms.Document("application/pdf", nil)}},
		"bad media type":    {Parts: []llms.ContentPart{llms.Image("png", []byte("x"))}},
	} {
		t.Run(name, func(t *testing.T) {
			c := newClient(t, func(http.ResponseWriter, *http.Request) {
				t.Error("request sent; want a pre-call rejection")
			})
			_, err := c.Complete(context.Background(), llms.ChatRequest{Messages: toolResultTranscript(r)})
			var pe *llms.ProviderError
			if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
				t.Fatalf("err = %v, want bad_request", err)
			}
		})
	}
}
