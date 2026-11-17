package convert

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

var (
	png = []byte("\x89PNG\r\n\x1a\n-not-really")
	pdf = []byte("%PDF-1.7 not really")
)

func TestResultMapping(t *testing.T) {
	cases := []struct {
		name string
		in   *mcp.CallToolResult
		want toolloop.ToolResult
	}{
		{
			name: "single text",
			in:   &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hi"}}},
			want: toolloop.ToolResult{Content: "hi"},
		},
		{
			name: "texts joined",
			in:   &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "a"}, &mcp.TextContent{Text: "b"}}},
			want: toolloop.ToolResult{Content: "a\nb"},
		},
		{
			name: "empty text between texts dropped",
			in:   &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "a"}, &mcp.TextContent{}, &mcp.TextContent{Text: "b"}}},
			want: toolloop.ToolResult{Content: "a\nb"},
		},
		{
			name: "only empty text",
			in:   &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{}}},
			want: toolloop.ToolResult{},
		},
		{
			name: "no content",
			in:   &mcp.CallToolResult{},
			want: toolloop.ToolResult{},
		},
		{
			name: "isError carried",
			in:   &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "boom"}}},
			want: toolloop.ToolResult{Content: "boom", IsError: true},
		},
		{
			name: "embedded text resource",
			in: &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "file:///a.txt", MIMEType: "text/plain", Text: "file body"}},
			}},
			want: toolloop.ToolResult{Content: "file body"},
		},
		{
			name: "empty embedded text dropped",
			in: &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: "a"},
				&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "file:///empty"}},
			}},
			want: toolloop.ToolResult{Content: "a"},
		},
		{
			name: "resource link",
			in: &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.ResourceLink{URI: "file:///r.pdf", Name: "report", MIMEType: "application/pdf"},
				&mcp.ResourceLink{URI: "https://x.test/y"},
			}},
			want: toolloop.ToolResult{Content: "Resource link: report <file:///r.pdf> (application/pdf)\nResource link: <https://x.test/y>"},
		},
		{
			name: "image",
			in:   &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{MIMEType: "image/png", Data: png}}},
			want: toolloop.ToolResult{Parts: []llms.ContentPart{llms.Image("image/png", png)}},
		},
		{
			name: "text and image keep order, empty text dropped",
			in: &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{},
				&mcp.TextContent{Text: "see"},
				&mcp.ImageContent{MIMEType: "image/png", Data: png},
				&mcp.TextContent{},
				&mcp.TextContent{Text: "done"},
			}},
			want: toolloop.ToolResult{Parts: []llms.ContentPart{llms.Text("see"), llms.Image("image/png", png), llms.Text("done")}},
		},
		{
			name: "embedded blob document",
			in: &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "file:///r.pdf", MIMEType: "application/pdf", Blob: pdf}},
			}},
			want: toolloop.ToolResult{Parts: []llms.ContentPart{llms.Document("application/pdf", pdf)}},
		},
		{
			name: "embedded blob image",
			in: &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "file:///a.png", MIMEType: "IMAGE/PNG", Blob: png}},
			}},
			want: toolloop.ToolResult{Parts: []llms.ContentPart{llms.Image("IMAGE/PNG", png)}},
		},
		{
			name: "media with isError",
			in: &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
				&mcp.TextContent{Text: "partial"}, &mcp.ImageContent{MIMEType: "image/png", Data: png},
			}},
			want: toolloop.ToolResult{IsError: true, Parts: []llms.ContentPart{llms.Text("partial"), llms.Image("image/png", png)}},
		},
		{
			name: "structured content only",
			in:   &mcp.CallToolResult{StructuredContent: map[string]any{"n": 3}},
			want: toolloop.ToolResult{Content: `{"n":3}`},
		},
		{
			name: "structured content beside content not added",
			in:   &mcp.CallToolResult{StructuredContent: map[string]any{"n": 3}, Content: []mcp.Content{&mcp.TextContent{Text: "three"}}},
			want: toolloop.ToolResult{Content: "three"},
		},
		{
			name: "audio is a model-visible error",
			in: &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: "here"}, &mcp.AudioContent{MIMEType: "audio/wav", Data: []byte("RIFF")},
			}},
			want: toolloop.ToolResult{Content: "The tool returned audio content, which this client cannot pass to the model.", IsError: true},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Result(c.in)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Result =\n%#v\nwant\n%#v", got, c.want)
			}
			assertLoopAccepts(t, got)
		})
	}
}

