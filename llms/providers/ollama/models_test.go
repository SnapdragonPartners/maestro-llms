package ollama

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// Compile-time assertion that *Client satisfies llms.ModelLister.
var _ llms.ModelLister = (*Client)(nil)

const tagsListJSON = `{
  "models": [
    {
      "name": "llama3.2:1b",
      "modified_at": "2024-10-04T16:43:24.13614Z",
      "size": 1321098329,
      "digest": "abc123",
      "details": {"family": "llama", "parameter_size": "1B", "quantization_level": "Q4_K_M"}
    },
    {
      "name": "mistral:7b-instruct",
      "modified_at": "2024-11-15T09:00:00Z",
      "size": 4100000000,
      "digest": "def456",
      "details": {"family": "mistral", "parameter_size": "7B", "quantization_level": "Q4_0"}
    }
  ]
}`

func TestListModels(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/tags" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		jsonHandler(t, 200, tagsListJSON)(w, r)
	})
	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != "llama3.2:1b" {
		t.Errorf("ID[0] = %q", got[0].ID)
	}
	// Family is empty by design for Ollama (see ADR-0012).
	if got[0].Family != "" {
		t.Errorf("Family = %q, want empty (Ollama has no family concept)", got[0].Family)
	}
	wantT := time.Date(2024, 10, 4, 16, 43, 24, 136140000, time.UTC)
	if !got[0].Created.Equal(wantT) {
		t.Errorf("Created = %v, want %v", got[0].Created, wantT)
	}
	if got[0].Raw == nil {
		t.Error("Raw should carry the wire entry")
	}
}

func TestListModelsHTTPError(t *testing.T) {
	c := newClient(t, jsonHandler(t, 500, `{"error":"db down"}`))
	_, err := c.ListModels(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *llms.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *llms.ProviderError, got %T: %v", err, err)
	}
}

func TestListModelsConnectionRefused(t *testing.T) {
	// Build a client pointing at a port nobody listens on.
	c, err := New(
		WithModel("test"),
		WithBaseURL("http://127.0.0.1:1"),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.ListModels(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	// Transport failure should be classified as a *llms.ProviderError; the
	// shared apierr classifier marks connection failures as retryable
	// unknown.
	var pe *llms.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *llms.ProviderError, got %T: %v", err, err)
	}
}

// TestNoLatestInFamilyMethod is a regression guard: ADR-0012 says Ollama
// implements ModelLister only and does NOT provide a LatestInFamily
// helper. If this ever changes, that's a structural decision that needs
// an ADR amendment, not a quiet add. The check is by type assertion to
// an anonymous interface that requires the method we deliberately
// haven't added.
func TestNoLatestInFamilyMethod(t *testing.T) {
	var c any = (*Client)(nil)
	if _, ok := c.(interface {
		LatestInFamily(context.Context, string) (llms.ModelInfo, bool, error)
	}); ok {
		t.Fatal("Ollama *Client unexpectedly implements LatestInFamily; ADR-0012 says it should NOT")
	}
}
