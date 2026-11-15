package providers_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/auth"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers"
)

// recordingTransport records each request and fails it, so no request
// leaves the process; only what was sent matters.
type recordingTransport struct {
	reqs []*http.Request
	mu   sync.Mutex
}

var errRecorded = errors.New("recorded")

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.reqs = append(rt.reqs, r)
	return nil, errRecorded
}

func (rt *recordingTransport) take() []*http.Request {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	r := rt.reqs
	rt.reqs = nil
	return r
}

type stubTokenProvider struct{}

func (stubTokenProvider) Token(context.Context) (*auth.Token, error) {
	return &auth.Token{Value: "vertex-token"}, nil
}

func stubCreds() *auth.Credentials {
	return auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: stubTokenProvider{}})
}

func vertexOpts() *providers.Vertex {
	return &providers.Vertex{Credentials: stubCreds(), Project: "proj", Location: "us-central1"}
}

// TestNewRoutesAndForwardsOptions checks that every provider ID builds the
// right adapter and that Model, APIKey, BaseURL, HTTPClient and Vertex all
// reach the request: a dropped option changes the host or the auth header.
func TestNewRoutesAndForwardsOptions(t *testing.T) {
	for _, tc := range []struct {
		vertex     *providers.Vertex
		name       string
		id         llms.ProviderID
		wantID     llms.ProviderID
		apiKey     string
		authHeader string
		authValue  string
	}{
		{name: "anthropic", id: llms.ProviderAnthropic, wantID: llms.ProviderAnthropic, apiKey: "ak", authHeader: "X-Api-Key", authValue: "ak"},
		{name: "anthropic vertex", id: llms.ProviderAnthropic, wantID: llms.ProviderAnthropic, vertex: vertexOpts(), authHeader: "Authorization", authValue: "Bearer vertex-token"},
		{name: "openai", id: llms.ProviderOpenAI, wantID: llms.ProviderOpenAI, apiKey: "ok", authHeader: "Authorization", authValue: "Bearer ok"},
		{name: "google", id: llms.ProviderGoogle, wantID: llms.ProviderGoogle, apiKey: "gk", authHeader: "X-Goog-Api-Key", authValue: "gk"},
		{name: "google vertex", id: llms.ProviderGoogle, wantID: llms.ProviderGoogle, vertex: vertexOpts(), authHeader: "Authorization", authValue: "Bearer vertex-token"},
		{name: "ollama", id: llms.ProviderOllama, wantID: llms.ProviderOllama},
		{name: "openaicompat", id: llms.ProviderOpenAICompat, wantID: llms.ProviderOpenAICompat, apiKey: "ck", authHeader: "Authorization", authValue: "Bearer ck"},
		{name: "vllm alias", id: llms.ProviderVLLM, wantID: llms.ProviderOpenAICompat}, //nolint:staticcheck // the deprecated alias must keep routing (ADR-0048 §2)
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A Vertex PSC base URL is the host root: the Vertex route
			// builds the /v1/projects/... path itself.
			base := "https://probe.example/v1"
			if tc.vertex != nil {
				base = "https://probe.example/"
			}
			rt := &recordingTransport{}
			c, err := providers.New(tc.id, providers.Options{
				HTTPClient: &http.Client{Transport: rt},
				Vertex:     tc.vertex,
				Model:      "probe-model",
				APIKey:     tc.apiKey,
				BaseURL:    base,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if got := c.Model(); got != (llms.ModelRef{Provider: tc.wantID, Name: "probe-model"}) {
				t.Fatalf("Model() = %+v", got)
			}
			if _, ok := c.(llms.ModelLister); !ok {
				t.Fatalf("%T does not satisfy llms.ModelLister", c)
			}
			if _, err := c.Complete(context.Background(), llms.ChatRequest{
				Messages: []llms.Message{llms.UserText("hi")},
			}); err == nil {
				t.Fatal("Complete succeeded against a failing transport")
			}
			reqs := rt.take()
			if len(reqs) != 1 {
				t.Fatalf("got %d requests, want 1", len(reqs))
			}
			r := reqs[0]
			if r.URL.Host != "probe.example" {
				t.Errorf("request host = %q, want probe.example (BaseURL dropped?)", r.URL.Host)
			}
			if tc.authHeader != "" {
				if got := r.Header.Get(tc.authHeader); got != tc.authValue {
					t.Errorf("%s = %q, want %q", tc.authHeader, got, tc.authValue)
				}
			}
			if tc.vertex != nil && !strings.Contains(r.URL.Path, "projects/proj/locations/us-central1") {
				t.Errorf("path %q lacks the Vertex project/location", r.URL.Path)
			}
		})
	}
}

