// Package media holds the shared handling of image and document content
// parts (ADR-0040): the structural check every adapter and the validation
// middleware apply, media-type normalization, data URIs, and the header
// readers the token estimator uses.
package media

import (
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"strings"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// IsMedia reports whether t is an image or document part type.
func IsMedia(t llms.ContentPartType) bool {
	return t == llms.ContentImage || t == llms.ContentDocument
}

// Check validates an image or document part and returns its normalized media
// type (lowercase, parameters dropped). The rules are structural only: a
// payload is present, the bytes are non-empty, the media type parses, and an
// image part carries an image type while a document part does not. Which
// types a provider accepts is the adapter's decision.
func Check(p *llms.ContentPart) (string, error) {
	if p.Media == nil {
		return "", fmt.Errorf("%s part missing Media", p.Type)
	}
	if len(p.Media.Data) == 0 {
		return "", fmt.Errorf("%s part has empty Media.Data", p.Type)
	}
	base, err := Normalize(p.Media.MediaType)
	if err != nil {
		return "", fmt.Errorf("%s part: %w", p.Type, err)
	}
	image := strings.HasPrefix(base, "image/")
	switch {
	case p.Type == llms.ContentImage && !image:
		return "", fmt.Errorf("image part has non-image media type %q", base)
	case p.Type == llms.ContentDocument && image:
		return "", fmt.Errorf("document part has image media type %q; use an image part", base)
	}
	return base, nil
}

// Normalize returns the lowercase type/subtype of mediaType without
// parameters, or an error if it does not parse as a media type.
func Normalize(mediaType string) (string, error) {
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

// Base64 returns data base64-encoded with the standard alphabet and padding.
func Base64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// DataURI returns a base64 data URI for data with the given media type.
func DataURI(mediaType string, data []byte) string {
	return "data:" + mediaType + ";base64," + Base64(data)
}

// CheckRole rejects image and document parts on any message that is not a
// user message. Adapters call it so they fail closed without the validation
// middleware.
func CheckRole(m *llms.Message) error {
	if m.Role == llms.RoleUser {
		return nil
	}
	for i := range m.Content {
		if t := m.Content[i].Type; IsMedia(t) {
			return fmt.Errorf("%s content part is not valid in a %q message; media is allowed on user messages and in ToolResult.Parts only", t, m.Role)
		}
	}
	return nil
}

// CheckToolResult validates the shape of a tool result's Parts (ADR-0041):
// Content and Parts are not both set, and each part is a non-empty text part
// or a well-formed image or document part (Check). Which media a provider
// accepts in a tool result is the adapter's decision. The validation
// middleware, toolloop and every adapter call it.
func CheckToolResult(r *llms.ToolResult) error {
	if len(r.Parts) == 0 {
		return nil
	}
	if r.Content != "" {
		return errors.New("tool result sets both Content and Parts; set one")
	}
	return checkParts("tool result", r.Parts)
}

// CheckEmbeddingInput validates the shape of an embedding input's Parts
// (ADR-0045): Text and Parts are not both set, and each part is a non-empty
// text part or a well-formed image or document part (Check). An input with
// neither is not this check's concern (the provider decides, as before).
// Which media a provider accepts is the adapter's decision. The validation
// middleware and every embedding adapter call it.
func CheckEmbeddingInput(in *llms.EmbeddingInput) error {
	if len(in.Parts) == 0 {
		return nil
	}
	if in.Text != "" {
		return errors.New("embedding input sets both Text and Parts; set one")
	}
	return checkParts("embedding input", in.Parts)
}

// checkParts requires each part to be a non-empty text part or a well-formed
// image or document part (Check); what names the owner in errors. It is the
// shared rule behind CheckToolResult and CheckEmbeddingInput.
func checkParts(what string, parts []llms.ContentPart) error {
	for i := range parts {
		p := &parts[i]
		switch p.Type {
		case llms.ContentText:
			if p.Text == "" {
				return fmt.Errorf("%s part %d: empty text part", what, i)
			}
		case llms.ContentImage, llms.ContentDocument:
			if _, err := Check(p); err != nil {
				return fmt.Errorf("%s part %d: %w", what, i, err)
			}
		default:
			return fmt.Errorf("%s part %d: %q part not allowed; %s parts are text, image or document", what, i, p.Type, what)
		}
	}
	return nil
}

// HasMedia reports whether parts contains an image or document part.
func HasMedia(parts []llms.ContentPart) bool {
	for i := range parts {
		if IsMedia(parts[i].Type) {
			return true
		}
	}
	return false
}

// JoinText joins the text parts of a tool result's or embedding input's Parts
// with a blank line, for providers that take a single string (ADR-0041,
// ADR-0045). It ignores other parts; callers reject media first.
func JoinText(parts []llms.ContentPart) string {
	var b strings.Builder
	for i := range parts {
		if parts[i].Type != llms.ContentText {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(parts[i].Text)
	}
	return b.String()
}
