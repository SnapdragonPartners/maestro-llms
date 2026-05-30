package anthropic

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

const modelsListJSON = `{
  "data": [
    {"id":"claude-opus-4-7-20251015","type":"model","display_name":"Claude Opus 4.7","created_at":"2025-10-15T00:00:00Z"},
    {"id":"claude-opus-4-5-20251201","type":"model","display_name":"Claude Opus 4.5","created_at":"2025-09-01T00:00:00Z"},
    {"id":"claude-sonnet-4-5-20251015","type":"model","display_name":"Claude Sonnet 4.5","created_at":"2025-10-15T00:00:00Z"},
    {"id":"claude-haiku-4-5-20251001","type":"model","display_name":"Claude Haiku 4.5","created_at":"2025-10-01T00:00:00Z"},
    {"id":"claude-3-5-sonnet-20240620","type":"model","display_name":"Claude 3.5 Sonnet","created_at":"2024-06-20T00:00:00Z"}
  ],
  "has_more": false,
  "first_id": "claude-opus-4-7-20251015",
  "last_id": "claude-3-5-sonnet-20240620"
}`

func TestFamilyOf(t *testing.T) {
	cases := []struct {
		id, want string
	}{
		// New naming
		{"claude-opus-4-7-20251015", "claude-opus"},
		{"claude-opus-4-5-20251201", "claude-opus"},
		{"claude-sonnet-4-5-20251015", "claude-sonnet"},
		{"claude-haiku-4-5-20251001", "claude-haiku"},
		// Older naming (generation-first)
		{"claude-3-5-sonnet-20240620", "claude-sonnet"},
		{"claude-3-opus-20240229", "claude-opus"},
		// Unknown patterns → empty
		{"gpt-5-2026-03-15", ""},
		{"claude-2", ""},
		{"random-id", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := familyOf(tc.id); got != tc.want {
			t.Errorf("familyOf(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestListModels(t *testing.T) {
	c := newClient(t, respondJSON(t, 200, modelsListJSON, nil))
	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("len = %d, want 5", len(got))
	}
	// Spot-check the first entry: family classified, Created parsed.
	first := got[0]
	if first.ID != "claude-opus-4-7-20251015" {
		t.Errorf("ID = %q", first.ID)
	}
	if first.Family != "claude-opus" {
		t.Errorf("Family = %q, want claude-opus", first.Family)
	}
	want := time.Date(2025, 10, 15, 0, 0, 0, 0, time.UTC)
	if !first.Created.Equal(want) {
		t.Errorf("Created = %v, want %v", first.Created, want)
	}
	if first.Raw == nil {
		t.Error("Raw should carry the SDK payload")
	}
}

func TestListModelsAPIError(t *testing.T) {
	// 401 should surface as a classified *llms.ProviderError.
	c := newClient(t, respondJSON(t, 401, `{"error":{"type":"authentication_error","message":"invalid key"}}`, nil))
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
		{ID: "claude-opus-4-5-20250901", Family: "claude-opus", Created: tm("2025-09-01")},
		{ID: "claude-opus-4-7-20251015", Family: "claude-opus", Created: tm("2025-10-15")},
		{ID: "claude-sonnet-4-5-20251015", Family: "claude-sonnet", Created: tm("2025-10-15")},
		{ID: "claude-3-5-sonnet-20240620", Family: "claude-sonnet", Created: tm("2024-06-20")},
	}

	t.Run("newer opus available", func(t *testing.T) {
		newer, ok := LatestInFamily("claude-opus-4-5-20250901", models)
		if !ok {
			t.Fatal("expected newer found")
		}
		if newer.ID != "claude-opus-4-7-20251015" {
			t.Errorf("ID = %q", newer.ID)
		}
	})

	t.Run("already on latest", func(t *testing.T) {
		_, ok := LatestInFamily("claude-opus-4-7-20251015", models)
		if ok {
			t.Fatal("expected false (already newest)")
		}
	})

	t.Run("cross generation: 3-5-sonnet -> sonnet-4-5", func(t *testing.T) {
		// Permissive family parsing: 3-5-sonnet and sonnet-4-5 share family.
		newer, ok := LatestInFamily("claude-3-5-sonnet-20240620", models)
		if !ok {
			t.Fatal("expected newer found")
		}
		if newer.ID != "claude-sonnet-4-5-20251015" {
			t.Errorf("ID = %q", newer.ID)
		}
	})

	t.Run("unparseable family", func(t *testing.T) {
		_, ok := LatestInFamily("gpt-5", models)
		if ok {
			t.Fatal("expected false (no opus/sonnet/haiku in id)")
		}
	})

	t.Run("current not in list — still suggests newest known", func(t *testing.T) {
		// An ID Anthropic deprecated or aliased that we still hold locally:
		// the helper should still suggest the newest known family member.
		newer, ok := LatestInFamily("claude-opus-deprecated-alias", models)
		if !ok {
			t.Fatal("expected newer found")
		}
		if newer.ID != "claude-opus-4-7-20251015" {
			t.Errorf("ID = %q", newer.ID)
		}
	})

	t.Run("empty model list", func(t *testing.T) {
		_, ok := LatestInFamily("claude-opus-4-5-20250901", nil)
		if ok {
			t.Fatal("expected false on empty list")
		}
	})
}

func TestClientLatestInFamilyOneShot(t *testing.T) {
	c := newClient(t, respondJSON(t, 200, modelsListJSON, nil))
	newer, ok, err := c.LatestInFamily(context.Background(), "claude-opus-4-5-20251201")
	if err != nil {
		t.Fatalf("LatestInFamily: %v", err)
	}
	if !ok {
		t.Fatal("expected newer found")
	}
	if newer.ID != "claude-opus-4-7-20251015" {
		t.Errorf("ID = %q, want claude-opus-4-7-20251015", newer.ID)
	}
	if newer.Family != "claude-opus" {
		t.Errorf("Family = %q", newer.Family)
	}
}

// Compile-time assertion that *Client satisfies llms.ModelLister. Lives in
// the test file because a package-level var would trip gochecknoglobals.
var _ llms.ModelLister = (*Client)(nil)

// tm parses YYYY-MM-DD into a UTC time.
func tm(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}
