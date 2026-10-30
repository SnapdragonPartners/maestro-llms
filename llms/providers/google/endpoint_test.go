package google

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/auth"
	"google.golang.org/genai"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// recordingTransport answers every request locally with a 404 and records
// it, so a test can see where the client sent traffic without a network.
type recordingTransport struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.reqs = append(rt.reqs, r)
	rt.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"code":404,"message":"recorded"}}`)),
		Request:    r,
	}, nil
}

func (rt *recordingTransport) take() []*http.Request {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	reqs := rt.reqs
	rt.reqs = nil
	return reqs
}

// setAmbientGoogleEnv points every endpoint- and credential-related genai
// environment default at a decoy host.
func setAmbientGoogleEnv(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"GOOGLE_GEMINI_BASE_URL":    "https://env-decoy.invalid/",
		"GOOGLE_VERTEX_BASE_URL":    "https://env-decoy.invalid/",
		"GOOGLE_API_KEY":            "env-google-key",
		"GEMINI_API_KEY":            "env-gemini-key",
		"GOOGLE_CLOUD_PROJECT":      "env-project",
		"GOOGLE_CLOUD_LOCATION":     "env-location",
		"GOOGLE_CLOUD_REGION":       "env-region",
		"GOOGLE_GENAI_USE_VERTEXAI": "true",
	} {
		t.Setenv(k, v)
	}
}

// Divergence G6 (#132): the endpoint comes from options only. With every
// genai environment default pointing at a decoy, and separately with the
// process-wide genai.SetDefaultBaseURLs (which genai consults before the
// environment) set too, chat, embeddings and ListModels on both backends
// still go to the backend's default endpoint, or to WithBaseURL when set,
// with the app-supplied credentials only.
func TestEndpointIgnoresAmbientGoogleEnv(t *testing.T) {
	setAmbientGoogleEnv(t)
	t.Run("environment", testEndpointFromOptionsOnly)
	t.Run("environment+SetDefaultBaseURLs", func(t *testing.T) {
		const decoy = "https://process-decoy.invalid/"
		genai.SetDefaultBaseURLs(genai.BaseURLParameters{GeminiURL: decoy, VertexURL: decoy})
		t.Cleanup(func() { genai.SetDefaultBaseURLs(genai.BaseURLParameters{}) })
		testEndpointFromOptionsOnly(t)
	})
}

func testEndpointFromOptionsOnly(t *testing.T) {
	creds := auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: stubTokenProvider{}})
	gemini := []Option{WithAPIKey("k")}
	vertex := func(loc string) []Option { return []Option{WithVertex("proj", loc), WithCredentials(creds)} }
	for _, tc := range []struct {
		name     string
		backend  []Option
		baseURL  string
		wantHost string
		vertex   bool
	}{
		{"gemini default", gemini, "", "generativelanguage.googleapis.com", false},
		{"gemini WithBaseURL", gemini, "https://psc.example/", "psc.example", false},
		{"vertex global", vertex("global"), "", "aiplatform.googleapis.com", true},
		{"vertex us multi-region", vertex("us"), "", "aiplatform.us.rep.googleapis.com", true},
		{"vertex regional", vertex("us-central1"), "", "us-central1-aiplatform.googleapis.com", true},
		{"vertex WithBaseURL", vertex("us-central1"), "https://psc.example/", "psc.example", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &recordingTransport{}
			opts := append([]Option{WithHTTPClient(&http.Client{Transport: rt})}, tc.backend...)
			if tc.baseURL != "" {
				opts = append(opts, WithBaseURL(tc.baseURL))
			}
			chat, err := New(append([]Option{WithModel("gemini-test")}, opts...)...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			emb, err := NewEmbeddings(append([]Option{WithModel("embed-test"), WithMaxInputBytes(1 << 10)}, opts...)...)
			if err != nil {
				t.Fatalf("NewEmbeddings: %v", err)
			}
			ctx := context.Background()
			// Every call fails on the recorded 404; only the request matters.
			_, _ = chat.Complete(ctx, llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}})
			_, _ = emb.Embed(ctx, llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{{Text: "hi"}}})
			_, _ = chat.ListModels(ctx)

			reqs := rt.take()
			if len(reqs) != 3 {
				t.Fatalf("recorded %d requests, want 3 (chat, embed, list)", len(reqs))
			}
			for _, r := range reqs {
				if r.URL.Host != tc.wantHost {
					t.Errorf("%s %s: host %q, want %q", r.Method, r.URL, r.URL.Host, tc.wantHost)
				}
				key := r.Header.Get("X-Goog-Api-Key")
				switch {
				case tc.vertex && key != "":
					t.Errorf("%s: Vertex request carries x-goog-api-key %q from the environment", r.URL, key)
				case !tc.vertex && key != "k":
					t.Errorf("%s: x-goog-api-key %q, want the WithAPIKey key", r.URL, key)
				}
				if tc.vertex && strings.Contains(r.URL.Path, "/projects/") &&
					!strings.Contains(r.URL.Path, "/projects/proj/locations/") {
					t.Errorf("%s: Vertex path does not use the WithVertex project", r.URL)
				}
				if strings.Contains(r.URL.Path, "env-") {
					t.Errorf("%s: path carries an environment value", r.URL)
				}
			}
		})
	}
}

func TestDefaultBaseURL(t *testing.T) {
	for _, tc := range []struct {
		backend  genai.Backend
		location string
		want     string
	}{
		{genai.BackendGeminiAPI, "", "https://generativelanguage.googleapis.com/"},
		{genai.BackendVertexAI, "global", "https://aiplatform.googleapis.com/"},
		{genai.BackendVertexAI, "us", "https://aiplatform.us.rep.googleapis.com/"},
		{genai.BackendVertexAI, "eu", "https://aiplatform.eu.rep.googleapis.com/"},
		{genai.BackendVertexAI, "europe-west4", "https://europe-west4-aiplatform.googleapis.com/"},
	} {
		if got := defaultBaseURL(tc.backend, tc.location); got != tc.want {
			t.Errorf("defaultBaseURL(%v, %q) = %q, want %q", tc.backend, tc.location, got, tc.want)
		}
	}
}

// The table mirrors genai's own default; this pins it to the SDK so a genai
// bump that moves an endpoint fails here instead of silently diverging.
func TestDefaultBaseURLMatchesGenai(t *testing.T) {
	for _, k := range []string{"GOOGLE_GEMINI_BASE_URL", "GOOGLE_VERTEX_BASE_URL", "GOOGLE_API_KEY", "GEMINI_API_KEY"} {
		t.Setenv(k, "") // genai treats empty as unset for these
	}
	creds := auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: stubTokenProvider{}})
	for _, cc := range []genai.ClientConfig{
		{Backend: genai.BackendGeminiAPI, APIKey: "k"},
		{Backend: genai.BackendVertexAI, Project: "p", Location: "global"},
		{Backend: genai.BackendVertexAI, Project: "p", Location: "us"},
		{Backend: genai.BackendVertexAI, Project: "p", Location: "eu"},
		{Backend: genai.BackendVertexAI, Project: "p", Location: "us-central1"},
	} {
		if cc.Backend == genai.BackendVertexAI {
			cc.Credentials = creds
		}
		cc.HTTPClient = &http.Client{Transport: &recordingTransport{}}
		want := defaultBaseURL(cc.Backend, cc.Location)
		c, err := genai.NewClient(context.Background(), &cc)
		if err != nil {
			t.Fatalf("genai.NewClient(%v, %q): %v", cc.Backend, cc.Location, err)
		}
		if got := c.ClientConfig().HTTPOptions.BaseURL; got != want {
			t.Errorf("genai default for (%v, %q) is %q, defaultBaseURL says %q", cc.Backend, cc.Location, got, want)
		}
	}
}