func TestResultMalformed(t *testing.T) {
	cases := []struct {
		name string
		in   *mcp.CallToolResult
		want string
	}{
		{"nil", nil, "nil"},
		{"image without data", &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{MIMEType: "image/png"}}}, "no data"},
		{"image without MIME type", &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: png}}}, "no MIME type"},
		{"image with non-image type", &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{MIMEType: "application/pdf", Data: pdf}}}, "non-image"},
		{"embedded without contents", &mcp.CallToolResult{Content: []mcp.Content{&mcp.EmbeddedResource{}}}, "no contents"},
		{"blob without MIME type", &mcp.CallToolResult{Content: []mcp.Content{&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "u", Blob: pdf}}}}, "no MIME type"},
		// mcp.Content is sealed; the deprecated sampling-only blocks are the
		// only types the mapping doesn't handle.
		{"sampling-only content", &mcp.CallToolResult{Content: []mcp.Content{&mcp.ToolUseContent{}}}, "unsupported content type"}, //nolint:staticcheck // SA1019: deliberately the deprecated type
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Result(c.in)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Result error = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

// TestResultWireBlob decodes results from the wire, where an explicit empty
// blob and an absent one differ.
func TestResultWireBlob(t *testing.T) {
	decode := func(t *testing.T, js string) *mcp.CallToolResult {
		t.Helper()
		var r mcp.CallToolResult
		if err := json.Unmarshal([]byte(js), &r); err != nil {
			t.Fatalf("decode %s: %v", js, err)
		}
		return &r
	}
	t.Run("explicit empty blob is malformed", func(t *testing.T) {
		r := decode(t, `{"content":[{"type":"resource","resource":{"uri":"file:///r.pdf","mimeType":"application/pdf","blob":""}}]}`)
		if _, err := Result(r); err == nil || !strings.Contains(err.Error(), "no data") {
			t.Fatalf("Result error = %v, want a malformed-blob error", err)
		}
	})
	t.Run("absent blob is a text resource", func(t *testing.T) {
		r := decode(t, `{"content":[{"type":"resource","resource":{"uri":"file:///a.txt","text":"body"}}]}`)
		got, err := Result(r)
		if err != nil || got.Content != "body" {
			t.Fatalf("Result = %+v, %v", got, err)
		}
	})
	t.Run("blob document from the wire", func(t *testing.T) {
		r := decode(t, `{"content":[{"type":"resource","resource":{"uri":"file:///r.pdf","mimeType":"application/pdf","blob":"JVBERg=="}}]}`)
		got, err := Result(r)
		if err != nil || len(got.Parts) != 1 || got.Parts[0].Type != llms.ContentDocument || string(got.Parts[0].Media.Data) != "%PDF" {
			t.Fatalf("Result = %+v, %v", got, err)
		}
	})
}

// assertLoopAccepts runs r through toolloop.Run, so a result the loop's
// ToolResult check (ADR-0041) rejects fails here rather than in a consumer.
func assertLoopAccepts(t *testing.T, r toolloop.ToolResult) {
	t.Helper()
	fake := &testllm.FakeChatClient{Responses: []llms.ChatResponse{
		testllm.ToolCallResponse("c1", "t", `{}`),
		testllm.TextResponse("ok"),
	}}
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  fake,
		Request: llms.ChatRequest{Messages: []llms.Message{llms.UserText("go")}},
		Tools: []toolloop.Tool{{
			Definition: llms.ToolDefinition{Name: "t", InputSchema: json.RawMessage(objectSchema)},
			Execute: func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) {
				return r, nil
			},
		}},
	})
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("toolloop rejected the result: %s: %v", out.Kind, out.Err)
	}
}
