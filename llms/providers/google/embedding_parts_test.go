package google

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/auth"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// newVertexEmbed builds an embedding client on the Vertex backend pointed at a
// test server.
func newVertexEmbed(t *testing.T, model string, h http.HandlerFunc, extra ...Option) *EmbeddingClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	creds := auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: stubTokenProvider{}})
	opts := append([]Option{
		WithModel(model), WithVertex("p", "us-central1"), WithCredentials(creds),
		WithBaseURL(srv.URL), WithHTTPClient(srv.Client()), WithMaxInputBytes(1 << 20),
	}, extra...)
	c, err := NewEmbeddings(opts...)
	if err != nil {
		t.Fatalf("NewEmbeddings: %v", err)
	}
	return c
}

// vertexServer answers both Vertex embedding routes and records the request
// path and body. Predict gets one prediction per instance, each with the
// given statistics JSON (empty = none); :embedContent gets one embedding with
// the given usageMetadata JSON (empty = none).
type vertexServer struct {
	path, body string
	stats      []string
	usage      string
	calls      int
}

func (v *vertexServer) handle(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	v.path, v.body = r.URL.Path, string(b)
	v.calls++
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, ":embedContent") {
		out := `{"embedding":{"values":[0.5]}`
		if v.usage != "" {
			out += `,"usageMetadata":` + v.usage
		}
		_, _ = io.WriteString(w, out+"}")
		return
	}
	var req struct {
		Instances []json.RawMessage `json:"instances"`
	}
	_ = json.Unmarshal(b, &req)
	preds := make([]string, len(req.Instances))
	for i := range preds {
		e := `{"values":[0.25]`
		if i < len(v.stats) && v.stats[i] != "" {
			e += `,"statistics":` + v.stats[i]
		}
		preds[i] = `{"embeddings":` + e + `}}`
	}
	_, _ = io.WriteString(w, `{"predictions":[`+strings.Join(preds, ",")+`]}`)
}

func isBadRequest(err error) bool {
	var pe *llms.ProviderError
	return errors.As(err, &pe) && pe.Kind == llms.ErrorKindBadRequest
}

// ADR-0045 item 9: embedContentRoute agrees with the route genai v1.77
// actually takes, on bare names and on raw resource paths (where genai
// matches substrings of the project or location too). A genai bump that
// changes routing fails here.
func TestEmbedContentRouteMatchesSDK(t *testing.T) {
	cases := []struct {
		model string
		embed bool
	}{
		{"gemini-embedding-001", false},
		{"gemini-embedding-2", true},
		{"text-embedding-005", false},
		{"multimodalembedding@001", false},
		{"publishers/google/models/gemini-embedding-2", true},
		{"projects/p/locations/us-central1/publishers/google/models/gemini-embedding-001", true},
		{"projects/maas-prod/locations/us-central1/publishers/google/models/text-embedding-005", true},
		{"projects/gemini-team/locations/us-central1/publishers/google/models/text-embedding-005", true},
		{"projects/p/locations/us-central1/publishers/google/models/text-embedding-005", false},
	}
	for _, tc := range cases {
		if got := embedContentRoute(tc.model); got != tc.embed {
			t.Errorf("embedContentRoute(%q) = %v, want %v", tc.model, got, tc.embed)
		}
		v := &vertexServer{}
		c := newVertexEmbed(t, tc.model, v.handle)
		if _, err := c.Embed(context.Background(), llms.EmbeddingRequest{
			Inputs: []llms.EmbeddingInput{{Text: "x"}},
		}); err != nil {
			t.Fatalf("%s: Embed: %v", tc.model, err)
		}
		if got := strings.HasSuffix(v.path, ":embedContent"); got != tc.embed {
			t.Errorf("%s: genai sent %s; embedContentRoute says embedContent=%v", tc.model, v.path, tc.embed)
		}
	}
}

