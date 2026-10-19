//go:build integration

package google_test

import (
	"context"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/auth/credentials"

	"github.com/SnapdragonPartners/maestro-llms/llms"
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
