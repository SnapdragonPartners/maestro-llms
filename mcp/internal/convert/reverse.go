package convert

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

// ServerSchema checks a served tool's input schema (ADR-0050 §2) and returns
// what to serve: {"type":"object"} for an empty schema, otherwise the schema
// unchanged. It must be a JSON object whose top-level type is "object"; an
// object with no type is an error, not rewritten.
func ServerSchema(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage(objectSchema), nil
	}
	v, err := decodeJSON(raw)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("input schema is not a JSON object")
	}
	typ, present := m["type"]
	switch {
	case !present:
		return nil, errors.New(`input schema has no top-level "type"; MCP requires "object"`)
	case typ != "object":
		return nil, fmt.Errorf(`input schema's top-level type is %s; MCP requires "object"`, compact(typ))
	}
	return raw, nil
}

func compact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// DocumentURI is the synthetic URI of the embedded resource that carries
// the document part at index i (ADR-0050 §4). MCP requires a URI; this one
// names the part's position and is not meant to be fetched.
func DocumentURI(i int) string { return "part:" + strconv.Itoa(i) }

// ToMCP maps a tool's result to an MCP call result (ADR-0050 §4), the
// reverse of Result. An error means the result is one the loop would reject
// (ADR-0041); the server treats it like an Execute error. Stop is dropped.
func ToMCP(r *toolloop.ToolResult) (*mcp.CallToolResult, error) {
	if len(r.Parts) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: r.Content}},
			IsError: r.IsError,
		}, nil
	}
	if r.Content != "" {
		return nil, errors.New("invalid tool result: both Content and Parts are set; set one")
	}
	content := make([]mcp.Content, 0, len(r.Parts))
	for i := range r.Parts {
		c, err := toBlock(&r.Parts[i], i)
		if err != nil {
			return nil, fmt.Errorf("invalid tool result: part %d: %w", i, err)
		}
		content = append(content, c)
	}
	return &mcp.CallToolResult{Content: content, IsError: r.IsError}, nil
}

func toBlock(p *llms.ContentPart, i int) (mcp.Content, error) {
	switch p.Type {
	case llms.ContentText:
		if p.Text == "" {
			return nil, errors.New("empty text part")
		}
		return &mcp.TextContent{Text: p.Text}, nil
	case llms.ContentImage, llms.ContentDocument:
		if err := CheckPartMedia(p); err != nil {
			return nil, err
		}
		if p.Type == llms.ContentImage {
			return &mcp.ImageContent{Data: p.Media.Data, MIMEType: p.Media.MediaType}, nil
		}
		return &mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
			URI:      DocumentURI(i),
			MIMEType: p.Media.MediaType,
			Blob:     p.Media.Data,
		}}, nil
	default:
		return nil, fmt.Errorf("%q part not allowed; tool result parts are text, image or document", p.Type)
	}
}

// CheckPartMedia applies the structural rules of llms/internal/media.Check,
// which this module can't import, to an image or document part: Media and
// its Data present, a media type that parses as type/subtype, an image part
// typed image/* and a document part not. The rules must stay identical.
func CheckPartMedia(p *llms.ContentPart) error {
	if p.Media == nil {
		return fmt.Errorf("%s part missing Media", p.Type)
	}
	if len(p.Media.Data) == 0 {
		return fmt.Errorf("%s part has empty Media.Data", p.Type)
	}
	base, err := normalizeMediaType(p.Media.MediaType)
	if err != nil {
		return fmt.Errorf("%s part: %w", p.Type, err)
	}
	image := strings.HasPrefix(base, "image/")
	switch {
	case p.Type == llms.ContentImage && !image:
		return fmt.Errorf("image part has non-image media type %q", base)
	case p.Type == llms.ContentDocument && image:
		return fmt.Errorf("document part has image media type %q; use an image part", base)
	}
	return nil
}

// normalizeMediaType is llms/internal/media.Normalize: the lowercase
// type/subtype of mediaType without parameters.
func normalizeMediaType(mediaType string) (string, error) {
	if strings.TrimSpace(mediaType) == "" {
		return "", errors.New("empty Media.MediaType")
	}
	base, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		return "", fmt.Errorf("invalid Media.MediaType %q: %w", mediaType, err)
	}
	if !strings.Contains(base, "/") {
		return "", fmt.Errorf("invalid Media.MediaType %q: want type/subtype", mediaType)
	}
	return base, nil
}
