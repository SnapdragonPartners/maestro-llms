package anthropic

import (
	"context"
	"net/http"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// providerName is the llms.ModelRef.Provider value for this package.
const providerName = "anthropic"

// defaultMaxTokens is used when a ChatRequest leaves MaxTokens unset; the
// Anthropic API requires a positive max_tokens.
const defaultMaxTokens = 4096

// Option configures a Client. Applications own config resolution; this package
// only accepts already-resolved values.
type Option func(*settings)

//nolint:govet // fieldalignment: internal one-shot config struct, layout irrelevant.
type settings struct {
	apiKey       string
	model        string
	baseURL      string
	httpClient   option.HTTPClient
	maxRetries   int
	hasRetries   bool
	extraReqOpts []option.RequestOption
}

// WithAPIKey sets the Anthropic API key.
func WithAPIKey(key string) Option { return func(s *settings) { s.apiKey = key } }

// WithModel sets the model identifier. Unknown/new model names are accepted;
// the package does not gate on a registry.
func WithModel(model string) Option { return func(s *settings) { s.model = model } }

// WithBaseURL overrides the API base URL (useful for proxies and tests).
func WithBaseURL(u string) Option { return func(s *settings) { s.baseURL = u } }

// WithHTTPClient injects the HTTP client (timeouts, proxies, observability,
// test transports). Any *http.Client satisfies this.
func WithHTTPClient(c *http.Client) Option {
	return func(s *settings) { s.httpClient = c }
}

// WithMaxRetries sets SDK-level retries. Default is 0: retries are expected to
// be handled by maestro-llms middleware, not the provider SDK.
func WithMaxRetries(n int) Option {
	return func(s *settings) { s.maxRetries = n; s.hasRetries = true }
}

// WithRequestOptions is a low-level escape hatch: the given SDK request
// options are applied LAST, after the ones derived from the other Options, so
// they take precedence (e.g. they can override the base URL or replace auth).
// This is how alternate backends (e.g. Anthropic via Vertex AI — see the
// anthropicvertex subpackage) inject their auth/endpoint WITHOUT this base
// package importing any backend SDK, preserving leaf imports. Supplying
// request options makes the API key optional: when present, the caller owns
// authentication.
func WithRequestOptions(opts ...option.RequestOption) Option {
	return func(s *settings) { s.extraReqOpts = append(s.extraReqOpts, opts...) }
}

// Client is an llms.ChatClient backed by the official Anthropic SDK. Its size
// is dominated by the embedded third-party anthropic.Client, so reordering our
// two fields cannot meaningfully change layout.
//
//nolint:govet // fieldalignment: size dominated by embedded third-party SDK client
type Client struct {
	api   anthropic.Client
	model string
}

// compile-time assertion lives in the test file (a package-level var would
// trip gochecknoglobals).

// New builds a Client. It returns a *llms.ProviderError of kind config when
// required values are missing.
func New(opts ...Option) (*Client, error) {
	var s settings
	for _, o := range opts {
		o(&s)
	}
	// The API key is required UNLESS the caller supplied request options:
	// those carry the caller's own auth (e.g. Vertex), so the key is theirs
	// to own, not ours to demand.
	if s.apiKey == "" && len(s.extraReqOpts) == 0 {
		return nil, &llms.ProviderError{Provider: providerName, Kind: llms.ErrorKindConfig, Message: "missing API key"}
	}
	if s.model == "" {
		return nil, &llms.ProviderError{Provider: providerName, Kind: llms.ErrorKindConfig, Message: "missing model"}
	}
	if s.hasRetries && s.maxRetries < 0 {
		return nil, &llms.ProviderError{Provider: providerName, Kind: llms.ErrorKindConfig, Message: "max retries must be >= 0"}
	}

	var reqOpts []option.RequestOption
	if s.apiKey != "" {
		reqOpts = append(reqOpts, option.WithAPIKey(s.apiKey))
	}
	if s.baseURL != "" {
		reqOpts = append(reqOpts, option.WithBaseURL(s.baseURL))
	}
	if s.httpClient != nil {
		reqOpts = append(reqOpts, option.WithHTTPClient(s.httpClient))
	}
	retries := 0
	if s.hasRetries {
		retries = s.maxRetries
	}
	reqOpts = append(reqOpts, option.WithMaxRetries(retries))
	// Caller-supplied options last: they win over the derived ones.
	reqOpts = append(reqOpts, s.extraReqOpts...)

	return &Client{api: anthropic.NewClient(reqOpts...), model: s.model}, nil
}

// Model returns the model this client targets.
func (c *Client) Model() llms.ModelRef {
	return llms.ModelRef{Provider: providerName, Name: c.model}
}

// Complete sends a synchronous chat completion.
func (c *Client) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
	params, err := c.toParams(req)
	if err != nil {
		return llms.ChatResponse{}, err
	}
	msg, apiErr := c.api.Messages.New(ctx, params)
	if apiErr != nil {
		return llms.ChatResponse{}, classifyError(c.model, apiErr)
	}
	return toResponse(msg), nil
}
