package anthropic

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

// captureBlocks sends one user message and returns its wire content blocks.
func captureBlocks(t *testing.T, parts ...llms.ContentPart) []map[string]any {
	t.Helper()
	sent := captureMessages(t, []llms.Message{{Role: llms.RoleUser, Content: parts}})
	if len(sent) != 1 {
		t.Fatalf("messages = %d, want 1", len(sent))
	}
	m, _ := sent[0].(map[string]any)
	raw, _ := m["content"].([]any)
	out := make([]map[string]any, len(raw))
	for i := range raw {
		out[i], _ = raw[i].(map[string]any)
	}
	return out
}

func TestMediaImageBlock(t *testing.T) {
	data := []byte("\x89PNG fake")
	blocks := captureBlocks(t, llms.Image("IMAGE/PNG; x=y", data), llms.Text("what is this?"))
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(blocks))
	}
	if blocks[0]["type"] != "image" {
		t.Fatalf("block 0 type = %v, want image", blocks[0]["type"])
	}
	src, _ := blocks[0]["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("image source = %v", src)
	}
	if blocks[1]["type"] != "text" {
		t.Fatalf("block order changed: %v", blocks)
	}
}

func TestMediaDocumentBlocks(t *testing.T) {
	pdf := []byte("%PDF-1.7 fake")
	blocks := captureBlocks(t,
		llms.Document("application/pdf", pdf),
		llms.Document("text/plain; charset=utf-8", []byte("plain notes")),
	)
	src0, _ := blocks[0]["source"].(map[string]any)
	if blocks[0]["type"] != "document" || src0["type"] != "base64" || src0["media_type"] != "application/pdf" ||
		src0["data"] != base64.StdEncoding.EncodeToString(pdf) {
		t.Fatalf("pdf block = %v", blocks[0])
	}
	src1, _ := blocks[1]["source"].(map[string]any)
	if blocks[1]["type"] != "document" || src1["type"] != "text" || src1["media_type"] != "text/plain" || src1["data"] != "plain notes" {
		t.Fatalf("text document block = %v", blocks[1])
	}
}

func TestMediaCacheBreakpoint(t *testing.T) {
	doc := llms.Document("application/pdf", []byte("%PDF"))
	doc.CacheBreakpoint = true
	blocks := captureBlocks(t, doc, llms.Text("q"))
	if blocks[0]["cache_control"] == nil {
		t.Fatalf("document cache breakpoint not mapped: %v", blocks[0])
	}
	if blocks[1]["cache_control"] != nil {
		t.Fatalf("unmarked text got cache_control: %v", blocks[1])
	}
}

// Media breakpoints count toward Anthropic's four-marker cap, and the last
// ones in order win.
func TestMediaCacheBreakpointCapped(t *testing.T) {
	var parts []llms.ContentPart
	for range 3 {
		p := llms.Image("image/png", []byte("img"))
		p.CacheBreakpoint = true
		parts = append(parts, p)
	}
	for range 2 {
		parts = append(parts, llms.ContentPart{Type: llms.ContentText, Text: "t", CacheBreakpoint: true})
	}
	blocks := captureBlocks(t, parts...)
	n := 0
	for i, b := range blocks {
		if b["cache_control"] != nil {
			n++
		} else if i != 0 {
			t.Fatalf("block %d lost its marker; only the earliest should drop", i)
		}
	}
	if n != 4 {
		t.Fatalf("cache_control markers = %d, want 4", n)
	}
}

func TestMediaRejectedBeforeCall(t *testing.T) {
	cases := map[string][]llms.Message{
		"unsupported image type": {{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Image("image/bmp", []byte("x"))}}},
		"unsupported document":   {{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Document("application/msword", []byte("x"))}}},
		"invalid utf-8 text":     {{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Document("text/plain", []byte{0xff, 0xfe})}}},
		"empty data":             {{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Image("image/png", nil)}}},
		"nil media":              {{Role: llms.RoleUser, Content: []llms.ContentPart{{Type: llms.ContentImage}}}},
		"image typed document":   {{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Document("image/png", []byte("x"))}}},
		"media on assistant": {
			llms.UserText("q"),
			{Role: llms.RoleAssistant, Content: []llms.ContentPart{llms.Text("a"), llms.Image("image/png", []byte("x"))}},
			llms.UserText("next"),
		},
	}
	for name, msgs := range cases {
		t.Run(name, func(t *testing.T) {
			called := false
			c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				called = true
				_, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{})
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
