package testllm

import (
	"context"
	"reflect"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// ADR-0045 item 12: Parts inputs hash every part deterministically, and the
// recorded request is a deep copy.
func TestFakeEmbeddingParts(t *testing.T) {
	f := &FakeEmbeddingClient{Vectors: map[string][]float32{"cap": {9}}}
	img := func(b byte) llms.ContentPart { return llms.Image("image/png", []byte{b}) }
	data := []byte{1}
	req := llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{
		{ID: "a", Parts: []llms.ContentPart{llms.Text("cap"), {Type: llms.ContentImage, Media: &llms.Media{MediaType: "image/png", Data: data}}}},
		{ID: "b", Parts: []llms.ContentPart{llms.Text("cap"), img(1)}},
		{ID: "c", Parts: []llms.ContentPart{llms.Text("cap"), img(2)}},
		{ID: "d", Parts: []llms.ContentPart{llms.Text("cap"), llms.Image("image/jpeg", []byte{1})}},
		{ID: "e", Parts: []llms.ContentPart{llms.Text("cap")}},
		{ID: "f", Text: "cap"},
	}}
	resp, err := f.Embed(context.Background(), req)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	v := resp.Vectors
	if len(v) != 6 || v[0].ID != "a" || v[5].ID != "f" {
		t.Fatalf("vectors/IDs wrong: %+v", v)
	}
	if !reflect.DeepEqual(v[0].Values, v[1].Values) {
		t.Error("equal parts must give equal vectors")
	}
	if reflect.DeepEqual(v[1].Values, v[2].Values) || reflect.DeepEqual(v[1].Values, v[3].Values) {
		t.Error("different bytes or media type must give different vectors")
	}
	if reflect.DeepEqual(v[4].Values, v[5].Values) || len(v[4].Values) != defaultFakeDimensions {
		t.Errorf("a Parts input is not looked up in Vectors: got %v", v[4].Values)
	}
	if !reflect.DeepEqual(v[5].Values, []float32{9}) {
		t.Errorf("Text input must still use Vectors: %v", v[5].Values)
	}

	data[0] = 7 // mutate the caller's bytes after the call
	got := f.Calls()[0].Inputs[0].Parts[1].Media.Data
	if got[0] != 1 {
		t.Fatal("recorded Parts share the caller's media bytes")
	}
}
