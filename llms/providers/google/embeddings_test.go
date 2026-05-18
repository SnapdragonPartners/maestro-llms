package google

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/auth"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

var _ llms.EmbeddingClient = (*EmbeddingClient)(nil)

// newEmbed builds an embedding client on the Gemini-API backend pointed at a
// test server (the request/response mapping is backend-agnostic).
func newEmbed(t *testing.T, model string, h http.HandlerFunc, mut func(*EmbeddingConfig)) *EmbeddingClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := EmbeddingConfig{
		Model: model, APIKey: "k", Endpoint: srv.URL, HTTPClient: srv.Client(),
		MaxInputBytes: 1 << 20, // guard configured (effectively off for tiny test inputs)
	}
	if mut != nil {
		mut(&cfg)
	}
	c, err := NewEmbeddings(cfg)
	if err != nil {
		t.Fatalf("NewEmbeddings: %v", err)
	}
	return c
}

func embedOK(values string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"embeddings":[`+values+`]}`)
	}
}

func TestEmbedMapsTaskTitleDimsAndPreservesID(t *testing.T) {
	var body string
	c := newEmbed(t, "gemini-embedding-001", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"embeddings":[{"values":[0.5,0.6]}]}`)
	}, nil)

	resp, err := c.Embed(context.Background(), llms.EmbeddingRequest{
		Task:       llms.EmbeddingTaskRetrievalDocument,
		Dimensions: 256,
		Inputs:     []llms.EmbeddingInput{{ID: "a", Text: "x", Title: "T"}},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	for _, want := range []string{`"taskType":"RETRIEVAL_DOCUMENT"`, `"title":"T"`, `"outputDimensionality":256`} {
		if !strings.Contains(body, want) {
			t.Fatalf("request body missing %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, `"autoTruncate":true`) {
		t.Fatalf("autoTruncate must default off: %s", body)
	}
	if len(resp.Vectors) != 1 || resp.Vectors[0].ID != "a" ||
		len(resp.Vectors[0].Values) != 2 || resp.Vectors[0].Values[0] != 0.5 {
		t.Fatalf("vector mapping wrong: %+v", resp.Vectors)
	}
}

func TestGeminiEmbedding001RejectsMultiInput(t *testing.T) {
	called := false
	c := newEmbed(t, "gemini-embedding-001", func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = io.WriteString(w, "{}")
	}, nil)
	_, err := c.Embed(context.Background(), llms.EmbeddingRequest{
		Inputs: []llms.EmbeddingInput{{ID: "1", Text: "a"}, {ID: "2", Text: "b"}},
	})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
		t.Fatalf("multi-input to gemini-embedding-001 must be bad_request, got %v", err)
	}
	if called {
		t.Fatal("provider must not be called (no fan-out / chunking exception)")
	}
}

func TestEmbedEmptyInputsRejected(t *testing.T) {
	called := false
	c := newEmbed(t, "gemini-embedding-001", func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = io.WriteString(w, "{}")
	}, nil)
	_, err := c.Embed(context.Background(), llms.EmbeddingRequest{})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
		t.Fatalf("empty inputs must be bad_request, got %v", err)
	}
	if called {
		t.Fatal("provider must not be called for empty inputs")
	}
}

func TestMaxInputBytesGuardRejectsOversized(t *testing.T) {
	called := false
	c := newEmbed(t, "gemini-embedding-001", func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = io.WriteString(w, `{"embeddings":[{"values":[1]}]}`)
	}, func(cfg *EmbeddingConfig) { cfg.MaxInputBytes = 4 })

	_, err := c.Embed(context.Background(), llms.EmbeddingRequest{
		Inputs: []llms.EmbeddingInput{{ID: "1", Text: "toolong"}}, // 7 bytes > 4
	})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
		t.Fatalf("oversized input must be bad_request, got %v", err)
	}
	if called {
		t.Fatal("oversized input must be rejected before the provider call")
	}
	// Within the limit succeeds.
	if _, err := c.Embed(context.Background(), llms.EmbeddingRequest{
		Inputs: []llms.EmbeddingInput{{ID: "1", Text: "ok"}}, // 2 bytes <= 4
	}); err != nil {
		t.Fatalf("within-limit input must succeed, got %v", err)
	}
}

