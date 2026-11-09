package anthropic

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// toolResultTranscript is a user turn, one tool call and its result r.
func toolResultTranscript(r llms.ToolResult, user ...llms.ContentPart) []llms.Message {
	if len(user) == 0 {
		user = []llms.ContentPart{llms.Text("q")}
	}
	r.ToolCallID = "c1"
	return []llms.Message{
		{Role: llms.RoleUser, Content: user},
		{Role: llms.RoleAssistant, Content: []llms.ContentPart{{
			Type: llms.ContentToolCall, ToolCall: &llms.ToolCall{ID: "c1", Name: "get_document", Parameters: []byte(`{}`)},
		}}},
		llms.ToolResultMessage(r),
	}
}

// sentToolResult returns the tool_result block sent for msgs' last message.
func sentToolResult(t *testing.T, msgs []llms.Message) map[string]any {
	t.Helper()
	sent := captureMessages(t, msgs)
	if len(sent) != 3 {
		t.Fatalf("messages = %d, want 3", len(sent))
	}
	m, _ := sent[2].(map[string]any)
	content, _ := m["content"].([]any)
	b, _ := content[0].(map[string]any)
	if b["type"] != "tool_result" || b["tool_use_id"] != "c1" {
		t.Fatalf("block = %v, want tool_result for c1", b)
	}
	return b
}

func TestToolResultParts(t *testing.T) {
	img, pdf := []byte("\x89PNG fake"), []byte("%PDF-1.7 fake")
	b := sentToolResult(t, toolResultTranscript(llms.ToolResult{
		Parts: []llms.ContentPart{
			llms.Text("here it is"),
			llms.Image("image/png", img),
			llms.Document("application/pdf", pdf),
		},
		IsError: true,
	}))
	if b["is_error"] != true {
		t.Fatalf("is_error = %v, want true", b["is_error"])
	}
	content, _ := b["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content = %v, want 3 blocks", content)
	}
	c0, _ := content[0].(map[string]any)
	c1, _ := content[1].(map[string]any)
	c2, _ := content[2].(map[string]any)
	if c0["type"] != "text" || c0["text"] != "here it is" {
		t.Fatalf("content 0 = %v", c0)
	}
	src1, _ := c1["source"].(map[string]any)
	if c1["type"] != "image" || src1["media_type"] != "image/png" || src1["data"] != base64.StdEncoding.EncodeToString(img) {
		t.Fatalf("content 1 = %v", c1)
	}
	src2, _ := c2["source"].(map[string]any)
	if c2["type"] != "document" || src2["media_type"] != "application/pdf" || src2["data"] != base64.StdEncoding.EncodeToString(pdf) {
		t.Fatalf("content 2 = %v", c2)
	}
	for i, c := range []map[string]any{c0, c1, c2} {
		if _, ok := c["cache_control"]; ok {
			t.Fatalf("content %d has cache_control without a breakpoint: %v", i, c)
		}
	}
}

// TestToolResultContentUnchanged: a Content-only result is sent as before.
func TestToolResultContentUnchanged(t *testing.T) {
	b := sentToolResult(t, toolResultTranscript(llms.ToolResult{Content: "plain"}))
	content, _ := b["content"].([]any)
	c0, _ := content[0].(map[string]any)
	if len(content) != 1 || c0["type"] != "text" || c0["text"] != "plain" || b["is_error"] != false {
		t.Fatalf("block = %v", b)
	}
}

// TestToolResultPartCacheBreakpoint: a marker on a tool result part is
// honored and counts toward the four-marker cap, in conversation order.
func TestToolResultPartCacheBreakpoint(t *testing.T) {
	marked := func(s string) llms.ContentPart {
		p := llms.Text(s)
		p.CacheBreakpoint = true
		return p
	}
	doc := llms.Document("application/pdf", []byte("%PDF-1.7 fake"))
	doc.CacheBreakpoint = true
	msgs := toolResultTranscript(llms.ToolResult{Parts: []llms.ContentPart{doc}},
		marked("a"), marked("b"), marked("c"), marked("d"))
	sent := captureMessages(t, msgs)
	user, _ := sent[0].(map[string]any)
	ublocks, _ := user["content"].([]any)
	for i, want := range []bool{false, true, true, true} {
		blk, _ := ublocks[i].(map[string]any)
		if _, got := blk["cache_control"]; got != want {
			t.Fatalf("user block %d cache_control = %v, want %v (five markers, earliest dropped)", i, got, want)
		}
	}
	tr, _ := sent[2].(map[string]any)
	tblocks, _ := tr["content"].([]any)
	res, _ := tblocks[0].(map[string]any)
	content, _ := res["content"].([]any)
	d, _ := content[0].(map[string]any)
	if _, ok := d["cache_control"]; !ok {
		t.Fatalf("tool result document lost its cache_control: %v", d)
	}
}

func TestToolResultPartsRejected(t *testing.T) {
	for name, r := range map[string]llms.ToolResult{
		"content and parts": {Content: "x", Parts: []llms.ContentPart{llms.Text("y")}},
		"unsupported image": {Parts: []llms.ContentPart{llms.Image("image/bmp", []byte("BM"))}},
		"unsupported doc":   {Parts: []llms.ContentPart{llms.Document("application/zip", []byte("PK"))}},
		"empty text":        {Parts: []llms.ContentPart{llms.Text("")}},
		"nested tool call": {Parts: []llms.ContentPart{{
			Type: llms.ContentToolCall, ToolCall: &llms.ToolCall{ID: "x", Name: "y"},
		}}},
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
