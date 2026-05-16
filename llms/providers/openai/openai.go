package openai

import (
	"context"
	"net/http"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

const providerName = "openai"

// maxBatchInputs caps inputs per request. OpenAI's embeddings endpoint accepts
// at most 2048 inputs; exceeding it is a caller error. Chunking is the
// application's responsibility (it owns retry policy, progress, and source
// IDs), so this is a hard validation error rather than auto-splitting.
const maxBatchInputs = 2048

// Option configures a Client. Applications own config resolution; this package
// accepts already-resolved values.
type Option func(*settings)

//nolint:govet // fieldalignment: internal one-shot config struct, layout irrelevant.
type settings struct {
	apiKey     string
	model      string
	baseURL    string
	httpClient option.HTTPClient
	dimensions int
	maxRetries int
	hasRetries bool
}

// WithAPIKey sets the OpenAI API key.
func WithAPIKey(key string) Option { return func(s *settings) { s.apiKey = key } }

// WithModel sets the embedding model identifier. Unknown/new names are
// accepted; the package does not gate on a registry.
func WithModel(model string) Option { return func(s *settings) { s.model = model } }

// WithBaseURL overrides the API base URL (proxies, tests).
func WithBaseURL(u string) Option { return func(s *settings) { s.baseURL = u } }

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

// WithDimensions sets the advisory default dimension reported by
// DefaultDimensions. A per-request EmbeddingRequest.Dimensions still overrides
// it when the model supports caller-selected dimensions.
func WithDimensions(d int) Option { return func(s *settings) { s.dimensions = d } }

// Client is an llms.EmbeddingClient backed by the official OpenAI SDK.
//
//nolint:govet // fieldalignment: size dominated by embedded third-party SDK client
type Client struct {
	api        openai.Client
	model      string
	dimensions int
}

// compile-time assertion lives in the test file (a package-level var would
// trip gochecknoglobals).

// New builds a Client. It returns a *llms.ProviderError of kind config when
// required values are missing or invalid.
func New(opts ...Option) (*Client, error) {
	var s settings
	for _, o := range opts {
		o(&s)
	}
	if s.apiKey == "" {
		return nil, &llms.ProviderError{Provider: providerName, Kind: llms.ErrorKindConfig, Message: "missing API key"}
	}
	if s.model == "" {
		return nil, &llms.ProviderError{Provider: providerName, Kind: llms.ErrorKindConfig, Message: "missing model"}
	}
	if s.hasRetries && s.maxRetries < 0 {
		return nil, &llms.ProviderError{Provider: providerName, Kind: llms.ErrorKindConfig, Message: "max retries must be >= 0"}
	}
	if s.dimensions < 0 {
		return nil, &llms.ProviderError{Provider: providerName, Kind: llms.ErrorKindConfig, Message: "dimensions must be >= 0"}
	}

	reqOpts := []option.RequestOption{option.WithAPIKey(s.apiKey)}
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

	return &Client{
		api:        openai.NewClient(reqOpts...),
		model:      s.model,
		dimensions: s.dimensions,
	}, nil
}

// Model returns the model this client targets.
func (c *Client) Model() llms.ModelRef {
	return llms.ModelRef{Provider: providerName, Name: c.model}
}

// DefaultDimensions returns the advisory configured dimension (0 if unset).
// The authoritative dimension is the length of each returned vector.
func (c *Client) DefaultDimensions() int { return c.dimensions }

// Embed returns one vector per input, in input order, tagged with input IDs.
func (c *Client) Embed(ctx context.Context, req llms.EmbeddingRequest) (llms.EmbeddingResponse, error) {
	if len(req.Inputs) == 0 {
		return llms.EmbeddingResponse{}, &llms.ProviderError{
			Provider: providerName, Model: c.model,
			Kind: llms.ErrorKindBadRequest, Message: "no embedding inputs",
		}
	}
	if len(req.Inputs) > maxBatchInputs {
		return llms.EmbeddingResponse{}, &llms.ProviderError{
			Provider: providerName, Model: c.model, Kind: llms.ErrorKindBadRequest,
			Message: "too many inputs for one request; the application must chunk",
		}
	}

	texts := make([]string, len(req.Inputs))
	for i := range req.Inputs {
		texts[i] = req.Inputs[i].Text
	}

	params := openai.EmbeddingNewParams{
		Model: c.model,
		Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: texts},
	}
	switch {
	case req.Dimensions > 0:
		params.Dimensions = openai.Int(int64(req.Dimensions))
	case c.dimensions > 0:
		params.Dimensions = openai.Int(int64(c.dimensions))
	}

	resp, err := c.api.Embeddings.New(ctx, params)
	if err != nil {
		return llms.EmbeddingResponse{}, classifyError(c.model, err)
	}
	return c.toResponse(req, resp)
}
