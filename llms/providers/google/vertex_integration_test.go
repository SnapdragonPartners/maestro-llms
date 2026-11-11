//go:build integration

package google_test

import (
	"context"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/auth/credentials"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/google"
)

// Live Gemini-embeddings-on-Vertex test. Skipped unless VERTEX_PROJECT is set.
// Like anthropicvertex's live test, it loads Application Default Credentials
// itself and passes them in: the package does no ADC discovery (ADR-0009).
//
//	VERTEX_PROJECT                 required
//	VERTEX_EMBEDDINGS_LOCATION     default "us-central1"
//	VERTEX_EMBEDDINGS_MODEL        default "gemini-embedding-001"
func TestIntegrationVertexGeminiEmbeddings(t *testing.T) {
	project := os.Getenv("VERTEX_PROJECT")
	if project == "" {
		t.Skip("VERTEX_PROJECT not set")
	}
	location := os.Getenv("VERTEX_EMBEDDINGS_LOCATION")
	if location == "" {
		location = "us-central1"
	}
	model := os.Getenv("VERTEX_EMBEDDINGS_MODEL")
	if model == "" {
		model = "gemini-embedding-001"
	}
	creds, err := credentials.DetectDefault(&credentials.DetectOptions{
		Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
	})
	if err != nil {
		t.Skipf("no Google credentials (run `gcloud auth application-default login`): %v", err)
	}
	c, err := google.NewEmbeddings(
		google.WithModel(model),
		google.WithVertex(project, location),
		google.WithCredentials(creds),
		google.WithMaxInputBytes(8192),
	)
	if err != nil {
		t.Fatalf("NewEmbeddings: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := c.Embed(ctx, llms.EmbeddingRequest{
		Inputs: []llms.EmbeddingInput{{Text: "The quick brown fox jumps over the lazy dog."}},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(resp.Vectors) != 1 || len(resp.Vectors[0].Values) == 0 {
		t.Fatalf("expected one non-empty vector, got %d vectors", len(resp.Vectors))
	}
	t.Logf("OK Vertex embeddings: project=%s location=%s model=%s dims=%d", project, location, model, len(resp.Vectors[0].Values))
}

// vertexEmbedClient builds a live Vertex embedding client for model, or skips.
func vertexEmbedClient(t *testing.T, model string) *google.EmbeddingClient {
	t.Helper()
	project := os.Getenv("VERTEX_PROJECT")
	if project == "" {
		t.Skip("VERTEX_PROJECT not set")
	}
	location := os.Getenv("VERTEX_EMBEDDINGS_LOCATION")
	if location == "" {
		location = "us-central1"
	}
	creds, err := credentials.DetectDefault(&credentials.DetectOptions{
		Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
	})
	if err != nil {
		t.Skipf("no Google credentials (run `gcloud auth application-default login`): %v", err)
	}
	c, err := google.NewEmbeddings(
		google.WithModel(model),
		google.WithVertex(project, location),
		google.WithCredentials(creds),
		google.WithMaxInputBytes(8192),
	)
	if err != nil {
		t.Fatalf("NewEmbeddings: %v", err)
	}
	t.Logf("Vertex embeddings: project=%s location=%s model=%s", project, location, model)
	return c
}

// Image and document embedding inputs on Vertex (ADR-0045).
//
//	VERTEX_MULTIMODAL_EMBEDDINGS_MODEL   default "gemini-embedding-2"
func TestIntegrationVertexEmbeddingMedia(t *testing.T) {
	model := os.Getenv("VERTEX_MULTIMODAL_EMBEDDINGS_MODEL")
	if model == "" {
		model = "gemini-embedding-2"
	}
	livetest.EmbeddingMedia(t, vertexEmbedClient(t, model))
}

// Text Parts on Vertex's predict route are joined (ADR-0045 item 7), and
// usage is reported from the prediction statistics (item 11).
//
//	VERTEX_PREDICT_EMBEDDINGS_MODEL   default "text-embedding-005"
func TestIntegrationVertexEmbeddingJoinedText(t *testing.T) {
	model := os.Getenv("VERTEX_PREDICT_EMBEDDINGS_MODEL")
	if model == "" {
		model = "text-embedding-005"
	}
	livetest.EmbeddingJoinedText(t, vertexEmbedClient(t, model))
}
