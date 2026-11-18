package convert

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"strconv"
	"strings"
)

// maxRefDepth bounds nested reference expansion. A non-recursive schema can
// still nest references deeply or fan out exponentially; past this depth the
// schema is rejected rather than expanded without limit.
const maxRefDepth = 32

// maxNodes and maxBytes bound what inlining copies into a schema: the JSON
// values, and the bytes of their strings (object keys included), of every
// copy of a reference's target, nested copies included. References that fan
// out (a target referenced twice, whose own target is referenced twice, ...)
// grow exponentially without being recursive, and a large literal (an enum,
// a default) is copied once per reference. The schema as the server wrote it
// is not counted: one with no references is sent unchanged whatever its
// size. The count is checked as copies are made, so expansion stops at the
// bound, and doesn't depend on the order of map keys.
const (
	maxNodes = 100_000
	maxBytes = 4 << 20
)

// objectSchema is the schema a tool with none gets.
const objectSchema = `{"type":"object"}`

// Schema normalizes an MCP input schema for the providers (ADR-0049 §4.4).
// in is anything that marshals to JSON (the go-sdk hands clients a
// map[string]any). The only rewrite is inlining local references
// ("#/$defs/...", "#/definitions/..." or any other "#/" pointer into the
// schema), after which "$defs" and "definitions" are dropped at every depth,
// since nothing references them any more; Gemini doesn't accept references.
// A schema with no references is returned unchanged. Numbers are kept as
// written (decoded as json.Number), so JSON input round-trips exactly; a
// map from the go-sdk already holds float64 values. Only subschemas are
// searched (see kindOf):
// literal data such as a default, const, enum or examples value is kept as
// it is, even if it contains a "$ref" member. A recursive, remote, anchor or
// dangling reference is an error, and so is a reference inside a nested
// schema resource (a subschema with its own "$id", against which the
// reference would resolve), an expansion past maxRefDepth, maxNodes or
// maxBytes, and a top-level type other than "object". A nil or JSON null
// schema becomes {"type":"object"}.
func Schema(in any) (json.RawMessage, error) {
	raw, err := toRaw(in)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(objectSchema), nil
	}
	doc, err := decodeJSON(raw)
	if err != nil {
		return nil, err
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, errors.New("input schema is not a JSON object")
	}
	if t, ok := root["type"].(string); !ok || t != "object" {
		return nil, fmt.Errorf(`input schema type must be "object", got %v`, root["type"])
	}
	r := resolver{root: root}
	out, err := r.schema(root, nil)
	if err != nil {
		return nil, err
	}
	if r.inlined == 0 {
		// Nothing to inline: send the schema unchanged.
		return raw, nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("input schema: %w", err)
	}
	return b, nil
}

// decodeJSON decodes one JSON value, keeping numbers as json.Number so that
// a re-marshaled number is byte-for-byte what was written (no float64
// rounding past 2^53, and no failure for a number out of float64 range).
func decodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("input schema is not valid JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("input schema is not valid JSON: data after the top-level value")
	}
	return doc, nil
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

// keywordKind says what a schema keyword's value holds, so that only
// subschemas are searched for references. Every other keyword is literal
// data (default, const, enum, examples, unknown keywords) and is kept as it
// is, even when it contains an object with a "$ref" member.
//
// Definitions ($defs, definitions) are reached only through references.
// The expanded schema is used only when something was inlined, and then
// every reference has been resolved against the original document, so no
// definition is used any more: they are dropped wherever they appear, not
// walked, and not counted.
type keywordKind int

const (
	kindLiteral      keywordKind = iota
	kindSchema                   // one schema
	kindSchemaMap                // name -> schema
	kindSchemaList               // array of schemas
	kindSchemaOrList             // a schema, or an array of schemas (items before 2020-12)
	kindDependencies             // name -> schema, or name -> array of property names (draft 7)
	kindDefinitions              // $defs / definitions: dropped from the expansion
)

func kindOf(keyword string) keywordKind {
	switch keyword {
	case "additionalItems", "additionalProperties", "contains", "contentSchema",
		"else", "if", "not", "propertyNames", "then",
		"unevaluatedItems", "unevaluatedProperties":
		return kindSchema
	case "dependentSchemas", "patternProperties", "properties":
		return kindSchemaMap
	case "allOf", "anyOf", "oneOf", "prefixItems":
		return kindSchemaList
	case "items":
		return kindSchemaOrList
	case "dependencies":
		return kindDependencies
	case "$defs", "definitions":
		return kindDefinitions
	default:
		return kindLiteral
	}
}

