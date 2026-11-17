package convert

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad test JSON %s: %v", s, err)
	}
	return m
}

func equalJSON(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("result is not JSON: %s", got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad want JSON: %s", want)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("schema = %s\nwant     %s", got, want)
	}
}

func TestSchemaNoRefsUnchanged(t *testing.T) {
	in := `{"type":"object","properties":{"q":{"type":"string"}},"$defs":{"unused":{"type":"number"}}}`
	got, err := Schema(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != in {
		t.Errorf("schema without refs was rewritten: %s", got)
	}
}

func TestSchemaMissing(t *testing.T) {
	for _, in := range []any{nil, json.RawMessage(nil), json.RawMessage("null")} {
		got, err := Schema(in)
		if err != nil {
			t.Fatalf("Schema(%v): %v", in, err)
		}
		equalJSON(t, got, `{"type":"object"}`)
	}
}

func TestSchemaFromMap(t *testing.T) {
	// The go-sdk hands clients a map[string]any.
	got, err := Schema(mustJSON(t, `{"type":"object","properties":{"a":{"$ref":"#/$defs/A"}},"$defs":{"A":{"type":"string"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	equalJSON(t, got, `{"type":"object","properties":{"a":{"type":"string"}}}`)
}

func TestSchemaInlining(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			name: "defs",
			in:   `{"type":"object","properties":{"a":{"$ref":"#/$defs/A"}},"$defs":{"A":{"type":"string","description":"an A"}}}`,
			want: `{"type":"object","properties":{"a":{"type":"string","description":"an A"}}}`,
		},
		{
			name: "definitions",
			in:   `{"type":"object","properties":{"a":{"$ref":"#/definitions/A"}},"definitions":{"A":{"type":"integer"}}}`,
			want: `{"type":"object","properties":{"a":{"type":"integer"}}}`,
		},
		{
			name: "nested",
			in: `{"type":"object","properties":{"p":{"$ref":"#/$defs/P"}},"$defs":{
				"P":{"type":"object","properties":{"addr":{"$ref":"#/$defs/Addr"}}},
				"Addr":{"type":"object","properties":{"city":{"type":"string"}}}}}`,
			want: `{"type":"object","properties":{"p":{"type":"object","properties":{"addr":{"type":"object","properties":{"city":{"type":"string"}}}}}}}`,
		},
		{
			name: "repeated",
			in:   `{"type":"object","properties":{"a":{"$ref":"#/$defs/S"},"b":{"type":"array","items":{"$ref":"#/$defs/S"}}},"$defs":{"S":{"type":"string"}}}`,
			want: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"array","items":{"type":"string"}}}}`,
		},
		{
			name: "sibling keywords win",
			in:   `{"type":"object","properties":{"a":{"$ref":"#/$defs/S","description":"local"}},"$defs":{"S":{"type":"string","description":"shared"}}}`,
			want: `{"type":"object","properties":{"a":{"type":"string","description":"local"}}}`,
		},
		{
			name: "pointer escapes",
			in:   `{"type":"object","properties":{"a":{"$ref":"#/$defs/a~1b~0c"}},"$defs":{"a/b~c":{"type":"boolean"}}}`,
			want: `{"type":"object","properties":{"a":{"type":"boolean"}}}`,
		},
		{
			name: "pointer into properties",
			in:   `{"type":"object","properties":{"a":{"type":"string"},"b":{"$ref":"#/properties/a"}}}`,
			want: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}}}`,
		},
		{
			name: "array index",
			in:   `{"type":"object","properties":{"a":{"anyOf":[{"type":"string"},{"type":"null"}]},"b":{"$ref":"#/properties/a/anyOf/0"}}}`,
			want: `{"type":"object","properties":{"a":{"anyOf":[{"type":"string"},{"type":"null"}]},"b":{"type":"string"}}}`,
		},
		{
			name: "property named $ref is not a reference",
			in:   `{"type":"object","properties":{"$ref":{"type":"string"},"x":{"$ref":"#/$defs/X"}},"$defs":{"X":{"type":"number"}}}`,
			want: `{"type":"object","properties":{"$ref":{"type":"string"},"x":{"type":"number"}}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Schema(json.RawMessage(c.in))
			if err != nil {
				t.Fatal(err)
			}
			equalJSON(t, got, c.want)
		})
	}
}

