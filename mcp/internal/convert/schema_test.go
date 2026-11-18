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
			name: "percent-encoded fragment",
			in:   `{"type":"object","properties":{"a":{"$ref":"#/$defs/a%20b"},"b":{"$ref":"#/$defs/caf%C3%A9"},"c":{"$ref":"#/$defs/p%25"}},"$defs":{"a b":{"type":"string"},"café":{"type":"number"},"p%":{"type":"boolean"}}}`,
			want: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"number"},"c":{"type":"boolean"}}}`,
		},
		{
			name: "percent-encoded tilde escape",
			in:   `{"type":"object","properties":{"a":{"$ref":"#/$defs/x%7E1y"}},"$defs":{"x/y":{"type":"string"}}}`,
			want: `{"type":"object","properties":{"a":{"type":"string"}}}`,
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

// TestSchemaLiteralRefs: a "$ref" member inside literal data is data, not a
// reference. With no real reference the schema is unchanged; beside a real
// one, the literal survives the rewrite as it was.
func TestSchemaLiteralRefs(t *testing.T) {
	literals := []string{
		`{"type":"object","properties":{"a":{"type":"object","default":{"$ref":"literal-id"}}}}`,
		`{"type":"object","properties":{"a":{"const":{"$ref":"#/nowhere"}}}}`,
		`{"type":"object","properties":{"a":{"enum":[{"$ref":"x"},{"$ref":"#"}]}}}`,
		`{"type":"object","properties":{"a":{"type":"object","examples":[{"$ref":"https://e.test/s"}]}}}`,
		`{"type":"object","x-vendor":{"$ref":"#/missing"}}`,
		`{"type":"object","default":{"$ref":"#/properties"}}`,
	}
	for _, in := range literals {
		got, err := Schema(json.RawMessage(in))
		if err != nil {
			t.Errorf("Schema(%s): %v", in, err)
			continue
		}
		if string(got) != in {
			t.Errorf("literal $ref rewritten:\n got %s\nwant %s", got, in)
		}
	}

	in := `{"type":"object","properties":{
		"a":{"$ref":"#/$defs/A"},
		"b":{"type":"object","default":{"$ref":"#/$defs/A"},"const":{"$ref":"#/$defs/A"}}},
		"$defs":{"A":{"type":"string","examples":[{"$ref":"#/$defs/A"}]}}}`
	got, err := Schema(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	equalJSON(t, got, `{"type":"object","properties":{
		"a":{"type":"string","examples":[{"$ref":"#/$defs/A"}]},
		"b":{"type":"object","default":{"$ref":"#/$defs/A"},"const":{"$ref":"#/$defs/A"}}}}`)
}

// TestSchemaSubschemaKeywords: references are inlined under every keyword
// that holds subschemas.
func TestSchemaSubschemaKeywords(t *testing.T) {
	const s = `{"type":"string"}`
	cases := []struct{ name, in, want string }{
		{"additionalProperties", `{"additionalProperties":@R}`, `{"additionalProperties":@S}`},
		{"items schema", `{"properties":{"l":{"type":"array","items":@R}}}`, `{"properties":{"l":{"type":"array","items":@S}}}`},
		{"items list", `{"properties":{"l":{"type":"array","items":[@R,@R]}}}`, `{"properties":{"l":{"type":"array","items":[@S,@S]}}}`},
		{"prefixItems", `{"properties":{"l":{"prefixItems":[@R]}}}`, `{"properties":{"l":{"prefixItems":[@S]}}}`},
		{"anyOf", `{"properties":{"u":{"anyOf":[@R,{"type":"null"}]}}}`, `{"properties":{"u":{"anyOf":[@S,{"type":"null"}]}}}`},
		{"allOf oneOf not", `{"allOf":[@R],"oneOf":[@R],"not":@R}`, `{"allOf":[@S],"oneOf":[@S],"not":@S}`},
		{"if then else", `{"if":@R,"then":@R,"else":@R}`, `{"if":@S,"then":@S,"else":@S}`},
		{"patternProperties", `{"patternProperties":{"^x":@R}}`, `{"patternProperties":{"^x":@S}}`},
		{"dependentSchemas", `{"dependentSchemas":{"a":@R}}`, `{"dependentSchemas":{"a":@S}}`},
		{"dependencies", `{"dependencies":{"a":@R,"b":["c"]}}`, `{"dependencies":{"a":@S,"b":["c"]}}`},
		{"contains propertyNames", `{"propertyNames":@R,"properties":{"l":{"contains":@R}}}`, `{"propertyNames":@S,"properties":{"l":{"contains":@S}}}`},
		{"unevaluated", `{"unevaluatedProperties":@R,"unevaluatedItems":@R}`, `{"unevaluatedProperties":@S,"unevaluatedItems":@S}`},
		{"boolean schemas kept", `{"additionalProperties":false,"properties":{"a":true,"b":@R}}`, `{"additionalProperties":false,"properties":{"a":true,"b":@S}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wrap := func(body string) string {
				return `{"type":"object",` + body[1:len(body)-1] + `,"$defs":{"S":` + s + `}}`
			}
			in := wrap(strings.ReplaceAll(c.in, "@R", `{"$ref":"#/$defs/S"}`))
			want := `{"type":"object",` + strings.ReplaceAll(c.want, "@S", s)[1:]
			got, err := Schema(json.RawMessage(in))
			if err != nil {
				t.Fatal(err)
			}
			equalJSON(t, got, want)
		})
	}
}