type resolver struct {
	root       map[string]any
	nodes      int
	bytes      int
	inlined    int
	copying    int // depth of reference expansions around the current value
	inResource int // depth of nested "$id" resources around the current schema
}

// charge counts one JSON value with n string bytes when it is part of a copy
// made by inlining, and fails once the copies are over a bound.
func (r *resolver) charge(n int) error {
	if r.copying == 0 {
		return nil
	}
	r.nodes++
	r.bytes += n
	if r.nodes > maxNodes || r.bytes > maxBytes {
		return fmt.Errorf("input schema: inlining copies more than %d values or %d bytes", maxNodes, maxBytes)
	}
	return nil
}

// key counts an object key's bytes when it is part of a copy; the next
// charge checks the bound.
func (r *resolver) key(k string) {
	if r.copying > 0 {
		r.bytes += len(k)
	}
}

// literal charges every value in the literal v, which is copied into the
// expansion as it is (shared, but serialized once per copy).
func (r *resolver) literal(v any) error {
	switch n := v.(type) {
	case map[string]any:
		if err := r.charge(0); err != nil {
			return err
		}
		for k, c := range n {
			r.key(k)
			if err := r.literal(c); err != nil {
				return err
			}
		}
		return nil
	case []any:
		if err := r.charge(0); err != nil {
			return err
		}
		for _, c := range n {
			if err := r.literal(c); err != nil {
				return err
			}
		}
		return nil
	case string:
		return r.charge(len(n))
	case json.Number:
		return r.charge(len(n))
	default:
		return r.charge(0)
	}
}

// schema returns a copy of the schema v with every reference in a subschema
// replaced by a copy of its expanded target. stack holds the decoded
// pointers being expanded, outermost first, to detect recursion. A
// non-object schema (true or false) is returned as it is.
func (r *resolver) schema(v any, stack []string) (any, error) {
	n, ok := v.(map[string]any)
	if !ok {
		return v, r.literal(v)
	}
	if err := r.charge(0); err != nil {
		return nil, err
	}
	// A subschema with its own "$id" starts a schema resource: references
	// inside it resolve against that resource, not the document root, so
	// they can't be inlined from the root. The root's own "$id" is fine.
	if _, isResource := n["$id"].(string); isResource && !isRoot(n, r.root) {
		r.inResource++
		defer func() { r.inResource-- }()
	}
	if ref, ok := n["$ref"].(string); ok {
		return r.inline(n, ref, stack)
	}
	out := make(map[string]any, len(n))
	for k, c := range n {
		if kindOf(k) == kindDefinitions {
			continue
		}
		r.key(k)
		e, err := r.keyword(k, c, stack)
		if err != nil {
			return nil, err
		}
		out[k] = e
	}
	return out, nil
}

// isRoot reports whether m is the document root map itself.
func isRoot(m, root map[string]any) bool {
	return reflect.ValueOf(m).UnsafePointer() == reflect.ValueOf(root).UnsafePointer()
}

// keyword expands the subschemas in the value c of keyword k and returns
// literal values unchanged. A value of the wrong shape for its keyword is
// kept as it is; the provider decides what to make of it.
func (r *resolver) keyword(k string, c any, stack []string) (any, error) {
	switch kindOf(k) {
	case kindSchema:
		return r.schema(c, stack)
	case kindSchemaMap:
		return r.schemaMap(c, stack, false)
	case kindDependencies:
		return r.schemaMap(c, stack, true)
	case kindSchemaList:
		return r.schemaList(c, stack)
	case kindSchemaOrList:
		if _, isList := c.([]any); isList {
			return r.schemaList(c, stack)
		}
		return r.schema(c, stack)
	default:
		return c, r.literal(c)
	}
}

// schemaMap expands each value of a name -> schema object. With
// keepArrays, an array value (a draft-7 property dependency) is literal.
func (r *resolver) schemaMap(c any, stack []string, keepArrays bool) (any, error) {
	m, ok := c.(map[string]any)
	if !ok {
		return c, r.literal(c)
	}
	if err := r.charge(0); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(m))
	for name, v := range m {
		r.key(name)
		if _, isList := v.([]any); keepArrays && isList {
			out[name] = v
			if err := r.literal(v); err != nil {
				return nil, err
			}
			continue
		}
		e, err := r.schema(v, stack)
		if err != nil {
			return nil, err
		}
		out[name] = e
	}
	return out, nil
}

