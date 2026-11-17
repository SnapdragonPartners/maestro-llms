package convert

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// maxRefDepth bounds nested reference expansion. A non-recursive schema can
// still nest references deeply or fan out exponentially; past this depth the
// schema is rejected rather than expanded without limit.
const maxRefDepth = 32

// maxNodes bounds the size of an expanded schema. References that fan out
// (a target referenced twice, whose own target is referenced twice, ...)
// grow exponentially without being recursive.
const maxNodes = 100_000

// objectSchema is the schema a tool with none gets.
const objectSchema = `{"type":"object"}`

// Schema normalizes an MCP input schema for the providers (ADR-0049 §4.4).
// in is anything that marshals to JSON (the go-sdk hands clients a
// map[string]any). The only rewrite is inlining local references
// ("#/$defs/...", "#/definitions/..." or any other "#/" pointer into the
// schema) and removing the top-level "$defs" and "definitions"; Gemini
// doesn't accept references. A recursive, remote, anchor or dangling
// reference is an error, and so is a top-level type other than "object". A
// nil or JSON null schema becomes {"type":"object"}.
func Schema(in any) (json.RawMessage, error) {
	raw, err := toRaw(in)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(objectSchema), nil
	}
	var doc any
	if uerr := json.Unmarshal(raw, &doc); uerr != nil {
		return nil, fmt.Errorf("input schema is not valid JSON: %w", uerr)
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, errors.New("input schema is not a JSON object")
	}
	if t, ok := root["type"].(string); !ok || t != "object" {
		return nil, fmt.Errorf(`input schema type must be "object", got %v`, root["type"])
	}
	if !hasRef(root) {
		// Nothing to inline: send the schema unchanged.
		return raw, nil
	}
	r := resolver{root: root}
	out, err := r.expand(root, nil)
	if err != nil {
		return nil, err
	}
	m, _ := out.(map[string]any) // expand returns the same kind it was given
	delete(m, "$defs")
	delete(m, "definitions")
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("input schema: %w", err)
	}
	return b, nil
}

func toRaw(in any) (json.RawMessage, error) {
	switch v := in.(type) {
	case nil:
		return nil, nil
	case json.RawMessage:
		return v, nil
	case []byte:
		return v, nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("input schema does not marshal to JSON: %w", err)
		}
		return b, nil
	}
}

// hasRef reports whether any object in v has a string "$ref".
func hasRef(v any) bool {
	switch n := v.(type) {
	case map[string]any:
		if _, ok := n["$ref"].(string); ok {
			return true
		}
		for _, c := range n {
			if hasRef(c) {
				return true
			}
		}
	case []any:
		for _, c := range n {
			if hasRef(c) {
				return true
			}
		}
	}
	return false
}

type resolver struct {
	root  map[string]any
	nodes int
}

// expand returns a copy of v with every reference replaced by a copy of its
// target. stack holds the pointers being expanded, outermost first, to
// detect recursion.
func (r *resolver) expand(v any, stack []string) (any, error) {
	r.nodes++
	if r.nodes > maxNodes {
		return nil, fmt.Errorf("input schema: expands to more than %d nodes", maxNodes)
	}
	switch n := v.(type) {
	case map[string]any:
		if ref, ok := n["$ref"].(string); ok {
			return r.inline(n, ref, stack)
		}
		out := make(map[string]any, len(n))
		for k, c := range n {
			e, err := r.expand(c, stack)
			if err != nil {
				return nil, err
			}
			out[k] = e
		}
		return out, nil
	case []any:
		out := make([]any, len(n))
		for i, c := range n {
			e, err := r.expand(c, stack)
			if err != nil {
				return nil, err
			}
			out[i] = e
		}
		return out, nil
	default:
		return v, nil
	}
}

// inline replaces the object n, which has "$ref": ref, with its expanded
// target. Keywords beside "$ref" (allowed since JSON Schema 2019-09, for
// example a description) are kept and win over the target's.
func (r *resolver) inline(n map[string]any, ref string, stack []string) (any, error) {
	if !strings.HasPrefix(ref, "#/") {
		if ref == "#" {
			return nil, fmt.Errorf("input schema: recursive reference %q", ref)
		}
		return nil, fmt.Errorf("input schema: unsupported reference %q (only local JSON pointers like \"#/$defs/name\" are inlined)", ref)
	}
	for _, s := range stack {
		if s == ref {
			return nil, fmt.Errorf("input schema: recursive reference %q", ref)
		}
	}
	if len(stack) >= maxRefDepth {
		return nil, fmt.Errorf("input schema: references nested deeper than %d", maxRefDepth)
	}
	target, err := r.lookup(ref)
	if err != nil {
		return nil, err
	}
	expanded, err := r.expand(target, append(stack, ref))
	if err != nil {
		return nil, err
	}
	if len(n) == 1 {
		return expanded, nil
	}
	m, ok := expanded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("input schema: reference %q has sibling keywords but its target is not an object", ref)
	}
	for k, c := range n {
		if k == "$ref" {
			continue
		}
		e, err := r.expand(c, stack)
		if err != nil {
			return nil, err
		}
		m[k] = e
	}
	return m, nil
}

// lookup resolves a "#/..." JSON pointer (RFC 6901) against the root.
func (r *resolver) lookup(ref string) (any, error) {
	var cur any = r.root
	for _, tok := range strings.Split(ref[2:], "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch c := cur.(type) {
		case map[string]any:
			next, ok := c[tok]
			if !ok {
				return nil, fmt.Errorf("input schema: reference %q does not resolve", ref)
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(c) {
				return nil, fmt.Errorf("input schema: reference %q does not resolve", ref)
			}
			cur = c[i]
		default:
			return nil, fmt.Errorf("input schema: reference %q does not resolve", ref)
		}
	}
	return cur, nil
}
