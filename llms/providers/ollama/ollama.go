// Package ollama implements the maestro-llms chat client for local Ollama
// models. It speaks the Ollama /api/chat JSON endpoint directly over net/http
// rather than importing github.com/ollama/ollama: that module carries
// server-side CVEs (no fixed version) which govulncheck would attribute to
// any consumer, and the endpoint is a trivial JSON contract. A hand-rolled
// client keeps the dependency surface minimal (a spec non-goal) and yields
// real HTTP status/headers for error classification plus raw tool-arg
// fidelity.
package ollama

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

const providerName = "ollama"

// defaultBaseURL is the conventional local Ollama endpoint.
const defaultBaseURL = "http://localhost:11434"

// Option configures a Client. Ollama is a local runtime and needs no API key.
type Option func(*settings)

//nolint:govet // fieldalignment: internal one-shot config struct, layout irrelevant.
type settings struct {
	model      string
	baseURL    string
	httpClient *http.Client
	maxRetries int
	hasRetries bool
}

// WithModel sets the model identifier. Unknown/new names are accepted.
func WithModel(model string) Option { return func(s *settings) { s.model = model } }

// WithBaseURL overrides the Ollama server URL (default http://localhost:11434).
func WithBaseURL(u string) Option { return func(s *settings) { s.baseURL = u } }

// WithHTTPClient injects the HTTP client (timeouts, proxies, test transports).
func WithHTTPClient(c *http.Client) Option { return func(s *settings) { s.httpClient = c } }

// WithMaxRetries is accepted for cross-provider symmetry and validated;
// retries are maestro-llms middleware's job, not this client's.
func WithMaxRetries(n int) Option {
	return func(s *settings) { s.maxRetries = n; s.hasRetries = true }
}

// Client is an llms.ChatClient backed by the Ollama /api/chat endpoint.
type Client struct {
	httpClient *http.Client
	baseURL    string
	model      string
}

// compile-time assertion lives in the test file.

func configErr(msg string) error {
	return &llms.ProviderError{Provider: providerName, Kind: llms.ErrorKindConfig, Message: msg}
}

// New builds a Client. It returns a *llms.ProviderError of kind config when
// required values are missing/invalid.
func New(opts ...Option) (*Client, error) {
	var s settings
	for _, o := range opts {
		o(&s)
	}
	if s.model == "" {
		return nil, configErr("missing model")
	}
	if s.hasRetries && s.maxRetries < 0 {
		return nil, configErr("max retries must be >= 0")
	}
	base := s.baseURL
	if base == "" {
		base = defaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	if u, err := url.Parse(base); err != nil || u.Scheme == "" || u.Host == "" {
		return nil, &llms.ProviderError{
			Provider: providerName, Kind: llms.ErrorKindConfig,
			Message: "invalid base URL: " + base, Cause: err,
		}
	}
	hc := s.httpClient
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{httpClient: hc, baseURL: base, model: s.model}, nil
}

// Model returns the model this client targets.
func (c *Client) Model() llms.ModelRef {
	return llms.ModelRef{Provider: providerName, Name: c.model}
}

// Complete sends a synchronous (non-streaming) chat completion.
func (c *Client) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
	wireReq, perr := c.toWire(req)
	if perr != nil {
		return llms.ChatResponse{}, perr
	}
	resp, err := c.do(ctx, wireReq)
	if err != nil {
		return llms.ChatResponse{}, err
	}
	return toChatResponse(resp), nil
}
