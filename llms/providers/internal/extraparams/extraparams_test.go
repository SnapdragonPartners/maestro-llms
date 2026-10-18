package extraparams

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

func TestMerge(t *testing.T) {
	dst := map[string]any{
		"model":            "m",
		"generationConfig": map[string]any{"temperature": 0.2, "maxOutputTokens": 100},
		"stop":             []any{"a"},
		"tools":            map[string]any{"x": 1},
		"keep":             true,
	}
	Merge(dst, map[string]any{
		"generationConfig": map[string]any{"maxOutputTokens": 5, "thinkingConfig": map[string]any{"thinkingLevel": "LOW"}},
		"stop":             []any{"b", "c"}, // arrays replace
		"tools":            nil,             // null replaces
		"model":            map[string]any{"name": "n"},
	})
	want := map[string]any{
		"model":            map[string]any{"name": "n"},
		"generationConfig": map[string]any{"temperature": 0.2, "maxOutputTokens": 5, "thinkingConfig": map[string]any{"thinkingLevel": "LOW"}},
		"stop":             []any{"b", "c"},
		"tools":            nil,
		"keep":             true,
	}
	if !reflect.DeepEqual(dst, want) {
		t.Fatalf("Merge = %v\nwant    %v", dst, want)
	}
}

func TestMergeJSONPreservesNumbersAndText(t *testing.T) {
	body := []byte(`{"seed":9007199254740993,"note":"a<b&c","n":1.5}`)
	got, err := MergeJSON(body, map[string]any{"extra": json.Number("12345678901234567890")})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"seed":9007199254740993`, `"note":"a<b&c"`, `"n":1.5`, `"extra":12345678901234567890`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("MergeJSON = %s, missing %s", got, want)
		}
	}
	if _, err := MergeJSON([]byte(`[1,2]`), map[string]any{"a": 1}); err == nil {
		t.Error("a non-object body must be an error")
	}
}

func TestOverrides(t *testing.T) {
	src := map[string]any{
		"thinking":      map[string]any{"display": "summarized"},
		"output_config": "replaced",
		"reasoning":     map[string]any{"effort": "low"},
	}
	for _, tc := range []struct {
		path []string
		want bool
	}{
		{[]string{"reasoning", "effort"}, true},           // exact leaf
		{[]string{"output_config", "effort"}, true},       // ancestor replaced by a non-object
		{[]string{"thinking", "type"}, false},             // sibling inside a merged object
		{[]string{"thinking"}, true},                      // the object itself is set
		{[]string{"absent", "x"}, false},                  // not touched
		{[]string{"reasoning", "effort", "deeper"}, true}, // ancestor replaced by a scalar
	} {
		if got := Overrides(src, tc.path...); got != tc.want {
			t.Errorf("Overrides(%v) = %v, want %v", tc.path, got, tc.want)
		}
	}
	if Overrides(nil, "x") {
		t.Error("nil src overrides nothing")
	}
}

func TestFor(t *testing.T) {
	type cfg struct {
		Type string `json:"type"`
	}
	req := llms.ChatRequest{ProviderParams: map[llms.ProviderID]map[string]any{
		llms.ProviderAnthropic: {"thinking": cfg{Type: "adaptive"}, "n": 3},
		llms.ProviderOpenAI:    {"bad": make(chan int)},
	}}
	got, ok, err := For(&req, llms.ProviderAnthropic)
	if err != nil || !ok {
		t.Fatalf("For: ok=%v err=%v", ok, err)
	}
	// A struct value is normalized to an object so it merges like one.
	if th, isMap := got["thinking"].(map[string]any); !isMap || th["type"] != "adaptive" {
		t.Fatalf("thinking = %#v, want a normalized object", got["thinking"])
	}
	if got["n"] != json.Number("3") {
		t.Fatalf("n = %#v, want json.Number 3", got["n"])
	}
	if _, ok, err := For(&req, llms.ProviderGoogle); ok || err != nil {
		t.Fatalf("no entry: ok=%v err=%v", ok, err)
	}
	if _, _, err := For(&req, llms.ProviderOpenAI); err == nil || !strings.Contains(err.Error(), "does not marshal") {
		t.Fatalf("non-marshalable value: err = %v", err)
	}
}

func TestMiddleware(t *testing.T) {
	mw := Middleware(map[string]any{"max_tokens": json.Number("7"), "extra": map[string]any{"a": true}}) //nolint:bodyclose // the caller (here, the test) closes the response
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://x", bytes.NewReader([]byte(`{"max_tokens":100,"model":"m"}`)))
	var sent []byte
	resp, err := mw(req, func(r *http.Request) (*http.Response, error) {
		sent, _ = io.ReadAll(r.Body)
		if r.ContentLength != int64(len(sent)) {
			t.Errorf("ContentLength = %d, body is %d bytes", r.ContentLength, len(sent))
		}
		again, _ := r.GetBody()
		if b, _ := io.ReadAll(again); !bytes.Equal(b, sent) {
			t.Error("GetBody must replay the merged body")
		}
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	var got map[string]any
	_ = json.Unmarshal(sent, &got)
	if got["max_tokens"] != float64(7) || got["model"] != "m" || got["extra"] == nil {
		t.Fatalf("merged body = %s", sent)
	}
}
