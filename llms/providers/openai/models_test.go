package openai

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// Compile-time assertion that *ChatClient satisfies llms.ModelLister.
var _ llms.ModelLister = (*ChatClient)(nil)

// modelsListJSON mimics the GET /v1/models response shape OpenAI returns.
// It mixes chat, embeddings, image, and dated snapshots so we exercise
// the self-filtering behavior: querying gpt-5 must not pull in embedding
// models even though they share the catalog.
const modelsListJSON = `{
  "object": "list",
  "data": [
    {"id":"gpt-5-2026-03-15","object":"model","created":1742000000,"owned_by":"openai"},
    {"id":"gpt-5-2026-01-01","object":"model","created":1735689600,"owned_by":"openai"},
    {"id":"gpt-5","object":"model","created":1742000000,"owned_by":"openai"},
    {"id":"gpt-5-mini-2025-12-01","object":"model","created":1733011200,"owned_by":"openai"},
    {"id":"gpt-4o-2024-08-06","object":"model","created":1722902400,"owned_by":"openai"},
    {"id":"o1-preview-2024-09-12","object":"model","created":1726099200,"owned_by":"openai"},
    {"id":"text-embedding-3-small","object":"model","created":1700000000,"owned_by":"openai"},
    {"id":"dall-e-3","object":"model","created":1698000000,"owned_by":"openai"}
  ]
}`

func TestFamilyOf(t *testing.T) {
	cases := []struct {
		id, want string
	}{
		{"gpt-5-2026-03-15", "gpt-5"},
		{"gpt-5", "gpt-5"},
		{"gpt-5-mini-2025-12-01", "gpt-5-mini"},
		{"gpt-4o-2024-08-06", "gpt-4o"},
		{"gpt-4o-mini", "gpt-4o-mini"},
		{"o1-preview-2024-09-12", "o1-preview"},
		{"o3-mini", "o3-mini"},
		// Non-chat IDs still get a family — that's fine because family-prefix
		// match in LatestInFamily prevents cross-modality bleed.
		{"text-embedding-3-small", "text-embedding-3-small"},
		{"dall-e-3", "dall-e-3"},
		// Empty stays empty.
		{"", ""},
	}
	for _, tc := range cases {
		if got := familyOf(tc.id); got != tc.want {
			t.Errorf("familyOf(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestListModels(t *testing.T) {
	c := newChat(t, jsonHandler(t, 200, modelsListJSON, nil))
	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 8 {
		t.Fatalf("len = %d, want 8", len(got))
	}
	// Spot-check: families classified, Created parsed from Unix seconds.
	byID := map[string]llms.ModelInfo{}
	for _, m := range got {
		byID[m.ID] = m
	}
	if byID["gpt-5-2026-03-15"].Family != "gpt-5" {
		t.Errorf("gpt-5 dated Family = %q", byID["gpt-5-2026-03-15"].Family)
	}
	if byID["gpt-4o-2024-08-06"].Family != "gpt-4o" {
		t.Errorf("gpt-4o Family = %q", byID["gpt-4o-2024-08-06"].Family)
	}
	wantT := time.Unix(1742000000, 0).UTC()
	if !byID["gpt-5-2026-03-15"].Created.Equal(wantT) {
		t.Errorf("Created = %v, want %v", byID["gpt-5-2026-03-15"].Created, wantT)
	}
	if byID["gpt-5-2026-03-15"].Raw == nil {
		t.Error("Raw should carry the SDK payload")
	}
}

func TestListModelsAPIError(t *testing.T) {
	c := newChat(t, jsonHandler(t, 401, `{"error":{"message":"invalid key"}}`, nil))
	_, err := c.ListModels(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *llms.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *llms.ProviderError, got %T: %v", err, err)
	}
	if pe.Kind != llms.ErrorKindAuth {
		t.Errorf("Kind = %q, want auth", pe.Kind)
	}
}

func TestLatestInFamilyPureHelper(t *testing.T) {
	models := []llms.ModelInfo{
		{ID: "gpt-5-2026-01-01", Family: "gpt-5", Created: time.Unix(1735689600, 0).UTC()},
		{ID: "gpt-5-2026-03-15", Family: "gpt-5", Created: time.Unix(1742000000, 0).UTC()},
		{ID: "gpt-5-mini-2025-12-01", Family: "gpt-5-mini", Created: time.Unix(1733011200, 0).UTC()},
		{ID: "gpt-4o-2024-08-06", Family: "gpt-4o", Created: time.Unix(1722902400, 0).UTC()},
		{ID: "text-embedding-3-small", Family: "text-embedding-3-small", Created: time.Unix(1700000000, 0).UTC()},
	}

	t.Run("newer gpt-5 snapshot available", func(t *testing.T) {
		newer, ok := LatestInFamily("gpt-5-2026-01-01", models)
		if !ok {
			t.Fatal("expected newer found")
		}
		if newer.ID != "gpt-5-2026-03-15" {
			t.Errorf("ID = %q", newer.ID)
		}
	})

	t.Run("already on latest", func(t *testing.T) {
		_, ok := LatestInFamily("gpt-5-2026-03-15", models)
		if ok {
			t.Fatal("expected false (already newest)")
		}
	})

	t.Run("cross-modality does not bleed", func(t *testing.T) {
		// A gpt-5 caller must not be told "upgrade to text-embedding-3-small"
		// just because it's in the same catalog — different family entirely.
		newer, ok := LatestInFamily("gpt-5-2026-01-01", models)
		if !ok || newer.Family != "gpt-5" {
			t.Fatalf("got family=%q want gpt-5", newer.Family)
		}
	})

	t.Run("mini stays in mini family", func(t *testing.T) {
		// gpt-5-mini should NOT upgrade to gpt-5 (different family).
		_, ok := LatestInFamily("gpt-5-mini-2025-12-01", models)
		if ok {
			t.Fatal("expected false (no newer mini)")
		}
	})

	t.Run("empty list", func(t *testing.T) {
		_, ok := LatestInFamily("gpt-5-2026-01-01", nil)
		if ok {
			t.Fatal("expected false on empty list")
		}
	})
}

func TestClientLatestInFamilyOneShot(t *testing.T) {
	c := newChat(t, jsonHandler(t, 200, modelsListJSON, nil))
	newer, ok, err := c.LatestInFamily(context.Background(), "gpt-5-2026-01-01")
	if err != nil {
		t.Fatalf("LatestInFamily: %v", err)
	}
	if !ok {
		t.Fatal("expected newer found")
	}
	if newer.ID != "gpt-5-2026-03-15" {
		t.Errorf("ID = %q", newer.ID)
	}
	if newer.Family != "gpt-5" {
		t.Errorf("Family = %q", newer.Family)
	}
}
