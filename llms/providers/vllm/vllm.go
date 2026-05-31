package vllm

import (
	"context"
	"net/http"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// providerName is the llms.ModelRef.Provider value for this package.
const providerName = "vllm"

// Option configures a Client. Applications own config resolution; this
// package accepts already-resolved values.
type Option func(*settings)

//nolint:govet // fieldalignment: internal one-shot config struct, layout irrelevant.
type settings struct {
	apiKey     string
	model      string
	baseURL    string
	httpClient option.HTTPClient
	maxRetries int
	hasRetries bool
}

// WithBaseURL sets the vLLM server's base URL (e.g. "http://localhost:8000").
// Required — vLLM has no canonical default endpoint the way Ollama does.
// The /v1 path suffix is appended by the SDK; pass only the host:port root.
func WithBaseURL(u string) Option { return func(s *settings) { s.baseURL = u } }

// WithModel sets the model identifier. This must match an ID that the
// running vLLM instance is serving (see /v1/models). Unknown names are
// accepted at construction; the server rejects them at call time.
func WithModel(model string) Option { return func(s *settings) { s.model = model } }

// WithAPIKey sets a bearer token for vLLM instances configured with
// VLLM_API_KEY. Optional: vLLM's default install has no auth and the
// SDK will still send a request without a key. Unlike the OpenAI and
// Anthropic adapters, an empty key is NOT a configuration error here.
func WithAPIKey(key string) Option { return func(s *settings) { s.apiKey = key } }

// WithHTTPClient injects the HTTP client (timeouts, proxies, observability,
// test transports). Any *http.Client satisfies this.
func WithHTTPClient(c *http.Client) Option {
	return func(s *settings) { s.httpClient = c }
}

// WithMaxRetries sets SDK-level retries. Default 0: retries are handled by
// maestro-llms middleware, not the provider SDK.
func WithMaxRetries(n int) Option {
	return func(s *settings) { s.maxRetries = n; s.hasRetries = true }
}

// Client is an llms.ChatClient backed by vLLM's OpenAI-compatible Chat
// Completions endpoint.
//
//nolint:govet // fieldalignment: size dominated by embedded third-party SDK client
type Client struct {
	api   openai.Client
	model string
}

// compile-time assertions live in the test file.

func configErr(msg string) error {
	return &llms.ProviderError{Provider: providerName, Kind: llms.ErrorKindConfig, Message: msg}
}

// New builds a Client. It returns a *llms.ProviderError of kind config when
// required values are missing or invalid.
//
// Required: WithBaseURL, WithModel. WithAPIKey is optional (vLLM's
// default deployment has no auth).
func New(opts ...Option) (*Client, error) {
	var s settings
	for _, o := range opts {
		o(&s)
	}
	if s.baseURL == "" {
		return nil, configErr("missing base URL")
	}
	if s.model == "" {
		return nil, configErr("missing model")
	}
	if s.hasRetries && s.maxRetries < 0 {
		return nil, configErr("max retries must be >= 0")
	}

	reqOpts := []option.RequestOption{option.WithBaseURL(s.baseURL)}
	// API key is optional: vLLM's default deployment has no auth. The
	// SDK accepts an empty key and just omits the Authorization header.
	if s.apiKey != "" {
		reqOpts = append(reqOpts, option.WithAPIKey(s.apiKey))
	}
	if s.httpClient != nil {
		reqOpts = append(reqOpts, option.WithHTTPClient(s.httpClient))
	}
	retries := 0
	if s.hasRetries {
		retries = s.maxRetries
	}
	reqOpts = append(reqOpts, option.WithMaxRetries(retries))

	return &Client{api: openai.NewClient(reqOpts...), model: s.model}, nil
}

// Model returns the model this client targets.
func (c *Client) Model() llms.ModelRef {
	return llms.ModelRef{Provider: providerName, Name: c.model}
}

// Complete sends a synchronous chat completion to vLLM via the OpenAI
// Chat Completions endpoint.
func (c *Client) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
	params, err := c.toParams(req)
	if err != nil {
		return llms.ChatResponse{}, err
	}
	resp, apiErr := c.api.Chat.Completions.New(ctx, params)
	if apiErr != nil {
		return llms.ChatResponse{}, classifyError(c.model, apiErr)
	}
	return c.toResponse(resp), nil
}
