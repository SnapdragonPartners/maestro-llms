package openai

import (
	"net/http"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

const providerName = llms.ProviderOpenAI

// Option configures a Client or an EmbeddingClient. The options are shared
// by both constructors; an option that does not apply to the constructor it
// is passed to is a config error, never silently ignored (as in the google
// package). Applications own config resolution; this package accepts
// already-resolved values.
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

	// Names of the options set that only NewEmbeddings accepts, in order,
	// for New's error message.
	embedOnly []string
}

// WithAPIKey sets the OpenAI API key.
func WithAPIKey(key string) Option { return func(s *settings) { s.apiKey = key } }

// WithModel sets the model identifier (a chat model for New, an embedding
// model for NewEmbeddings). Unknown/new names are
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
// it when the model supports caller-selected dimensions. NewEmbeddings only.
func WithDimensions(d int) Option {
	return func(s *settings) {
		s.dimensions = d
		s.embedOnly = append(s.embedOnly, "WithDimensions")
	}
}

func configErr(msg string) error {
	return &llms.ProviderError{Provider: providerName, Kind: llms.ErrorKindConfig, Message: msg}
}

// validate checks the common required config. checkDimensions is embedding-only.
func (s *settings) validate(checkDimensions bool) error {
	switch {
	case s.apiKey == "":
		return configErr("missing API key")
	case s.model == "":
		return configErr("missing model")
	case s.hasRetries && s.maxRetries < 0:
		return configErr("max retries must be >= 0")
	case checkDimensions && s.dimensions < 0:
		return configErr("dimensions must be >= 0")
	default:
		return nil
	}
}

// newAPI builds the SDK client from app-supplied options only.
// openai.NewClient prepends DefaultClientOptions, which read OPENAI_BASE_URL,
// OPENAI_API_KEY, OPENAI_ADMIN_KEY, OPENAI_ORG_ID, OPENAI_PROJECT_ID and
// OPENAI_CUSTOM_HEADERS from the environment, so an ambient OPENAI_BASE_URL
// would redirect traffic (with the app's key) and ambient headers would be
// attached. openai-go v3's opt-out (requestconfig.WithEnvironmentDefaultsDisabled)
// is internal API, so the services this package uses are constructed
// directly (divergence X6). Without an app HTTP client the SDK falls back
// to http.DefaultClient, as in v1; timeouts belong to the Timeout middleware.
func newAPI(opts []option.RequestOption) openai.Client {
	return openai.Client{
		Options:    opts,
		Embeddings: openai.NewEmbeddingService(opts...),
		Models:     openai.NewModelService(opts...),
		Responses:  responses.NewResponseService(opts...),
	}
}

// requestOptions builds the SDK request options shared by all clients.
// WithEnvironmentProduction supplies the api.openai.com default base URL that
// DefaultClientOptions would otherwise have provided (see newAPI).
func (s *settings) requestOptions() []option.RequestOption {
	reqOpts := []option.RequestOption{option.WithEnvironmentProduction(), option.WithAPIKey(s.apiKey)}
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
	return append(reqOpts, option.WithMaxRetries(retries))
}
