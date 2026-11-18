// Package convert is the one place the MCP module translates between MCP
// and the llms/toolloop types (ADR-0049 §3): tool names, input schemas and
// call results. mcpclient uses it in one direction and mcpserver (ADR-0050)
// in the other, so the two can't drift.
package convert

import (
	"fmt"
	"regexp"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// namePattern is the portable tool-name rule (ADR-0049 §4.3): the
// intersection of Anthropic's and OpenAI's ^[a-zA-Z0-9_-]{1,64}$ and
// Gemini's rule that a name starts with a letter or underscore.
var namePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_-]{0,63}$`)

// CheckName reports whether name can be offered to every provider. It never
// suggests a rewrite: a rewritten name would be invisible to an app's grants
// and logs.
func CheckName(name string) error {
	if namePattern.MatchString(name) {
		return nil
	}
	return fmt.Errorf("name %q does not match %s (letters, digits, '_' and '-', at most 64, starting with a letter or '_')", name, namePattern)
}

// Description returns the text a model sees for t: its description, falling
// back to its title, then its annotations' title.
func Description(t *mcp.Tool) string {
	switch {
	case t.Description != "":
		return t.Description
	case t.Title != "":
		return t.Title
	case t.Annotations != nil:
		return t.Annotations.Title
	default:
		return ""
	}
}