func TestSchemaUnusedRecursiveDefOK(t *testing.T) {
	// Definitions are reached only through references, so an unused
	// recursive one is not an error.
	in := `{"type":"object","properties":{"a":{"$ref":"#/$defs/A"}},"$defs":{"A":{"type":"string"},"Node":{"type":"object","properties":{"next":{"$ref":"#/$defs/Node"}}}}}`
	got, err := Schema(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	equalJSON(t, got, `{"type":"object","properties":{"a":{"type":"string"}}}`)
}

func TestSchemaErrors(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"recursive", `{"type":"object","properties":{"n":{"$ref":"#/$defs/Node"}},"$defs":{"Node":{"type":"object","properties":{"next":{"$ref":"#/$defs/Node"}}}}}`, "recursive"},
		{"mutually recursive", `{"type":"object","properties":{"a":{"$ref":"#/$defs/A"}},"$defs":{"A":{"type":"object","properties":{"b":{"$ref":"#/$defs/B"}}},"B":{"type":"object","properties":{"a":{"$ref":"#/$defs/A"}}}}}`, "recursive"},
		{"recursive through two spellings", `{"type":"object","properties":{"n":{"$ref":"#/$defs/N"}},"$defs":{"N":{"type":"object","properties":{"next":{"$ref":"#/$defs/%4E"}}}}}`, "recursive"},
		{"root reference", `{"type":"object","properties":{"self":{"$ref":"#"}}}`, "recursive"},
		{"remote", `{"type":"object","properties":{"a":{"$ref":"https://example.com/s.json"}}}`, "unsupported reference"},
		{"relative file", `{"type":"object","properties":{"a":{"$ref":"other.json#/A"}}}`, "unsupported reference"},
		{"anchor", `{"type":"object","properties":{"a":{"$ref":"#thing"}}}`, "unsupported reference"},
		{"bad percent escape", `{"type":"object","properties":{"a":{"$ref":"#/$defs/a%zz"}},"$defs":{"a%zz":{"type":"string"}}}`, "not a valid URI fragment"},
		{"percent-encoded name not decoded twice", `{"type":"object","properties":{"a":{"$ref":"#/$defs/a%2520b"}},"$defs":{"a b":{"type":"string"}}}`, "does not resolve"},
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

func TestSchemaLiteralCopiesBounded(t *testing.T) {
	// A 100 KB enum referenced 50 times: 5 MB of copies, only 50 references.
	enum := make([]string, 100)
	for i := range enum {
		enum[i] = fmt.Sprintf("%q", strings.Repeat(string(rune('a'+i%26)), 1000))
	}
	props := make([]string, 50)
	for i := range props {
		props[i] = fmt.Sprintf(`"p%d":{"$ref":"#/$defs/Big"}`, i)
	}
	in := `{"type":"object","properties":{` + strings.Join(props, ",") + `},"$defs":{"Big":{"enum":[` + strings.Join(enum, ",") + `]}}}`
	_, err := Schema(json.RawMessage(in))
	if err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("Schema error = %v, want the size bound", err)
	}

	// The same literal with no references is sent unchanged, whatever its size.
	big := `{"type":"object","properties":{"a":{"enum":[` + strings.Repeat(strings.Join(enum, ",")+",", 50) + `"z"]}}}`
	got, err := Schema(json.RawMessage(big))
	if err != nil || string(got) != big {
		t.Fatalf("large schema without references: err=%v, changed=%v", err, string(got) != big)
	}
}

