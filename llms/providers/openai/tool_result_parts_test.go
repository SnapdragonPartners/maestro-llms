package openai

import (
	"context"
	"encoding/base64"
	"errors"
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

func TestToolResultParts(t *testing.T) {
	img, pdf := []byte("\x89PNG fake"), []byte("%PDF-1.7 fake")
	items := captureInput(t, toolResultTranscript(llms.ToolResult{Parts: []llms.ContentPart{
		llms.Text("here it is"),
		llms.Image("image/png", img),
		llms.Document("application/pdf", pdf),
	}}))
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	out := items[2]
	if out["type"] != "function_call_output" || out["call_id"] != "c1" {
		t.Fatalf("item = %v, want function_call_output for c1", out)
	}
	list, _ := out["output"].([]any)
	if len(list) != 3 {
		t.Fatalf("output = %v, want 3 items", out["output"])
	}
	o0, _ := list[0].(map[string]any)
	o1, _ := list[1].(map[string]any)
	o2, _ := list[2].(map[string]any)
	if o0["type"] != "input_text" || o0["text"] != "here it is" {
		t.Fatalf("output 0 = %v", o0)
	}
	if o1["type"] != "input_image" || o1["detail"] != "auto" ||
		o1["image_url"] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(img) {
		t.Fatalf("output 1 = %v", o1)
	}
	if o2["type"] != "input_file" || o2["filename"] != documentFilename ||
		o2["file_data"] != "data:application/pdf;base64,"+base64.StdEncoding.EncodeToString(pdf) {
		t.Fatalf("output 2 = %v", o2)
	}
}

// TestToolResultContentUnchanged: a Content-only result is still a string.
func TestToolResultContentUnchanged(t *testing.T) {
	items := captureInput(t, toolResultTranscript(llms.ToolResult{Content: "plain"}))
	if items[2]["output"] != "plain" || items[2]["call_id"] != "c1" {
		t.Fatalf("item = %v", items[2])
	}
}

func TestToolResultPartsRejected(t *testing.T) {
	for name, r := range map[string]llms.ToolResult{
		"content and parts": {Content: "x", Parts: []llms.ContentPart{llms.Text("y")}},
		"unsupported image": {Parts: []llms.ContentPart{llms.Image("image/bmp", []byte("BM"))}},
		"unsupported doc":   {Parts: []llms.ContentPart{llms.Document("text/plain", []byte("notes"))}},
		"reasoning part": {Parts: []llms.ContentPart{{
			Type: llms.ContentReasoning, Reasoning: &llms.Reasoning{Provider: llms.ProviderOpenAI},
		}}},
	} {
		t.Run(name, func(t *testing.T) {
			c := newChat(t, func(http.ResponseWriter, *http.Request) {
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
