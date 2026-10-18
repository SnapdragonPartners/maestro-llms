package google

import (
	"context"
	"net/http"
	"strings"

	"cloud.google.com/go/auth"
	"google.golang.org/genai"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/internal/extraparams"
)

const providerName = llms.ProviderGoogle

// Option configures a Client or an EmbeddingClient: New and NewEmbeddings
// share one option type, so the backend options (WithAPIKey, WithVertex,
// WithCredentials, WithBaseURL, WithHTTPClient) are defined once (ADR-0027).
// An option that does not apply to the constructor it is passed to is a
// config error, never silently ignored. Applications own config resolution;
// this package accepts already-resolved values.
type Option func(*settings)

//nolint:govet // fieldalignment: internal one-shot config struct, layout irrelevant.
type settings struct {
	apiKey     string
	model      string
	baseURL    string
	httpClient *http.Client
	maxRetries int
	hasRetries bool

	// Vertex backend (WithVertex, WithCredentials).
	project  string
	location string
	creds    *auth.Credentials

	// Embeddings only (WithDimensions, WithMaxInputBytes, WithAutoTruncate).
	dims          int
	maxInputBytes int
	autoTruncate  bool

	// Names of the options set that only NewEmbeddings accepts, in order,
	// for New's error message. vertexOpts is a separate list because it is
	// the one #104 (Gemini chat on Vertex) will lift.
	embedOnly  []string
	vertexOpts []string
}

// WithAPIKey sets the Gemini API key and selects the Gemini API backend.
// It is mutually exclusive with WithVertex / WithCredentials.
func WithAPIKey(key string) Option { return func(s *settings) { s.apiKey = key } }

// WithModel sets the model identifier. Unknown/new names are accepted.
func WithModel(model string) Option { return func(s *settings) { s.model = model } }

// WithBaseURL overrides the API base URL (Private Service Connect endpoint,
// proxies, tests).
func WithBaseURL(u string) Option { return func(s *settings) { s.baseURL = u } }

// WithHTTPClient injects the HTTP client (PSC dialer / custom CA, timeouts,
// proxies, observability, test transports).
func WithHTTPClient(c *http.Client) Option { return func(s *settings) { s.httpClient = c } }

// WithMaxRetries is accepted for cross-provider symmetry and validated, but
// the genai SDK exposes no SDK-level retry knob; retries are maestro-llms
// middleware's job anyway (default 0 = no provider-SDK retries).
func WithMaxRetries(n int) Option {
	return func(s *settings) { s.maxRetries = n; s.hasRetries = true }
}

// WithVertex selects the Vertex AI backend in the given GCP project and
// location (e.g. "us-central1", "global"). It requires WithCredentials and
// is mutually exclusive with WithAPIKey. NewEmbeddings only, for now: New
// rejects it until Gemini chat on Vertex lands (#104).
func WithVertex(project, location string) Option {
	return func(s *settings) {
		s.project, s.location = project, location
		s.vertexOpts = append(s.vertexOpts, "WithVertex")
	}
}

// WithCredentials supplies the Google credentials for the Vertex backend
// (the cloud.google.com/go/auth type, e.g. from credentials.DetectDefault;
// anthropicvertex.WithCredentials takes the same type, ADR-0027). Required
// with WithVertex: the package does NO ADC discovery.
func WithCredentials(c *auth.Credentials) Option {
	return func(s *settings) {
		s.creds = c
		s.vertexOpts = append(s.vertexOpts, "WithCredentials")
	}
}

// WithDimensions sets the default output dimensionality (0 = model default);
// EmbeddingRequest.Dimensions overrides it per call. NewEmbeddings only.
func WithDimensions(n int) Option {
	return func(s *settings) {
		s.dims = n
		s.embedOnly = append(s.embedOnly, "WithDimensions")
	}
}

// WithMaxInputBytes sets the no-silent-truncation guard: unless
// WithAutoTruncate(true), an input whose Text exceeds this many UTF-8 bytes
// is rejected with a typed bad_request before the provider call. It is a
// literal byte count (not tokens — conservative, mechanical, auditable); the
// application sets it from its model/chunking policy. Required unless
// WithAutoTruncate(true). NewEmbeddings only.
func WithMaxInputBytes(n int) Option {
	return func(s *settings) {
		s.maxInputBytes = n
		s.embedOnly = append(s.embedOnly, "WithMaxInputBytes")
	}
}

// WithAutoTruncate controls oversized-input handling. genai's
// EmbedContentConfig.AutoTruncate is `bool,omitempty`, so a false value is
// UNREPRESENTABLE on the wire — genai cannot send autoTruncate:false, and
// Vertex then applies its server default (silently truncate). So:
//
//   - true: opt into provider/server truncation. Vertex-only (the Gemini API
//     rejects the parameter); construction fails on the Gemini-API backend.
//   - false (the default) + WithMaxInputBytes(n>0): this package enforces
//     no-silent-truncation itself by rejecting oversized input up front.
//   - false + no WithMaxInputBytes: NO guarantee is possible through genai,
//     so NewEmbeddings FAILS rather than look safe while Vertex silently
//     truncates (ADR-0009 refinement).
//
// NewEmbeddings only.
func WithAutoTruncate(on bool) Option {
	return func(s *settings) {
		s.autoTruncate = on
		s.embedOnly = append(s.embedOnly, "WithAutoTruncate")
	}
}

func applyOptions(opts []Option) settings {
	var s settings
	for _, o := range opts {
		o(&s)
	}
	return s
}

// commonErr checks the constraints both constructors share.
func (s *settings) commonErr() error {
	switch {
	case s.model == "":
		return configErr("missing model")
	case s.hasRetries && s.maxRetries < 0:
		return configErr("max retries must be >= 0")
	}
	return nil
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
	s := applyOptions(opts)
	switch {
	case len(s.embedOnly) > 0:
		return nil, configErr(strings.Join(s.embedOnly, ", ") + " applies only to NewEmbeddings")
	case len(s.vertexOpts) > 0:
		return nil, configErr(strings.Join(s.vertexOpts, ", ") +
			": the Vertex backend is not yet supported for chat (use the Gemini API key backend)")
	case s.apiKey == "":
		return nil, configErr("missing API key")
	}
	if err := s.commonErr(); err != nil {
		return nil, err
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
	extra, _, xerr := extraparams.For(&req, providerName)
	if xerr != nil {
		return llms.ChatResponse{}, badRequest(c.model, xerr.Error())
	}
	contents, cfg, perr := c.toParams(req)
	if perr != nil {
		return llms.ChatResponse{}, perr
	}
	if extra != nil {
		// The same deep merge every adapter uses (ADR-0024), not genai's
		// ExtraBody merge, which logs on type mismatches.
		cfg.HTTPOptions = &genai.HTTPOptions{ExtrasRequestProvider: func(body map[string]any) map[string]any {
			extraparams.Merge(body, extra)
			return body
		}}
	}
	result, err := c.api.Models.GenerateContent(ctx, c.model, contents, cfg)
	if err != nil {
		return llms.ChatResponse{}, classifyError(c.model, err)
	}
	out := toChatResponse(result)
	_, applied := mapReasoning(req.Reasoning)
	out.AppliedReasoning = withoutOverridden(applied, extra)
	return out, nil
}
