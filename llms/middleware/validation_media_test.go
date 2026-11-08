package middleware

import (
	"context"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

func TestValidationMediaValid(t *testing.T) {
	var calls int32
	c := ValidationChat()(countingFake(&calls))
	req := llms.ChatRequest{Messages: []llms.Message{{Role: llms.RoleUser, Content: []llms.ContentPart{
		llms.Image("image/png", []byte("x")),
		llms.Document("application/pdf; v=1", []byte("x")),
	}}}}
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("media-only user message rejected: %v", err)
	}
	if calls != 1 {
		t.Fatalf("inner client calls = %d, want 1", calls)
	}
}

func TestValidationMediaRejections(t *testing.T) {
	var calls int32
	user := func(p llms.ContentPart) llms.ChatRequest {
		return llms.ChatRequest{Messages: []llms.Message{{Role: llms.RoleUser, Content: []llms.ContentPart{p}}}}
	}
	img := llms.Image("image/png", []byte("x"))
	cases := []struct {
		req  llms.ChatRequest
		name string
		want string
	}{
		{name: "nil media", req: user(llms.ContentPart{Type: llms.ContentDocument}), want: "missing Media"},
		{name: "empty data", req: user(llms.Image("image/png", nil)), want: "empty Media.Data"},
		{name: "empty type", req: user(llms.Document("", []byte("x"))), want: "empty Media.MediaType"},
		{name: "image with document type", req: user(llms.Image("application/pdf", []byte("x"))), want: "non-image media type"},
		{name: "document with image type", req: user(llms.Document("image/png", []byte("x"))), want: "use an image part"},
		{name: "media in system", req: llms.ChatRequest{System: []llms.ContentPart{img}, Messages: []llms.Message{llms.UserText("q")}}, want: "only text is allowed"},
		{name: "media on assistant", req: llms.ChatRequest{Messages: []llms.Message{
			llms.UserText("q"),
			{Role: llms.RoleAssistant, Content: []llms.ContentPart{llms.Text("a"), img}},
		}}, want: "only allowed on user messages"},
		{name: "media on tool", req: llms.ChatRequest{Messages: []llms.Message{
			llms.UserText("q"),
			{Role: llms.RoleAssistant, Content: []llms.ContentPart{{Type: llms.ContentToolCall, ToolCall: &llms.ToolCall{ID: "a", Name: "f"}}}},
			{Role: llms.RoleTool, Content: []llms.ContentPart{img}},
		}}, want: "only allowed on user messages"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { mustReject(t, &calls, c.req, c.want) })
	}
}
