package convert

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCheckName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"search", true},
		{"_private", true},
		{"get-file_v2", true},
		{"A", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
		{"", false},
		{"2fa", false},     // Gemini: must start with a letter or '_'
		{"-dash", false},   // Gemini: must start with a letter or '_'
		{"fs.read", false}, // '.' is Gemini-only
		{"fs:read", false}, // ':' is Gemini-only
		{"fs/read", false}, // never allowed
		{"has space", false},
		{"émoji", false},
	}
	for _, c := range cases {
		err := CheckName(c.name)
		if (err == nil) != c.ok {
			t.Errorf("CheckName(%q) = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestDescription(t *testing.T) {
	cases := []struct {
		tool *mcp.Tool
		want string
	}{
		{&mcp.Tool{Description: "d", Title: "t", Annotations: &mcp.ToolAnnotations{Title: "a"}}, "d"},
		{&mcp.Tool{Title: "t", Annotations: &mcp.ToolAnnotations{Title: "a"}}, "t"},
		{&mcp.Tool{Annotations: &mcp.ToolAnnotations{Title: "a"}}, "a"},
		{&mcp.Tool{}, ""},
	}
	for i, c := range cases {
		if got := Description(c.tool); got != c.want {
			t.Errorf("case %d: Description = %q, want %q", i, got, c.want)
		}
	}
}
