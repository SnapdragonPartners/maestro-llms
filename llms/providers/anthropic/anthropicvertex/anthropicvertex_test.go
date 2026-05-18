package anthropicvertex_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/anthropic/anthropicvertex"
)

const msgRespJSON = `{"id":"msg_1","type":"message","role":"assistant",` +
	`"model":"claude-x","content":[{"type":"text","text":"hi"}],` +
	`"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

// markerRT records that the caller's transport was the one actually used.
type markerRT struct {
	used *bool
	base http.RoundTripper
}

func (m markerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	*m.used = true
	return m.base.RoundTrip(r)
}

func staticCreds() *google.Credentials {
	return &google.Credentials{
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"}),
	}
}

// Covers all four required properties in one round trip:
//  1. request path is rewritten to the Vertex rawPredict shape,
//  2. anthropic_version is injected,
//  3. the final base URL is the caller's PSC endpoint,
//  4. the final HTTP client/transport is the caller's PSC client.
func TestVertexPSCPrecedenceAndMiddleware(t *testing.T) {
	var (
		gotPath string
		gotBody map[string]any
		used    bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, msgRespJSON)
	}))
	defer srv.Close()

	pscClient := &http.Client{Transport: markerRT{used: &used, base: http.DefaultTransport}}

	c, err := anthropicvertex.New(
		anthropicvertex.WithRegion("us-central1"),
		anthropicvertex.WithProjectID("proj-1"),
		anthropicvertex.WithModel("claude-x"),
		anthropicvertex.WithCredentials(staticCreds()),
		anthropicvertex.WithEndpoint(srv.URL),     // applied AFTER the Vertex option
		anthropicvertex.WithHTTPClient(pscClient), // applied AFTER the Vertex option
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "hi" {
		t.Fatalf("response not parsed through the base provider: %q", resp.Text)
	}

	// (1) Vertex path rewrite (middleware survived the client override).
	wantPath := "/v1/projects/proj-1/locations/us-central1/publishers/anthropic/models/claude-x:rawPredict"
	if gotPath != wantPath {
		t.Fatalf("path not rewritten to Vertex shape:\n got  %s\n want %s", gotPath, wantPath)
	}
	// (2) anthropic_version injected by the Vertex middleware.
	if _, ok := gotBody["anthropic_version"]; !ok {
		t.Fatalf("anthropic_version not injected: %v", gotBody)
	}
	// (3) base URL = caller's PSC endpoint (request reached srv at all; the
	// Vertex default would be https://us-central1-aiplatform.googleapis.com).
	// (4) caller's PSC client/transport was used, not Vertex's self-built one.
	if !used {
		t.Fatal("caller's PSC HTTP client/transport was not used (Vertex's won)")
	}
}

func TestNewConfigValidation(t *testing.T) {
	base := []anthropicvertex.Option{
		anthropicvertex.WithRegion("us-central1"),
		anthropicvertex.WithProjectID("p"),
		anthropicvertex.WithModel("m"),
		anthropicvertex.WithCredentials(staticCreds()),
	}
	drop := func(i int) []anthropicvertex.Option {
		out := make([]anthropicvertex.Option, 0, len(base)-1)
		out = append(out, base[:i]...)
		return append(out, base[i+1:]...)
	}
	for i, name := range []string{"region", "projectID", "model", "credentials"} {
		_, err := anthropicvertex.New(drop(i)...)
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindConfig {
			t.Fatalf("missing %s: want a config *ProviderError, got %v", name, err)
		}
	}

	// Non-nil Credentials with a nil TokenSource must be rejected: otherwise
	// Google's transport silently falls back to ambient ADC, violating the
	// app-supplied-credentials-only contract.
	_, err := anthropicvertex.New(
		anthropicvertex.WithRegion("us-central1"),
		anthropicvertex.WithProjectID("p"),
		anthropicvertex.WithModel("m"),
		anthropicvertex.WithCredentials(&google.Credentials{}),
	)
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindConfig {
		t.Fatalf("empty credentials (nil TokenSource): want a config *ProviderError, got %v", err)
	}
}
