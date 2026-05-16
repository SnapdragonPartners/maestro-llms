package google

import (
	"context"
	"net/http"

	"google.golang.org/genai"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

const providerName = "google"

// Option configures a Client. Applications own config resolution; this package
// accepts already-resolved values.
type Option func(*settings)

//nolint:govet // fieldalignment: internal one-shot config struct, layout irrelevant.
type settings struct {
	apiKey     string
	model      string
	baseURL    string
	httpClient *http.Client
	maxRetries int
	hasRetries bool
}

// WithAPIKey sets the Gemini API key.
func WithAPIKey(key string) Option { return func(s *settings) { s.apiKey = key } }

// WithModel sets the model identifier. Unknown/new names are accepted.
func WithModel(model string) Option { return func(s *settings) { s.model = model } }

// WithBaseURL overrides the API base URL (proxies, tests).
func WithBaseURL(u string) Option { return func(s *settings) { s.baseURL = u } }

// WithHTTPClient injects the HTTP client (timeouts, proxies, observability,
// test transports).
func WithHTTPClient(c *http.Client) Option { return func(s *settings) { s.httpClient = c } }

// WithMaxRetries is accepted for cross-provider symmetry and validated, but
// the genai SDK exposes no SDK-level retry knob; retries are maestro-llms
// middleware's job anyway (default 0 = no provider-SDK retries).
func WithMaxRetries(n int) Option {
	return func(s *settings) { s.maxRetries = n; s.hasRetries = true }
}

// Client is an llms.ChatClient backed by the Google genai SDK.
//
//nolint:govet // fieldalignment: size dominated by the embedded third-party SDK client
type Client struct {
	api   *genai.Client
	model string
}

// compile-time assertion lives in the test file.

func configErr(msg string) error {
	return &llms.ProviderError{Provider: providerName, Kind: llms.ErrorKindConfig, Message: msg}
}

// New builds a Client. It returns a *llms.ProviderError of kind config when
// required values are missing/invalid or the SDK client cannot be created.
func New(opts ...Option) (*Client, error) {
	var s settings
	for _, o := range opts {
		o(&s)
	}
	switch {
	case s.apiKey == "":
		return nil, configErr("missing API key")
	case s.model == "":
		return nil, configErr("missing model")
	case s.hasRetries && s.maxRetries < 0:
		return nil, configErr("max retries must be >= 0")
	}

	cfg := &genai.ClientConfig{APIKey: s.apiKey, Backend: genai.BackendGeminiAPI}
	if s.baseURL != "" {
		cfg.HTTPOptions.BaseURL = s.baseURL
	}
	if s.httpClient != nil {
		cfg.HTTPClient = s.httpClient
	}
	// genai.NewClient builds transport/config only (no network call for the
	// Gemini API key backend); a non-nil error here is a config problem.
	api, err := genai.NewClient(context.Background(), cfg)
	if err != nil {
		return nil, &llms.ProviderError{
			Provider: providerName, Kind: llms.ErrorKindConfig,
			Message: "genai client init failed", Cause: err,
		}
	}
	return &Client{api: api, model: s.model}, nil
}

// Model returns the model this client targets.
func (c *Client) Model() llms.ModelRef {
	return llms.ModelRef{Provider: providerName, Name: c.model}
}

// Complete sends a synchronous chat completion via the Gemini API.
func (c *Client) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
	contents, cfg, perr := c.toParams(req)
	if perr != nil {
		return llms.ChatResponse{}, perr
	}
	result, err := c.api.Models.GenerateContent(ctx, c.model, contents, cfg)
	if err != nil {
		return llms.ChatResponse{}, classifyError(c.model, err)
	}
	return toChatResponse(result), nil
}
