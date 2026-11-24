package convert

import (
	"errors"
	"fmt"
	"strings"
)

// CheckMetaKey applies MCP's _meta key rule: an optional prefix of
// dot-separated labels followed by a slash, then a name. A label starts with
// a letter, ends with a letter or digit, and has letters, digits or hyphens
// inside. A name starts and ends with a letter or digit and has letters,
// digits, hyphens, underscores or dots inside. A prefix with a label
// "modelcontextprotocol" or "mcp" is reserved for MCP (the go-sdk's own keys
// are io.modelcontextprotocol/...), and progressToken is a field MCP defines.
func CheckMetaKey(k string) error {
	prefix, name, hasPrefix := strings.Cut(k, "/")
	if !hasPrefix {
		prefix, name = "", k
	}
	if hasPrefix {
		if prefix == "" {
			return errors.New("empty prefix")
		}
		for label := range strings.SplitSeq(prefix, ".") {
			if !validLabel(label) {
				return fmt.Errorf("invalid prefix label %q", label)
			}
			if reservedLabel(label) {
				return errors.New("the prefix is reserved for MCP")
			}
		}
	}
	if !validName(name) {
		return errors.New("invalid name")
	}
	if !hasPrefix && name == "progressToken" {
		return errors.New("progressToken is defined by MCP")
	}
	return nil
}

// ReservedMetaKey reports whether k is a _meta key MCP reserves for itself:
// one whose prefix has a label "modelcontextprotocol" or "mcp" (any case),
// such as the go-sdk's io.modelcontextprotocol/protocolVersion, or the
// unprefixed progressToken. It checks only the reservation, not the key
// format, so a server can drop MCP's own keys and keep everything else a
// client sent (ADR-0050 amendment for #200).
func ReservedMetaKey(k string) bool {
	prefix, _, hasPrefix := strings.Cut(k, "/")
	if !hasPrefix {
		return k == "progressToken"
	}
	for label := range strings.SplitSeq(prefix, ".") {
		if reservedLabel(label) {
			return true
		}
	}
	return false
}

func reservedLabel(label string) bool {
	l := strings.ToLower(label)
	return l == "modelcontextprotocol" || l == "mcp"
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlnum(c byte) bool { return isAlpha(c) || isDigit(c) }

func validLabel(l string) bool {
	if l == "" || !isAlpha(l[0]) || !isAlnum(l[len(l)-1]) {
		return false
	}
	for i := range len(l) {
		if c := l[i]; !isAlnum(c) && c != '-' {
			return false
		}
	}
	return true
}

func validName(n string) bool {
	if n == "" || !isAlnum(n[0]) || !isAlnum(n[len(n)-1]) {
		return false
	}
	for i := range len(n) {
		if c := n[i]; !isAlnum(c) && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}
