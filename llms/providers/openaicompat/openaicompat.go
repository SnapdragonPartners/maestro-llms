package openaicompat

import (
	"context"
	"net/http"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/internal/extraparams"
)

// providerName is the llms.ModelRef.Provider value for this package.
const providerName = llms.ProviderOpenAICompat

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

// WithBaseURL sets the server's base URL, including the API version path
// (e.g. "http://localhost:8000/v1" for vLLM, "https://gw.lunaroute.com/v1"
// for LunaRoute). Required — there is no canonical default endpoint. The SDK
// appends only the route ("chat/completions", "models"), not "/v1".
func WithBaseURL(u string) Option { return func(s *settings) { s.baseURL = u } }

// WithModel sets the model identifier. This must match an ID that the
// running vLLM instance is serving (see /v1/models). Unknown names are
// accepted at construction; the server rejects them at call time.
func WithModel(model string) Option { return func(s *settings) { s.model = model } }

// WithAPIKey sets the bearer token (a gateway key such as LunaRoute's, or
// VLLM_API_KEY on a vLLM instance). Optional: vLLM's default install has no
// auth and the SDK will still send a request without a key. Unlike the OpenAI and
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

// Client is an llms.ChatClient backed by an OpenAI-compatible Chat
// Completions endpoint (vLLM, LunaRoute, and other compatible servers).
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

	// Not openai.NewClient: it prepends DefaultClientOptions, which read
	// OPENAI_API_KEY, OPENAI_BASE_URL, OPENAI_ADMIN_KEY, OPENAI_ORG_ID,
	// OPENAI_PROJECT_ID and OPENAI_CUSTOM_HEADERS from the environment. With
	// no WithAPIKey, an ambient OPENAI_API_KEY would be sent as a bearer
	// token to whatever server baseURL names. openai-go v3's opt-out is
	// internal API, so only the services this package uses are constructed
	// (divergence X6).
	api := openai.Client{
		Options: reqOpts,
		Chat:    openai.NewChatService(reqOpts...),
		Models:  openai.NewModelService(reqOpts...),
	}
	return &Client{api: api, model: s.model}, nil
}

// Model returns the model this client targets.
func (c *Client) Model() llms.ModelRef {
	return llms.ModelRef{Provider: providerName, Name: c.model}
}

// Complete sends a synchronous chat completion via the OpenAI Chat
// Completions endpoint.
func (c *Client) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
	extra, _, err := extraparams.For(&req, providerName)
	if err != nil {
		return llms.ChatResponse{}, chatBadRequest(c.model, err.Error())
	}
	params, perr := c.toParams(req)
	if perr != nil {
		return llms.ChatResponse{}, perr
	}
	var opts []option.RequestOption
	if extra != nil {
		opts = append(opts, option.WithMiddleware(extraparams.Middleware(extra))) //nolint:bodyclose // the SDK owns and closes the response
	}
	resp, apiErr := c.api.Chat.Completions.New(ctx, params, opts...)
	if apiErr != nil {
		return llms.ChatResponse{}, classifyError(c.model, apiErr)
	}
	out := c.toResponse(resp)
	_, applied := mapEffort(req.Reasoning)
	if extraparams.Overrides(extra, "reasoning_effort") {
		applied = nil // ProviderParams replaced the mapped effort (ADR-0024)
	}
	out.AppliedReasoning = applied
	return out, nil
}
