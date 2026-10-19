package anthropicvertex_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"cloud.google.com/go/auth"

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

type staticToken struct{}

func (staticToken) Token(context.Context) (*auth.Token, error) {
	return &auth.Token{Value: "test-token"}, nil
}

func staticCreds() *auth.Credentials {
	return auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: staticToken{}})
}

// Covers the required properties in one round trip:
//  1. request path is rewritten to the Vertex rawPredict shape,
//  2. anthropic_version is injected,
//  3. the final base URL is the caller's PSC endpoint,
//  4. the final transport is the caller's PSC transport,
//  5. WithCredentials authenticates the PSC client, which carries no auth
//     of its own, without mutating it (ADR-0029).
func TestVertexPSCPrecedenceAndMiddleware(t *testing.T) {
	var (
		gotPath string
		gotAuth string
		gotBody map[string]any
		used    bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, msgRespJSON)
	}))
	defer srv.Close()

	pscTransport := markerRT{used: &used, base: http.DefaultTransport}
	pscClient := &http.Client{Transport: pscTransport}

	c, err := anthropicvertex.New(
		anthropicvertex.WithRegion("us-central1"),
		anthropicvertex.WithProjectID("proj-1"),
		anthropicvertex.WithModel("claude-x"),
		anthropicvertex.WithCredentials(staticCreds()),
		anthropicvertex.WithEndpoint(srv.URL), // applied AFTER the Vertex option
		anthropicvertex.WithHTTPClient(pscClient),
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
	// (5) WithCredentials signs requests sent through the PSC client.
	if gotAuth != "Bearer test-token" {
		t.Fatalf("Authorization = %q, want the WithCredentials token on the PSC client", gotAuth)
	}
	if rt, ok := pscClient.Transport.(markerRT); !ok || rt != pscTransport {
		t.Fatal("the caller's PSC client was mutated")
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

	// Non-nil Credentials with a nil TokenProvider must be rejected: they
	// cannot mint tokens, and must never fall back to ambient ADC.
	_, err := anthropicvertex.New(
		anthropicvertex.WithRegion("us-central1"),
		anthropicvertex.WithProjectID("p"),
		anthropicvertex.WithModel("m"),
		anthropicvertex.WithCredentials(&auth.Credentials{}),
	)
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindConfig {
		t.Fatalf("empty credentials (nil TokenProvider): want a config *ProviderError, got %v", err)
	}
}

// The app-supplied *auth.Credentials must be what signs requests: the
// adapter converts them for the Anthropic Vertex SDK (ADR-0027), so with no
// PSC client the SDK's own oauth2 client sends the caller's token.
func TestAuthCredentialsSignRequests(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, msgRespJSON)
	}))
	defer srv.Close()

	c, err := anthropicvertex.New(
		anthropicvertex.WithRegion("us-central1"),
		anthropicvertex.WithProjectID("proj-1"),
		anthropicvertex.WithModel("claude-x"),
		anthropicvertex.WithCredentials(staticCreds()),
		anthropicvertex.WithEndpoint(srv.URL),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("hi")},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("Authorization = %q, want the caller's token", gotAuth)
	}
}