func TestAutoTruncateConstructionRules(t *testing.T) {
	cfgErr := func(cfg EmbeddingConfig, what string) {
		_, err := NewEmbeddings(cfg)
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindConfig {
			t.Fatalf("%s: want config error, got %v", what, err)
		}
	}
	creds := auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: stubTokenProvider{}})

	// false + no guard => must fail (no genai-representable autoTruncate:false).
	cfgErr(EmbeddingConfig{Model: "m", APIKey: "k"}, "false + no MaxInputBytes")
	// true on the Gemini-API backend => Vertex-only, must fail.
	cfgErr(EmbeddingConfig{Model: "m", APIKey: "k", AutoTruncate: true}, "AutoTruncate on Gemini API")

	// false + guard => ok.
	if _, err := NewEmbeddings(EmbeddingConfig{Model: "m", APIKey: "k", MaxInputBytes: 10}); err != nil {
		t.Fatalf("false + MaxInputBytes must construct: %v", err)
	}
	// true on Vertex => ok.
	if _, err := NewEmbeddings(EmbeddingConfig{
		Model: "m", Project: "p", Location: "us", Credentials: creds, AutoTruncate: true,
	}); err != nil {
		t.Fatalf("AutoTruncate=true on Vertex must construct: %v", err)
	}
	// Vertex, false, no guard => fail.
	cfgErr(EmbeddingConfig{Model: "m", Project: "p", Location: "us", Credentials: creds},
		"Vertex false + no MaxInputBytes")
}

func TestAutoTruncateTrueVertexPassesThrough(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"predictions":[{"embeddings":{"values":[0.1,0.2]}}]}`)
	}))
	defer srv.Close()
	creds := auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: stubTokenProvider{}})
	c, err := NewEmbeddings(EmbeddingConfig{
		Model: "gemini-embedding-001", Project: "p", Location: "us-central1",
		Credentials: creds, Endpoint: srv.URL, HTTPClient: srv.Client(), AutoTruncate: true,
	})
	if err != nil {
		t.Fatalf("NewEmbeddings: %v", err)
	}
	// Oversized but AutoTruncate=true => guard skipped, request goes through.
	resp, err := c.Embed(context.Background(), llms.EmbeddingRequest{
		Inputs: []llms.EmbeddingInput{{ID: "a", Text: strings.Repeat("x", 100000)}},
	})
	if err != nil {
		t.Fatalf("Embed (Vertex, autoTruncate): %v", err)
	}
	if !strings.Contains(body, `"autoTruncate":true`) {
		t.Fatalf("AutoTruncate=true must reach Vertex: %s", body)
	}
	if len(resp.Vectors) != 1 || resp.Vectors[0].ID != "a" || resp.Vectors[0].Values[0] != float32(0.1) {
		t.Fatalf("Vertex embedding mapping wrong: %+v", resp.Vectors)
	}
}

func TestEmbedOrderIDsAndCountMismatch(t *testing.T) {
	c := newEmbed(t, "text-embedding-004",
		embedOK(`{"values":[1,1]},{"values":[2,2]}`), nil)
	resp, err := c.Embed(context.Background(), llms.EmbeddingRequest{
		Inputs: []llms.EmbeddingInput{{ID: "x", Text: "a"}, {ID: "y", Text: "b"}},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if resp.Vectors[0].ID != "x" || resp.Vectors[1].ID != "y" ||
		resp.Vectors[1].Values[0] != 2 {
		t.Fatalf("order/IDs not preserved: %+v", resp.Vectors)
	}

	// Provider returns fewer vectors than inputs -> typed error.
	c2 := newEmbed(t, "text-embedding-004", embedOK(`{"values":[1]}`), nil)
	_, err = c2.Embed(context.Background(), llms.EmbeddingRequest{
		Inputs: []llms.EmbeddingInput{{ID: "x", Text: "a"}, {ID: "y", Text: "b"}},
	})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
		t.Fatalf("count mismatch must be bad_request, got %v", err)
	}
}

type stubTokenProvider struct{}

func (stubTokenProvider) Token(context.Context) (*auth.Token, error) {
	return &auth.Token{Value: "stub"}, nil
}

func TestNewEmbeddingsConfigValidation(t *testing.T) {
	bad := func(cfg EmbeddingConfig, what string) {
		_, err := NewEmbeddings(cfg)
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindConfig {
			t.Fatalf("%s: want config *ProviderError, got %v", what, err)
		}
	}
	bad(EmbeddingConfig{}, "missing model")
	bad(EmbeddingConfig{Model: "m"}, "no backend")
	bad(EmbeddingConfig{Model: "m", Project: "p"}, "vertex missing location")
	bad(EmbeddingConfig{Model: "m", Project: "p", Location: "us"}, "vertex missing credentials")

	// Valid backends construct (truncation contract satisfied via a guard).
	if _, err := NewEmbeddings(EmbeddingConfig{Model: "m", APIKey: "k", MaxInputBytes: 8000}); err != nil {
		t.Fatalf("Gemini-API config must construct: %v", err)
	}
	creds := auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: stubTokenProvider{}})
	if _, err := NewEmbeddings(EmbeddingConfig{
		Model: "gemini-embedding-001", Project: "p", Location: "us-central1",
		Credentials: creds, MaxInputBytes: 8000,
	}); err != nil {
		t.Fatalf("Vertex config must construct: %v", err)
	}
}
