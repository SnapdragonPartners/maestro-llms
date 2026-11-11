package livetest

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
)

// embedOne embeds a single input and checks the response: one vector with
// the input's ID, and, when the client reports usage, DefaultEstimator's
// estimate is at least the reported tokens (ADR-0045 item 10, biased high).
func embedOne(t *testing.T, c llms.EmbeddingClient, in llms.EmbeddingInput) llms.EmbeddingResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req := llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{in}}
	resp, err := c.Embed(ctx, req)
	if err != nil {
		t.Fatalf("Embed(%s): %v", in.ID, err)
	}
	if len(resp.Vectors) != 1 || resp.Vectors[0].ID != in.ID || len(resp.Vectors[0].Values) == 0 {
		t.Fatalf("Embed(%s): want one non-empty vector with the input ID, got %d vectors", in.ID, len(resp.Vectors))
	}
	if est := (middleware.DefaultEstimator{}).EstimateEmbeddings(req).InputTokens; est < resp.Usage.EmbeddingTokens {
		t.Errorf("Embed(%s): estimate %d is below the reported %d tokens", in.ID, est, resp.Usage.EmbeddingTokens)
	}
	t.Logf("%s: dims=%d usage=%+v", in.ID, len(resp.Vectors[0].Values), resp.Usage)
	return resp
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range min(len(a), len(b)) {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// EmbeddingMedia checks a multimodal embedding client (ADR-0045): an image,
// an image with a caption, and a PDF each give one vector with reported
// usage; the image lands nearer a description of itself than unrelated
// text (so the bytes were embedded, not dropped); and a two-input request
// is a bad_request before the call (the model takes one content per call).
func EmbeddingMedia(t *testing.T, c llms.EmbeddingClient) {
	t.Helper()
	img := llms.Image("image/png", redPNG(t, 256))

	image := embedOne(t, c, llms.EmbeddingInput{ID: "image", Parts: []llms.ContentPart{img}})
	captioned := embedOne(t, c, llms.EmbeddingInput{ID: "image+text", Parts: []llms.ContentPart{
		llms.Text("A solid red square."), img,
	}})
	pdf := embedOne(t, c, llms.EmbeddingInput{ID: "pdf", Parts: []llms.ContentPart{
		llms.Document("application/pdf", codeWordPDF()),
	}})
	for _, r := range []*llms.EmbeddingResponse{&image, &captioned, &pdf} {
		if r.Usage.EmbeddingTokens <= 0 {
			t.Errorf("want reported EmbeddingTokens on a media input, got %+v", r.Usage)
		}
	}

	red := embedOne(t, c, llms.EmbeddingInput{ID: "red", Text: "A solid red square."})
	other := embedOne(t, c, llms.EmbeddingInput{ID: "other", Text: "Instructions for filing a quarterly sales tax return."})
	near := cosine(image.Vectors[0].Values, red.Vectors[0].Values)
	far := cosine(image.Vectors[0].Values, other.Vectors[0].Values)
	t.Logf("cosine(image, red text)=%.4f cosine(image, unrelated text)=%.4f", near, far)
	if near <= far {
		t.Errorf("the image should be nearer \"a solid red square\" (%.4f) than unrelated text (%.4f)", near, far)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := c.Embed(ctx, llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{{Text: "a"}, {Text: "b"}}})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
		t.Errorf("two inputs: want a pre-call bad_request, got %v", err)
	}
}

// EmbeddingJoinedText checks a text-only route (ADR-0045 item 7, Vertex
// predict): text Parts give the same vector as the same text joined by a
// blank line in Text, with reported usage.
func EmbeddingJoinedText(t *testing.T, c llms.EmbeddingClient) {
	t.Helper()
	parts := embedOne(t, c, llms.EmbeddingInput{ID: "parts", Parts: []llms.ContentPart{
		llms.Text("The quick brown fox."), llms.Text("It jumps over the lazy dog."),
	}})
	text := embedOne(t, c, llms.EmbeddingInput{ID: "text", Text: "The quick brown fox.\n\nIt jumps over the lazy dog."})
	if s := cosine(parts.Vectors[0].Values, text.Vectors[0].Values); s < 0.9999 {
		t.Errorf("joined Parts and the same Text should embed alike; cosine = %.6f", s)
	}
	if parts.Usage.EmbeddingTokens <= 0 || parts.Usage.EmbeddingTokens != text.Usage.EmbeddingTokens {
		t.Errorf("want equal positive usage, got %+v and %+v", parts.Usage, text.Usage)
	}
}
