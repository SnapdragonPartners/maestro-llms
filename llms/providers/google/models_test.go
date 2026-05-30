package google

import (
	"context"
	"errors"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// Compile-time assertion that *Client satisfies llms.ModelLister.
var _ llms.ModelLister = (*Client)(nil)

// modelsListJSON mimics the v1beta /models response shape Gemini returns
// (genai uses Models.All to iterate pages; a single page with no
// nextPageToken terminates the iterator after one fetch).
const modelsListJSON = `{
  "models": [
    {"name":"models/gemini-3-pro-preview","displayName":"Gemini 3 Pro","version":"001",
     "inputTokenLimit":1048576,"outputTokenLimit":8192,"supportedActions":["generateContent"]},
    {"name":"models/gemini-2.5-pro","displayName":"Gemini 2.5 Pro","version":"001",
     "inputTokenLimit":1048576,"outputTokenLimit":8192,"supportedActions":["generateContent"]},
    {"name":"models/gemini-1.5-pro-001","displayName":"Gemini 1.5 Pro","version":"001",
     "inputTokenLimit":1048576,"outputTokenLimit":8192,"supportedActions":["generateContent"]},
    {"name":"models/gemini-2.0-flash","displayName":"Gemini 2.0 Flash","version":"001",
     "inputTokenLimit":1048576,"outputTokenLimit":8192,"supportedActions":["generateContent"]},
    {"name":"models/text-embedding-004","displayName":"Embedding","version":"004",
     "inputTokenLimit":2048,"outputTokenLimit":1,"supportedActions":["embedContent"]}
  ]
}`

func TestFamilyOf(t *testing.T) {
	cases := []struct {
		id, want string
	}{
		// Stripped-id inputs.
		{"gemini-1.5-pro-001", "gemini-pro"},
		{"gemini-3-pro-preview", "gemini-pro"},
		{"gemini-2.5-pro", "gemini-pro"},
		{"gemini-2.0-flash", "gemini-flash"},
		{"gemini-nano", "gemini-nano"},
		// Resource-path-prefixed inputs still classify (familyOf strips).
		{"models/gemini-1.5-pro-001", "gemini-pro"},
		// Non-Gemini IDs / non-role IDs → "".
		{"text-embedding-004", ""},
		{"gpt-5", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := familyOf(tc.id); got != tc.want {
			t.Errorf("familyOf(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestVersionOf(t *testing.T) {
	cases := []struct {
		id   string
		want float64
	}{
		{"gemini-1.5-pro-001", 1.5},
		{"gemini-3-pro-preview", 3.0},
		{"gemini-2.5-pro", 2.5},
		{"gemini-2.0-flash", 2.0},
		// Legacy / no-version IDs return 0 (sort to the bottom).
		{"gemini-pro", 0},
		{"text-embedding-004", 0},
		// Resource-prefix tolerated.
		{"models/gemini-3-pro-preview", 3.0},
	}
	for _, tc := range cases {
		if got := versionOf(tc.id); got != tc.want {
			t.Errorf("versionOf(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestListModels(t *testing.T) {
	c := newClient(t, jsonHandler(t, 200, modelsListJSON))
	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("len = %d, want 5", len(got))
	}
	// IDs should be stripped of the `models/` prefix.
	byID := map[string]llms.ModelInfo{}
	for _, m := range got {
		byID[m.ID] = m
	}
	if _, ok := byID["gemini-3-pro-preview"]; !ok {
		t.Errorf("missing gemini-3-pro-preview; got ids: %v", idsOf(got))
	}
	if byID["gemini-3-pro-preview"].Family != "gemini-pro" {
		t.Errorf("Family = %q, want gemini-pro", byID["gemini-3-pro-preview"].Family)
	}
	// Created is intentionally zero — genai doesn't expose it.
	if !byID["gemini-3-pro-preview"].Created.IsZero() {
		t.Errorf("Created = %v, want zero (genai exposes no date)", byID["gemini-3-pro-preview"].Created)
	}
	if byID["gemini-3-pro-preview"].Raw == nil {
		t.Error("Raw should carry the SDK payload")
	}
}

func TestListModelsAPIError(t *testing.T) {
	c := newClient(t, jsonHandler(t, 401, `{"error":{"code":401,"message":"invalid key","status":"UNAUTHENTICATED"}}`))
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
		{ID: "gemini-1.5-pro-001", Family: "gemini-pro"},
		{ID: "gemini-2.5-pro", Family: "gemini-pro"},
		{ID: "gemini-3-pro-preview", Family: "gemini-pro"},
		{ID: "gemini-2.0-flash", Family: "gemini-flash"},
		{ID: "text-embedding-004", Family: ""},
	}

	t.Run("newer pro available", func(t *testing.T) {
		newer, ok := LatestInFamily("gemini-1.5-pro-001", models)
		if !ok {
			t.Fatal("expected newer found")
		}
		if newer.ID != "gemini-3-pro-preview" {
			t.Errorf("ID = %q, want gemini-3-pro-preview", newer.ID)
		}
	})

	t.Run("already on latest pro", func(t *testing.T) {
		_, ok := LatestInFamily("gemini-3-pro-preview", models)
		if ok {
			t.Fatal("expected false (already newest)")
		}
	})

	t.Run("flash stays in flash family", func(t *testing.T) {
		// Pro models should NOT be suggested to a flash user.
		_, ok := LatestInFamily("gemini-2.0-flash", models)
		if ok {
			t.Fatal("expected false (no newer flash in list)")
		}
	})

	t.Run("unparseable family", func(t *testing.T) {
		_, ok := LatestInFamily("text-embedding-004", models)
		if ok {
			t.Fatal("expected false (no gemini role token)")
		}
	})

	t.Run("empty list", func(t *testing.T) {
		_, ok := LatestInFamily("gemini-1.5-pro-001", nil)
		if ok {
			t.Fatal("expected false on empty list")
		}
	})
}

func TestClientLatestInFamilyOneShot(t *testing.T) {
	c := newClient(t, jsonHandler(t, 200, modelsListJSON))
	newer, ok, err := c.LatestInFamily(context.Background(), "gemini-1.5-pro-001")
	if err != nil {
		t.Fatalf("LatestInFamily: %v", err)
	}
	if !ok {
		t.Fatal("expected newer found")
	}
	if newer.ID != "gemini-3-pro-preview" {
		t.Errorf("ID = %q", newer.ID)
	}
	if newer.Family != "gemini-pro" {
		t.Errorf("Family = %q", newer.Family)
	}
}

func idsOf(ms []llms.ModelInfo) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}