func (r *resolver) schemaList(c any, stack []string) (any, error) {
	l, ok := c.([]any)
	if !ok {
		return c, r.literal(c)
	}
	if err := r.charge(0); err != nil {
		return nil, err
	}
	out := make([]any, len(l))
	for i, v := range l {
		e, err := r.schema(v, stack)
		if err != nil {
			return nil, err
		}
		out[i] = e
	}
	return out, nil
}

// inline replaces the schema n, which has "$ref": ref, with its expanded
// target. Keywords beside "$ref" (allowed since JSON Schema 2019-09, for
// example a description) are kept and win over the target's.
func (r *resolver) inline(n map[string]any, ref string, stack []string) (any, error) {
	pointer, err := r.pointer(ref, stack)
	if err != nil {
		return nil, err
	}
	target, inResource, err := r.lookup(ref, pointer)
	if err != nil {
		return nil, err
	}
	if inResource {
		// The target lies inside a nested resource, so references in it
		// resolve against that resource too.
		r.inResource++
		defer func() { r.inResource-- }()
	}
	r.inlined++
	r.copying++
	expanded, err := r.schema(target, append(stack, pointer))
	r.copying--
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
		if k == "$ref" || kindOf(k) == kindDefinitions {
			continue
		}
		r.key(k)
		e, err := r.keyword(k, c, stack)
		if err != nil {
			return nil, err
		}
		m[k] = e
	}
	return m, nil
}

// pointer checks the reference ref and returns its decoded JSON pointer: it
// must not sit inside a nested resource, must be a local "#/" pointer, must
// not already be on the expansion stack (recursion), and must not nest past
// maxRefDepth. The fragment is a URI fragment, so it is percent-decoded and
// then read as a JSON pointer (RFC 6901 §6): "#/$defs/a%20b" names "a b".
// The decoded pointer is what recursion is checked on, so two spellings of
// one target are one reference.
func (r *resolver) pointer(ref string, stack []string) (string, error) {
	if r.inResource > 0 {
		return "", fmt.Errorf("input schema: reference %q is inside a nested schema resource (a subschema with its own \"$id\"), which is not supported", ref)
	}
	if !strings.HasPrefix(ref, "#/") {
		if ref == "#" {
			return "", fmt.Errorf("input schema: recursive reference %q", ref)
		}
		return "", fmt.Errorf("input schema: unsupported reference %q (only local JSON pointers like \"#/$defs/name\" are inlined)", ref)
	}
	pointer, err := url.PathUnescape(ref[1:])
	if err != nil {
		return "", fmt.Errorf("input schema: reference %q is not a valid URI fragment: %w", ref, err)
	}
	for _, s := range stack {
		if s == pointer {
			return "", fmt.Errorf("input schema: recursive reference %q", ref)
		}
	}
	if len(stack) >= maxRefDepth {
		return "", fmt.Errorf("input schema: references nested deeper than %d", maxRefDepth)
	}
	return pointer, nil
}

// lookup resolves the decoded JSON pointer of reference ref (used in
// errors) against the root. inResource reports that the path passes through
// a nested schema resource (a non-root object with its own "$id") above the
// target; the target's own "$id" is seen when it is expanded.
func (r *resolver) lookup(ref, pointer string) (target any, inResource bool, err error) {
	if !strings.HasPrefix(pointer, "/") {
		return nil, false, fmt.Errorf("input schema: reference %q does not resolve", ref)
	}
	var cur any = r.root
	for _, tok := range strings.Split(pointer[1:], "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch c := cur.(type) {
		case map[string]any:
			if _, id := c["$id"].(string); id && !isRoot(c, r.root) {
				inResource = true
			}
			next, ok := c[tok]
			if !ok {
				return nil, false, fmt.Errorf("input schema: reference %q does not resolve", ref)
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(c) {
				return nil, false, fmt.Errorf("input schema: reference %q does not resolve", ref)
			}
			cur = c[i]
		default:
			return nil, false, fmt.Errorf("input schema: reference %q does not resolve", ref)
		}
	}
	return cur, inResource, nil
}
