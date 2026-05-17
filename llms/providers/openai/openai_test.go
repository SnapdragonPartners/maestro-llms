package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

var _ llms.EmbeddingClient = (*Client)(nil)

func newClient(t *testing.T, handler http.HandlerFunc, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	base := []Option{
		WithAPIKey("test-key"),
		WithModel("text-embedding-3-small"),
		WithBaseURL(srv.URL),
		WithHTTPClient(srv.Client()),
	}
	c, err := New(append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func respond(t *testing.T, status int, body string, headers map[string]string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func TestEmbedOrderedWithIDs(t *testing.T) {
	// Data returned out of order (index 1 before 0): result must be reordered.
	body := `{"object":"list","model":"text-embedding-3-small",
"data":[
 {"object":"embedding","index":1,"embedding":[0.3,0.4]},
 {"object":"embedding","index":0,"embedding":[0.1,0.2]}],
"usage":{"prompt_tokens":7,"total_tokens":7}}`
	c := newClient(t, respond(t, 200, body, nil))
	resp, err := c.Embed(context.Background(), llms.EmbeddingRequest{
		Inputs: []llms.EmbeddingInput{{ID: "a", Text: "alpha"}, {ID: "b", Text: "beta"}},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(resp.Vectors) != 2 {
		t.Fatalf("want 2 vectors, got %d", len(resp.Vectors))
	}
	if resp.Vectors[0].ID != "a" || resp.Vectors[1].ID != "b" {
		t.Fatalf("input order/IDs not preserved: %+v", resp.Vectors)
	}
	if resp.Vectors[0].Values[0] != float32(0.1) || resp.Vectors[1].Values[0] != float32(0.3) {
		t.Fatalf("vectors not placed by index: %+v", resp.Vectors)
	}
	if resp.Usage.EmbeddingTokens != 7 || resp.Usage.TotalTokens != 7 {
		t.Fatalf("usage mapping wrong: %+v", resp.Usage)
	}
	if resp.Raw == nil {
		t.Fatal("Raw should carry the provider response")
	}
	if c.Model().Provider != "openai" || c.Model().Name != "text-embedding-3-small" {
		t.Fatalf("model ref = %+v", c.Model())
	}
}

func TestDimensionsOverrideAndDefault(t *testing.T) {
	okBody := `{"object":"list","model":"m","data":[{"object":"embedding","index":0,"embedding":[1]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`

	capture := func(into *map[string]any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, into)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, okBody)
		}
	}

	// Per-request Dimensions overrides everything.
	var got map[string]any
	c := newClient(t, capture(&got), WithDimensions(64))
	_, err := c.Embed(context.Background(), llms.EmbeddingRequest{
		Inputs: []llms.EmbeddingInput{{ID: "1", Text: "x"}}, Dimensions: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["dimensions"] != float64(16) {
		t.Fatalf("per-request dimensions not sent: %v", got["dimensions"])
	}

	// No per-request value falls back to the configured default.
	var got2 map[string]any
	c2 := newClient(t, capture(&got2), WithDimensions(64))
	if c2.DefaultDimensions() != 64 {
		t.Fatalf("DefaultDimensions = %d", c2.DefaultDimensions())
	}
	if _, err := c2.Embed(context.Background(), llms.EmbeddingRequest{
		Inputs: []llms.EmbeddingInput{{ID: "1", Text: "x"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got2["dimensions"] != float64(64) {
		t.Fatalf("configured default dimensions not sent: %v", got2["dimensions"])
	}
}

func TestEmptyAndOverLimitInputsRejected(t *testing.T) {
	called := false
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = io.WriteString(w, "{}")
	})
	var pe *llms.ProviderError

	_, err := c.Embed(context.Background(), llms.EmbeddingRequest{})
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
		t.Fatalf("empty inputs: want bad_request, got %v", err)
	}

	big := make([]llms.EmbeddingInput, maxBatchInputs+1)
	_, err = c.Embed(context.Background(), llms.EmbeddingRequest{Inputs: big})
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
		t.Fatalf("over-limit: want bad_request, got %v", err)
	}
	if called {
		t.Fatal("provider must not be called for invalid input")
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		status     int
		retryAfter string
		wantKind   llms.ErrorKind
		wantRetry  time.Duration
	}{
		{401, "", llms.ErrorKindAuth, 0},
		{403, "", llms.ErrorKindAuth, 0},
		{429, "5", llms.ErrorKindRateLimited, 5 * time.Second},
		{400, "", llms.ErrorKindBadRequest, 0},
		{500, "", llms.ErrorKindUnavailable, 0},
		{503, "", llms.ErrorKindUnavailable, 0},
	}
	for _, tc := range cases {
		hdr := map[string]string{}
		if tc.retryAfter != "" {
			hdr["Retry-After"] = tc.retryAfter
		}
		c := newClient(t, respond(t, tc.status, `{"error":{"message":"boom"}}`, hdr))
		_, err := c.Embed(context.Background(), llms.EmbeddingRequest{
			Inputs: []llms.EmbeddingInput{{ID: "1", Text: "x"}},
		})
		var pe *llms.ProviderError
		if !errors.As(err, &pe) {
			t.Fatalf("status %d: want *llms.ProviderError, got %v", tc.status, err)
		}
		if pe.Kind != tc.wantKind || pe.StatusCode != tc.status || pe.RetryAfter != tc.wantRetry {
			t.Fatalf("status %d: kind=%q code=%d retry=%v", tc.status, pe.Kind, pe.StatusCode, pe.RetryAfter)
		}
	}
}

func TestContextCanceledClassified(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = io.WriteString(w, "{}")
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := c.Embed(ctx, llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{{ID: "1", Text: "x"}}})
	// Caller cancellation is not a provider failure: returned as-is (not a
	// *ProviderError), non-retryable, still errors.Is(context.Canceled)
	// (see apierr / divergences X5).
	var pe *llms.ProviderError
	if errors.As(err, &pe) {
		t.Fatalf("context.Canceled must not be a ProviderError, got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want errors.Is(context.Canceled), got %v", err)
	}
	if llms.Retryable(err) {
		t.Fatalf("context.Canceled must be non-retryable, got %v", err)
	}
}

func TestCountMismatchRejected(t *testing.T) {
	body := `{"object":"list","model":"m","data":[{"object":"embedding","index":0,"embedding":[1]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`
	c := newClient(t, respond(t, 200, body, nil))
	_, err := c.Embed(context.Background(), llms.EmbeddingRequest{
		Inputs: []llms.EmbeddingInput{{ID: "1", Text: "a"}, {ID: "2", Text: "b"}},
	})
	if err == nil || !strings.Contains(err.Error(), "different number of vectors") {
		t.Fatalf("want count-mismatch error, got %v", err)
	}
}

func TestDuplicateIndexRejected(t *testing.T) {
	// Two data, equal count to inputs (passes the count check), but both
	// report index 0: one input would be silently left zero-valued.
	body := `{"object":"list","model":"m","data":[
 {"object":"embedding","index":0,"embedding":[1]},
 {"object":"embedding","index":0,"embedding":[2]}],
"usage":{"prompt_tokens":2,"total_tokens":2}}`
	c := newClient(t, respond(t, 200, body, nil))
	_, err := c.Embed(context.Background(), llms.EmbeddingRequest{
		Inputs: []llms.EmbeddingInput{{ID: "1", Text: "a"}, {ID: "2", Text: "b"}},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate embedding index") {
		t.Fatalf("want duplicate-index error, got %v", err)
	}
}

func TestNewConfigErrors(t *testing.T) {
	for _, opts := range [][]Option{
		{WithModel("m")},  // no key
		{WithAPIKey("k")}, // no model
		{WithAPIKey("k"), WithModel("m"), WithMaxRetries(-1)},
		{WithAPIKey("k"), WithModel("m"), WithDimensions(-1)},
	} {
		_, err := New(opts...)
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindConfig {
			t.Fatalf("opts %v: want config ProviderError, got %v", opts, err)
		}
	}
}
