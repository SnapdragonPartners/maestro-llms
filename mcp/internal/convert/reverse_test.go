package convert

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

func TestServerSchema(t *testing.T) {
	ok := []struct{ in, want string }{
		{"", objectSchema},
		{"  \n", objectSchema},
		{`{"type":"object"}`, `{"type":"object"}`},
		// Served unchanged: numbers as written, refs not inlined.
		{`{"type":"object","properties":{"n":{"type":"integer","maximum":12345678901234567890}},"$defs":{"x":{}}}`,
			`{"type":"object","properties":{"n":{"type":"integer","maximum":12345678901234567890}},"$defs":{"x":{}}}`},
	}
	for _, c := range ok {
		got, err := ServerSchema(json.RawMessage(c.in))
		if err != nil {
			t.Errorf("ServerSchema(%q): %v", c.in, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("ServerSchema(%q) = %s, want %s", c.in, got, c.want)
		}
	}
	bad := map[string]string{
		`null`:                 "not a JSON object",
		`[]`:                   "not a JSON object",
		`"object"`:             "not a JSON object",
		`{"type":"object"`:     "not valid JSON",
		`{"type":"object"} {}`: "not valid JSON",
		`{"properties":{}}`:    `no top-level "type"`,
		`{"type":"array"}`:     `type is "array"`,
		`{"type":["object"]}`:  `type is ["object"]`,
		`{"type":null}`:        "type is null",
	}
	for in, want := range bad {
		_, err := ServerSchema(json.RawMessage(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ServerSchema(%s) error = %v, want it to contain %q", in, err, want)
		}
	}
}

func TestToMCP(t *testing.T) {
	cases := []struct {
		name string
		in   toolloop.ToolResult
		want *mcp.CallToolResult
	}{
		{"content", toolloop.ToolResult{Content: "hi"},
			&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hi"}}}},
		{"empty content is one empty text block", toolloop.ToolResult{},
			&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{}}}},
		{"is error", toolloop.ToolResult{Content: "nope", IsError: true},
			&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "nope"}}, IsError: true}},
		{"stop dropped", toolloop.ToolResult{Content: "done", Stop: true},
			&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}},
		{"parts", toolloop.ToolResult{Parts: []llms.ContentPart{
			llms.Text("a"),
			llms.Image("image/png", png),
			llms.Document("application/pdf; name=x", []byte("%PDF")),
		}, IsError: true}, &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: "a"},
			&mcp.ImageContent{MIMEType: "image/png", Data: png},
			&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "part:2", MIMEType: "application/pdf; name=x", Blob: []byte("%PDF")}},
		}, IsError: true}},
		{"empty parts slice is content", toolloop.ToolResult{Content: "x", Parts: []llms.ContentPart{}},
			&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "x"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ToMCP(&c.in)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("ToMCP = %#v, want %#v", got, c.want)
			}
		})
	}
}

func TestToMCPInvalid(t *testing.T) {
	media := func(typ llms.ContentPartType, mt string, data []byte) llms.ContentPart {
		return llms.ContentPart{Type: typ, Media: &llms.Media{MediaType: mt, Data: data}}
	}
	cases := map[string]struct {
		r    toolloop.ToolResult
		want string
	}{
		"content and parts":   {toolloop.ToolResult{Content: "x", Parts: []llms.ContentPart{llms.Text("y")}}, "both Content and Parts"},
		"empty text part":     {toolloop.ToolResult{Parts: []llms.ContentPart{llms.Text("")}}, "empty text part"},
		"other part type":     {toolloop.ToolResult{Parts: []llms.ContentPart{{Type: llms.ContentToolCall}}}, "not allowed"},
		"nil media":           {toolloop.ToolResult{Parts: []llms.ContentPart{{Type: llms.ContentImage}}}, "missing Media"},
		"empty data":          {toolloop.ToolResult{Parts: []llms.ContentPart{media(llms.ContentDocument, "application/pdf", nil)}}, "empty Media.Data"},
		"empty type":          {toolloop.ToolResult{Parts: []llms.ContentPart{media(llms.ContentImage, " ", png)}}, "empty Media.MediaType"},
		"no subtype":          {toolloop.ToolResult{Parts: []llms.ContentPart{media(llms.ContentImage, "png", png)}}, "invalid Media.MediaType"},
		"empty subtype":       {toolloop.ToolResult{Parts: []llms.ContentPart{media(llms.ContentImage, "image/", png)}}, "invalid Media.MediaType"},
		"bad params":          {toolloop.ToolResult{Parts: []llms.ContentPart{media(llms.ContentDocument, "application/pdf; =x", []byte("%PDF"))}}, "invalid Media.MediaType"},
		"image not image":     {toolloop.ToolResult{Parts: []llms.ContentPart{media(llms.ContentImage, "application/pdf", png)}}, "non-image media type"},
		"document is image":   {toolloop.ToolResult{Parts: []llms.ContentPart{media(llms.ContentDocument, "IMAGE/PNG", png)}}, "image media type"},
		"bad part after good": {toolloop.ToolResult{Parts: []llms.ContentPart{llms.Text("ok"), llms.Text("")}}, "part 1"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ToMCP(&c.r)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ToMCP error = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

// TestRoundTrip checks that Result inverts ToMCP for every valid shape, with
// Stop false (ADR-0050 §4), including over the JSON wire form.
func TestRoundTrip(t *testing.T) {
	cases := []toolloop.ToolResult{
		{Content: "hi"},
		{Content: ""},
		{Content: "multi\nline", IsError: true},
		{Parts: []llms.ContentPart{llms.Text("a"), llms.Image("image/png", png), llms.Document("application/pdf", []byte("%PDF")), llms.Text("b")}},
		{Parts: []llms.ContentPart{llms.Image("image/jpeg; q=1", png)}, IsError: true},
	}
	for i, want := range cases {
		res, err := ToMCP(&want)
		if err != nil {
			t.Fatalf("case %d: ToMCP: %v", i, err)
		}
		b, err := json.Marshal(res)
		if err != nil {
			t.Fatalf("case %d: marshal: %v", i, err)
		}
		var wire mcp.CallToolResult
		if err := json.Unmarshal(b, &wire); err != nil {
			t.Fatalf("case %d: unmarshal: %v", i, err)
		}
		for _, r := range []*mcp.CallToolResult{res, &wire} {
			got, err := Result(r)
			if err != nil {
				t.Fatalf("case %d: Result: %v", i, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("case %d: round trip = %#v, want %#v", i, got, want)
			}
		}
	}
	// Stop does not survive: nothing on the MCP side honors it.
	got, err := Result(must(ToMCP(&toolloop.ToolResult{Content: "x", Stop: true})))
	if err != nil || got.Stop {
		t.Fatalf("Stop round trip = %#v, %v; want Stop false", got, err)
	}
}

func must(r *mcp.CallToolResult, err error) *mcp.CallToolResult {
	if err != nil {
		panic(err)
	}
	return r
}
