package convert

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

// Result maps an MCP tool-call result to the loop's result (ADR-0049 §4.7).
// An error means the result is malformed, a server bug: the caller returns
// it as an Execute error. Audio is not an error but a model-visible IsError
// result, since the call itself succeeded.
func Result(r *mcp.CallToolResult) (toolloop.ToolResult, error) {
	if r == nil {
		return toolloop.ToolResult{}, errors.New("malformed result: nil")
	}
	if len(r.Content) == 0 && r.StructuredContent != nil {
		b, err := json.Marshal(r.StructuredContent)
		if err != nil {
			return toolloop.ToolResult{}, fmt.Errorf("malformed result: structuredContent: %w", err)
		}
		return toolloop.ToolResult{Content: string(b), IsError: r.IsError}, nil
	}

	parts := make([]llms.ContentPart, 0, len(r.Content))
	hasMedia := false
	for i, c := range r.Content {
		p, err := block(c)
		if errors.Is(err, errAudio) {
			return toolloop.ToolResult{
				Content: "The tool returned audio content, which this client cannot pass to the model.",
				IsError: true,
			}, nil
		}
		if err != nil {
			return toolloop.ToolResult{}, fmt.Errorf("malformed result: content block %d: %w", i, err)
		}
		switch {
		case p.Type == llms.ContentText && p.Text == "":
			// Empty text is dropped: ADR-0041 rejects an empty text part.
			continue
		case p.Type != llms.ContentText:
			hasMedia = true
		}
		parts = append(parts, p)
	}

	if hasMedia {
		return toolloop.ToolResult{Parts: parts, IsError: r.IsError}, nil
	}
	texts := make([]string, len(parts))
	for i := range parts {
		texts[i] = parts[i].Text
	}
	return toolloop.ToolResult{Content: strings.Join(texts, "\n"), IsError: r.IsError}, nil
}

var errAudio = errors.New("audio content")

// block maps one content block to a text, image or document part.
func block(c mcp.Content) (llms.ContentPart, error) {
	switch b := c.(type) {
	case *mcp.TextContent:
		return llms.Text(b.Text), nil
	case *mcp.ImageContent:
		if err := checkMedia(b.MIMEType, b.Data); err != nil {
			return llms.ContentPart{}, fmt.Errorf("image: %w", err)
		}
		if !isImage(b.MIMEType) {
			return llms.ContentPart{}, fmt.Errorf("image has non-image MIME type %q", b.MIMEType)
		}
		return llms.Image(b.MIMEType, b.Data), nil
	case *mcp.AudioContent:
		return llms.ContentPart{}, errAudio
	case *mcp.ResourceLink:
		return llms.Text(linkText(b)), nil
	case *mcp.EmbeddedResource:
		return embedded(b)
	default:
		return llms.ContentPart{}, fmt.Errorf("unsupported content type %T", c)
	}
}

func embedded(b *mcp.EmbeddedResource) (llms.ContentPart, error) {
	res := b.Resource
	if res == nil {
		return llms.ContentPart{}, errors.New("embedded resource has no contents")
	}
	if res.Blob == nil {
		// No blob member: a text resource. An explicit empty blob ("blob": "",
		// decoded as a non-nil empty slice) is a malformed blob, below.
		return llms.Text(res.Text), nil
	}
	if err := checkMedia(res.MIMEType, res.Blob); err != nil {
		return llms.ContentPart{}, fmt.Errorf("embedded resource %q: %w", res.URI, err)
	}
	if isImage(res.MIMEType) {
		return llms.Image(res.MIMEType, res.Blob), nil
	}
	return llms.Document(res.MIMEType, res.Blob), nil
}

func checkMedia(mimeType string, data []byte) error {
	switch {
	case len(data) == 0:
		return errors.New("no data")
	case strings.TrimSpace(mimeType) == "":
		return errors.New("no MIME type")
	}
	return nil
}

func isImage(mimeType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(mimeType)), "image/")
}

// linkText describes a resource link for the model. It always contains the
// URI, so it is never empty.
func linkText(l *mcp.ResourceLink) string {
	var sb strings.Builder
	sb.WriteString("Resource link:")
	if l.Name != "" {
		sb.WriteString(" " + l.Name)
	}
	sb.WriteString(" <" + l.URI + ">")
	if l.MIMEType != "" {
		sb.WriteString(" (" + l.MIMEType + ")")
	}
	return sb.String()
}