// ADR-0045 item 9: on Vertex every :embedContent-route model is single-input
// (typed bad_request, no call), and so is gemini-embedding-001 by base name;
// predict-route models still batch. On the Gemini API only
// gemini-embedding-001 is single-input.
func TestVertexSingleInputRule(t *testing.T) {
	two := llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{{ID: "1", Text: "a"}, {ID: "2", Text: "b"}}}
	for _, model := range []string{
		"gemini-embedding-2",
		"gemini-embedding-001",
		"projects/p/locations/us-central1/publishers/google/models/gemini-embedding-001",
		"projects/maas-prod/locations/us-central1/publishers/google/models/text-embedding-005",
	} {
		v := &vertexServer{}
		c := newVertexEmbed(t, model, v.handle)
		if _, err := c.Embed(context.Background(), two); !isBadRequest(err) {
			t.Errorf("%s: multi-input on Vertex must be bad_request, got %v", model, err)
		}
		if v.calls != 0 {
			t.Errorf("%s: provider called", model)
		}
	}

	v := &vertexServer{}
	c := newVertexEmbed(t, "text-embedding-005", v.handle)
	resp, err := c.Embed(context.Background(), two)
	if err != nil || len(resp.Vectors) != 2 {
		t.Fatalf("text-embedding-005 batch on Vertex: %+v / %v", resp, err)
	}

	var body string
	g := newEmbed(t, "gemini-embedding-2", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"embeddings":[{"values":[1]},{"values":[2]}]}`)
	})
	if resp, err := g.Embed(context.Background(), two); err != nil || len(resp.Vectors) != 2 {
		t.Fatalf("gemini-embedding-2 batch on the Gemini API: %+v / %v (%s)", resp, err, body)
	}
}

func mediaParts() []llms.ContentPart {
	return []llms.ContentPart{
		llms.Text("a red square"),
		llms.Image("image/PNG; x=1", []byte{0x89, 'P', 'N', 'G'}),
		llms.Document("application/pdf", []byte("%PDF-1.4")),
	}
}

// requestParts decodes the parts of the one content in a Vertex
// :embedContent body, or of the first request in a Gemini API
// batchEmbedContents body.
type wirePart struct {
	InlineData *struct {
		MIMEType string `json:"mimeType"`
		Data     string `json:"data"`
	} `json:"inlineData"`
	Text string `json:"text"`
}

func checkMediaWire(t *testing.T, parts []wirePart) {
	t.Helper()
	if len(parts) != 3 {
		t.Fatalf("want 3 parts in one content, got %d: %+v", len(parts), parts)
	}
	if parts[0].Text != "a red square" {
		t.Errorf("text part = %+v", parts[0])
	}
	img, doc := parts[1].InlineData, parts[2].InlineData
	if img == nil || img.MIMEType != "image/png" || img.Data != base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G'}) {
		t.Errorf("image part = %+v (want normalized image/png and the bytes)", img)
	}
	if doc == nil || doc.MIMEType != "application/pdf" || doc.Data != base64.StdEncoding.EncodeToString([]byte("%PDF-1.4")) {
		t.Errorf("document part = %+v", doc)
	}
}

// ADR-0045 items 4, 7, 11: on Vertex's :embedContent route the parts map 1:1
// into one content with Title and Task, and usage comes from usageMetadata.
func TestEmbedPartsVertexEmbedContent(t *testing.T) {
	v := &vertexServer{usage: `{"promptTokenCount":271}`}
	c := newVertexEmbed(t, "gemini-embedding-2", v.handle)
	resp, err := c.Embed(context.Background(), llms.EmbeddingRequest{
		Task:   llms.EmbeddingTaskRetrievalDocument,
		Inputs: []llms.EmbeddingInput{{ID: "m", Title: "T", Parts: mediaParts()}},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	var body struct {
		Content struct {
			Parts []wirePart `json:"parts"`
		} `json:"content"`
	}
	if derr := json.Unmarshal([]byte(v.body), &body); derr != nil {
		t.Fatalf("decode %s: %v", v.body, derr)
	}
	checkMediaWire(t, body.Content.Parts)
	for _, want := range []string{`"taskType":"RETRIEVAL_DOCUMENT"`, `"title":"T"`} {
		if !strings.Contains(v.body, want) {
			t.Errorf("body missing %s: %s", want, v.body)
		}
	}
	if len(resp.Vectors) != 1 || resp.Vectors[0].ID != "m" {
		t.Fatalf("vectors = %+v", resp.Vectors)
	}
	if resp.Usage.EmbeddingTokens != 271 || resp.Usage.TotalTokens != 271 {
		t.Fatalf("usage = %+v, want 271 from usageMetadata", resp.Usage)
	}

	// No usageMetadata: genai still sets (empty) statistics, so usage
	// stays zero rather than reporting 0 as a count.
	v = &vertexServer{}
	c = newVertexEmbed(t, "gemini-embedding-2", v.handle)
	resp, err = c.Embed(context.Background(), llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{{Parts: mediaParts()}}})
	if err != nil || resp.Usage != (llms.Usage{}) {
		t.Fatalf("no usageMetadata: usage %+v, err %v", resp.Usage, err)
	}
}

// ADR-0045 item 7: on the Gemini API the parts map 1:1 too; usage stays zero.
func TestEmbedPartsGeminiAPI(t *testing.T) {
	var raw string
	c := newEmbed(t, "gemini-embedding-2", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		raw = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"embeddings":[{"values":[1]}]}`)
	})
	resp, err := c.Embed(context.Background(), llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{{Parts: mediaParts()}}})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	var body struct {
		Requests []struct {
			Content struct {
				Parts []wirePart `json:"parts"`
			} `json:"content"`
		} `json:"requests"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil || len(body.Requests) != 1 {
		t.Fatalf("decode %s: %v", raw, err)
	}
	checkMediaWire(t, body.Requests[0].Content.Parts)
	if resp.Usage != (llms.Usage{}) {
		t.Fatalf("Gemini API reports no tokens; usage = %+v", resp.Usage)
	}
}

// ADR-0045 items 7, 11: on Vertex's predict route text Parts are joined into
// one text (genai would keep only the first), media is a pre-call
// bad_request, and usage sums the per-prediction statistics, rounded up.
func TestEmbedPartsVertexPredict(t *testing.T) {
	v := &vertexServer{stats: []string{`{"token_count":3,"truncated":false}`, `{"token_count":2.5}`}}
	c := newVertexEmbed(t, "text-embedding-005", v.handle)
	resp, err := c.Embed(context.Background(), llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{
		{ID: "p", Parts: []llms.ContentPart{llms.Text("a"), llms.Text("b")}},
		{ID: "t", Text: "c"},
	}})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	var body struct {
		Instances []struct {
			Content string `json:"content"`
		} `json:"instances"`
	}
	if derr := json.Unmarshal([]byte(v.body), &body); derr != nil {
		t.Fatalf("decode %s: %v", v.body, derr)
	}
	if len(body.Instances) != 2 || body.Instances[0].Content != "a\n\nb" || body.Instances[1].Content != "c" {
		t.Fatalf("predict instances = %+v, want joined text then plain text", body.Instances)
	}
	if resp.Usage.EmbeddingTokens != 6 || resp.Usage.TotalTokens != 6 {
		t.Fatalf("usage = %+v, want 3 + ceil(2.5) = 6", resp.Usage)
	}

	// One prediction without statistics: zero, not an undercount.
	v = &vertexServer{stats: []string{`{"token_count":3}`}}
	c = newVertexEmbed(t, "text-embedding-005", v.handle)
	resp, err = c.Embed(context.Background(), llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{{Text: "a"}, {Text: "b"}}})
	if err != nil || resp.Usage != (llms.Usage{}) {
		t.Fatalf("partial statistics: usage %+v, err %v", resp.Usage, err)
	}

	for _, p := range []llms.ContentPart{llms.Image("image/png", []byte("x")), llms.Document("application/pdf", []byte("x"))} {
		v = &vertexServer{}
		c = newVertexEmbed(t, "text-embedding-005", v.handle)
		_, err = c.Embed(context.Background(), llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{
			{Parts: []llms.ContentPart{llms.Text("a"), p}},
		}})
		if !isBadRequest(err) || v.calls != 0 {
			t.Fatalf("%s on predict: want pre-call bad_request, got %v (calls %d)", p.Type, err, v.calls)
		}
	}
}

// ADR-0045 item 8: MaxInputBytes measures the text as sent. On predict the
// joined separators count; on :embedContent text parts are summed and media
// bytes don't count.
func TestMaxInputBytesMeasuresTextAsSent(t *testing.T) {
	ab := []llms.EmbeddingInput{{Parts: []llms.ContentPart{llms.Text("a"), llms.Text("b")}}}

	v := &vertexServer{}
	c := newVertexEmbed(t, "text-embedding-005", v.handle, WithMaxInputBytes(2))
	if _, err := c.Embed(context.Background(), llms.EmbeddingRequest{Inputs: ab}); !isBadRequest(err) || v.calls != 0 {
		t.Fatalf("predict: \"a\\n\\nb\" is 4 bytes > 2; want pre-call bad_request, got %v", err)
	}
	c = newVertexEmbed(t, "text-embedding-005", v.handle, WithMaxInputBytes(4))
	if _, err := c.Embed(context.Background(), llms.EmbeddingRequest{Inputs: ab}); err != nil {
		t.Fatalf("predict: 4 bytes fits MaxInputBytes(4): %v", err)
	}

	big := llms.Image("image/png", make([]byte, 4096))
	v = &vertexServer{}
	c = newVertexEmbed(t, "gemini-embedding-2", v.handle, WithMaxInputBytes(2))
	if _, err := c.Embed(context.Background(), llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{
		{Parts: []llms.ContentPart{llms.Text("a"), llms.Text("b"), big}},
	}}); err != nil {
		t.Fatalf("embedContent: 2 text bytes fit and media doesn't count: %v", err)
	}
	if _, err := c.Embed(context.Background(), llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{
		{Parts: []llms.ContentPart{llms.Text("ab"), llms.Text("c"), big}},
	}}); !isBadRequest(err) {
		t.Fatalf("embedContent: 3 text bytes > 2; want bad_request, got %v", err)
	}
}

// ADR-0045 item 5: the adapter fails closed on malformed Parts without the
// validation middleware.
func TestEmbedPartsMalformedRejectedPreCall(t *testing.T) {
	for name, in := range map[string]llms.EmbeddingInput{
		"both":  {Text: "a", Parts: []llms.ContentPart{llms.Text("b")}},
		"empty": {Parts: []llms.ContentPart{llms.Text("")}},
		"type":  {Parts: []llms.ContentPart{llms.Image("text/plain", []byte("x"))}},
	} {
		v := &vertexServer{}
		c := newVertexEmbed(t, "gemini-embedding-2", v.handle)
		if _, err := c.Embed(context.Background(), llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{in}}); !isBadRequest(err) || v.calls != 0 {
			t.Fatalf("%s: want pre-call bad_request, got %v", name, err)
		}
	}
}