func TestSchemaErrors(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"recursive", `{"type":"object","properties":{"n":{"$ref":"#/$defs/Node"}},"$defs":{"Node":{"type":"object","properties":{"next":{"$ref":"#/$defs/Node"}}}}}`, "recursive"},
		{"mutually recursive", `{"type":"object","properties":{"a":{"$ref":"#/$defs/A"}},"$defs":{"A":{"type":"object","properties":{"b":{"$ref":"#/$defs/B"}}},"B":{"type":"object","properties":{"a":{"$ref":"#/$defs/A"}}}}}`, "recursive"},
		{"root reference", `{"type":"object","properties":{"self":{"$ref":"#"}}}`, "recursive"},
		{"remote", `{"type":"object","properties":{"a":{"$ref":"https://example.com/s.json"}}}`, "unsupported reference"},
		{"relative file", `{"type":"object","properties":{"a":{"$ref":"other.json#/A"}}}`, "unsupported reference"},
		{"anchor", `{"type":"object","properties":{"a":{"$ref":"#thing"}}}`, "unsupported reference"},
		{"dangling", `{"type":"object","properties":{"a":{"$ref":"#/$defs/Missing"}}}`, "does not resolve"},
		{"bad index", `{"type":"object","properties":{"a":{"anyOf":[{"type":"string"}]},"b":{"$ref":"#/properties/a/anyOf/3"}}}`, "does not resolve"},
		{"sibling on non-object target", `{"type":"object","properties":{"a":{"$ref":"#/$defs/T","description":"x"}},"$defs":{"T":true}}`, "not an object"},
		{"type string", `{"type":"string"}`, `must be "object"`},
		{"type missing", `{"properties":{}}`, `must be "object"`},
		{"type array", `{"type":["object","null"]}`, `must be "object"`},
		{"not an object", `[1,2]`, "not a JSON object"},
		{"invalid JSON", `{"type":`, "not valid JSON"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Schema(json.RawMessage(c.in))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Schema error = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestSchemaFanOutBounded(t *testing.T) {
	// D0 references D1 twice, D1 references D2 twice, ... : 2^40 nodes if
	// expanded, though nothing is recursive. Depth stays under maxRefDepth
	// only if the chain is short, so use a chain that trips the node budget.
	const levels = 30
	defs := make([]string, 0, levels+1)
	for i := range levels {
		defs = append(defs, fmt.Sprintf(`"D%d":{"type":"object","properties":{"a":{"$ref":"#/$defs/D%d"},"b":{"$ref":"#/$defs/D%d"}}}`, i, i+1, i+1))
	}
	defs = append(defs, fmt.Sprintf(`"D%d":{"type":"string"}`, levels))
	in := `{"type":"object","properties":{"x":{"$ref":"#/$defs/D0"}},"$defs":{` + strings.Join(defs, ",") + `}}`
	_, err := Schema(json.RawMessage(in))
	if err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("Schema error = %v, want the node budget", err)
	}
}

func TestSchemaDepthBounded(t *testing.T) {
	const levels = maxRefDepth + 2
	defs := make([]string, 0, levels+1)
	for i := range levels {
		defs = append(defs, fmt.Sprintf(`"D%d":{"type":"object","properties":{"a":{"$ref":"#/$defs/D%d"}}}`, i, i+1))
	}
	defs = append(defs, fmt.Sprintf(`"D%d":{"type":"string"}`, levels))
	in := `{"type":"object","properties":{"x":{"$ref":"#/$defs/D0"}},"$defs":{` + strings.Join(defs, ",") + `}}`
	_, err := Schema(json.RawMessage(in))
	if err == nil || !strings.Contains(err.Error(), "deeper than") {
		t.Fatalf("Schema error = %v, want the depth limit", err)
	}
}