// TestNewDefaultsApply checks that a zero option is not forwarded, so the
// provider's default applies (Ollama's local base URL).
func TestNewDefaultsApply(t *testing.T) {
	rt := &recordingTransport{}
	c, err := providers.New(llms.ProviderOllama, providers.Options{
		HTTPClient: &http.Client{Transport: rt},
		Model:      "probe-model",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _ = c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}})
	reqs := rt.take()
	if len(reqs) != 1 || reqs[0].URL.Host != "localhost:11434" {
		t.Fatalf("requests = %v, want one to Ollama's default localhost:11434", reqs)
	}
}

func TestNewRefusals(t *testing.T) {
	for _, tc := range []struct {
		want error
		name string
		id   llms.ProviderID
		opts providers.Options
	}{
		{name: "unknown", id: "acme", opts: providers.Options{Model: "m"}, want: providers.ErrUnknownProvider},
		{name: "empty id", id: "", opts: providers.Options{Model: "m"}, want: providers.ErrUnknownProvider},
		{name: "ollama api key", id: llms.ProviderOllama, opts: providers.Options{Model: "m", APIKey: "k"}, want: providers.ErrUnsupportedOption},
		{name: "openai vertex", id: llms.ProviderOpenAI, opts: providers.Options{Model: "m", Vertex: vertexOpts()}, want: providers.ErrUnsupportedOption},
		{name: "ollama vertex", id: llms.ProviderOllama, opts: providers.Options{Model: "m", Vertex: vertexOpts()}, want: providers.ErrUnsupportedOption},
		{name: "openaicompat vertex", id: llms.ProviderOpenAICompat, opts: providers.Options{Model: "m", BaseURL: "https://x/v1", Vertex: vertexOpts()}, want: providers.ErrUnsupportedOption},
		{name: "anthropic api key + vertex", id: llms.ProviderAnthropic, opts: providers.Options{Model: "m", APIKey: "k", Vertex: vertexOpts()}, want: providers.ErrUnsupportedOption},
		{name: "google api key + vertex", id: llms.ProviderGoogle, opts: providers.Options{Model: "m", APIKey: "k", Vertex: vertexOpts()}, want: providers.ErrUnsupportedOption},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := providers.New(tc.id, tc.opts)
			if c != nil {
				t.Fatalf("got client %T with error %v", c, err)
			}
			assertConfig(t, err, tc.id)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

// TestNewProviderValidationPassesThrough checks that the provider's own
// validation runs unchanged and New adds no sentinel to it.
func TestNewProviderValidationPassesThrough(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   llms.ProviderID
		opts providers.Options
	}{
		{name: "anthropic missing model", id: llms.ProviderAnthropic, opts: providers.Options{APIKey: "k"}},
		{name: "openai missing model", id: llms.ProviderOpenAI, opts: providers.Options{APIKey: "k"}},
		{name: "google missing credentials", id: llms.ProviderGoogle, opts: providers.Options{Model: "m"}},
		{name: "ollama missing model", id: llms.ProviderOllama, opts: providers.Options{}},
		{name: "openaicompat missing base URL", id: llms.ProviderOpenAICompat, opts: providers.Options{Model: "m"}},
		{name: "anthropic vertex empty", id: llms.ProviderAnthropic, opts: providers.Options{Model: "m", Vertex: &providers.Vertex{}}},
		{name: "google vertex no credentials", id: llms.ProviderGoogle, opts: providers.Options{Model: "m", Vertex: &providers.Vertex{Project: "p", Location: "l"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := providers.New(tc.id, tc.opts)
			if c != nil {
				t.Fatalf("got client %T with error %v", c, err)
			}
			assertConfig(t, err, tc.id)
			if errors.Is(err, providers.ErrUnknownProvider) || errors.Is(err, providers.ErrUnsupportedOption) {
				t.Fatalf("provider validation error carries a providers sentinel: %v", err)
			}
		})
	}
}

func assertConfig(t *testing.T, err error, id llms.ProviderID) {
	t.Helper()
	var pe *llms.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want *llms.ProviderError", err, err)
	}
	if pe.Kind != llms.ErrorKindConfig || pe.Provider != id {
		t.Fatalf("err = %+v, want kind config, provider %q", pe, id)
	}
}