func TestSchemaNestedResources(t *testing.T) {
	bad := []struct{ name, in string }{
		{"ref inside nested $id", `{"type":"object","properties":{"a":{"$id":"https://x.test/a","$defs":{"X":{"type":"integer"}},"type":"object","properties":{"b":{"$ref":"#/$defs/X"}}}},"$defs":{"X":{"type":"string"}}}`},
		{"ref beside nested $id", `{"type":"object","properties":{"a":{"$id":"https://x.test/a","$ref":"#/$defs/X"}},"$defs":{"X":{"type":"string"}}}`},
		{"target is a nested resource with refs", `{"type":"object","properties":{"a":{"$ref":"#/$defs/R"}},"$defs":{"X":{"type":"string"},"R":{"$id":"https://x.test/r","type":"object","properties":{"b":{"$ref":"#/$defs/X"}}}}}`},
		{"pointer through a nested resource", `{"type":"object","properties":{"a":{"$ref":"#/$defs/R/properties/b"}},"$defs":{"X":{"type":"string"},"R":{"$id":"https://x.test/r","type":"object","properties":{"b":{"type":"object","properties":{"c":{"$ref":"#/$defs/X"}}}}}}}`},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			_, err := Schema(json.RawMessage(c.in))
			if err == nil || !strings.Contains(err.Error(), "nested schema resource") {
				t.Fatalf("Schema error = %v, want the nested-resource error", err)
			}
		})
	}

	t.Run("root $id is fine", func(t *testing.T) {
		got, err := Schema(json.RawMessage(`{"$id":"https://x.test/root","type":"object","properties":{"a":{"$ref":"#/$defs/X"}},"$defs":{"X":{"type":"string"}}}`))
		if err != nil {
			t.Fatal(err)
		}
		equalJSON(t, got, `{"$id":"https://x.test/root","type":"object","properties":{"a":{"type":"string"}}}`)
	})
	t.Run("nested $id without refs inside is kept", func(t *testing.T) {
		got, err := Schema(json.RawMessage(`{"type":"object","properties":{"a":{"$ref":"#/$defs/X"},"r":{"$id":"https://x.test/r","type":"string"}},"$defs":{"X":{"type":"string"}}}`))
		if err != nil {
			t.Fatal(err)
		}
		equalJSON(t, got, `{"type":"object","properties":{"a":{"type":"string"},"r":{"$id":"https://x.test/r","type":"string"}}}`)
	})
	t.Run("pointer to a resource without refs", func(t *testing.T) {
		got, err := Schema(json.RawMessage(`{"type":"object","properties":{"a":{"$ref":"#/$defs/R"}},"$defs":{"R":{"$id":"https://x.test/r","type":"string"}}}`))
		if err != nil {
			t.Fatal(err)
		}
		equalJSON(t, got, `{"type":"object","properties":{"a":{"$id":"https://x.test/r","type":"string"}}}`)
	})
}

// TestSchemaUnusedLargeDefsStable: a large definitions block that nothing
// references isn't counted against the bound (only what inlining copies
// is), so a schema with one small reference is accepted on every run,
// whatever order map keys are walked in.
func TestSchemaUnusedLargeDefsStable(t *testing.T) {
	enum := make([]string, 150)
	for i := range enum {
		enum[i] = fmt.Sprintf("%q", strings.Repeat("x", 30_000)) // 4.5 MB in all, over maxBytes
	}
	in := `{"type":"object","properties":{"a":{"$ref":"#/$defs/Small"}},"$defs":{"Small":{"type":"string"},"Huge":{"enum":[` + strings.Join(enum, ",") + `]}}}`
	// The root's keys are walked in random order; before the fix, $defs
	// walked ahead of properties failed about half the time.
	for range 8 {
		got, err := Schema(json.RawMessage(in))
		if err != nil {
			t.Fatalf("Schema: %v", err)
		}
		equalJSON(t, got, `{"type":"object","properties":{"a":{"type":"string"}}}`)
	}
	// Referenced, the same literal is a copy and trips the bound.
	ref := strings.Replace(in, `#/$defs/Small`, `#/$defs/Huge`, 1)
	if _, err := Schema(json.RawMessage(ref)); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("Schema error = %v, want the size bound", err)
	}
}

// TestSchemaNestedDefinitionsDropped: once anything is inlined, no
// definition is used any more, so nested $defs / definitions are dropped
// too and no reference inside them reaches the provider.
func TestSchemaNestedDefinitionsDropped(t *testing.T) {
	in := `{"type":"object","properties":{
		"a":{"$ref":"#/$defs/A"},
		"b":{"type":"object","$defs":{"Z":{"$ref":"#/$defs/A"},"Loop":{"properties":{"n":{"$ref":"#/properties/b/$defs/Loop"}}}},"properties":{"z":{"$ref":"#/properties/b/$defs/Z"}}},
		"c":{"type":"object","definitions":{"Y":{"type":"integer"}},"properties":{"y":{"$ref":"#/properties/c/definitions/Y"}}}},
		"$defs":{"A":{"type":"string"}}}`
	got, err := Schema(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	equalJSON(t, got, `{"type":"object","properties":{
		"a":{"type":"string"},
		"b":{"type":"object","properties":{"z":{"type":"string"}}},
		"c":{"type":"object","properties":{"y":{"type":"integer"}}}}}`)
	if strings.Contains(string(got), "$ref") || strings.Contains(string(got), "$defs") || strings.Contains(string(got), "definitions") {
		t.Errorf("reference or definitions left in %s", got)
	}

	// A property literally named "$defs" is a property, not definitions.
	got, err = Schema(json.RawMessage(`{"type":"object","properties":{"$defs":{"$ref":"#/$defs/A"}},"$defs":{"A":{"type":"string"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	equalJSON(t, got, `{"type":"object","properties":{"$defs":{"type":"string"}}}`)
}
