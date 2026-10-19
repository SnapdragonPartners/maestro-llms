package google

import (
	"context"
	"net/http"
	"strings"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/httptransport"
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
	// for New's error message.
	embedOnly []string
	// vertex records that WithVertex or WithCredentials was set at all:
	// either one selects the Vertex backend (ADR-0027).
	vertex bool
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
// proxies, observability, test transports). It needs no Google auth of its
// own: on the Vertex backend the package authenticates a shallow copy of it
// with WithCredentials, as anthropicvertex does (ADR-0029). The caller's
// client is not mutated.
func WithHTTPClient(c *http.Client) Option { return func(s *settings) { s.httpClient = c } }

// WithMaxRetries is accepted for cross-provider symmetry and validated, but
// the genai SDK exposes no SDK-level retry knob; retries are maestro-llms
// middleware's job anyway (default 0 = no provider-SDK retries).
func WithMaxRetries(n int) Option {
	return func(s *settings) { s.maxRetries = n; s.hasRetries = true }
}

// WithVertex selects the Vertex AI backend in the given GCP project and
// location (e.g. "us-central1", "global"). It requires WithCredentials and
// is mutually exclusive with WithAPIKey. It applies to both New (Gemini chat
// on Vertex, ADR-0028) and NewEmbeddings.
func WithVertex(project, location string) Option {
	return func(s *settings) {
		s.project, s.location = project, location
		s.vertex = true
	}
}

// WithCredentials supplies the Google credentials for the Vertex backend
// (the cloud.google.com/go/auth type, e.g. from credentials.DetectDefault;
// anthropicvertex.WithCredentials takes the same type, ADR-0027). Required
// with WithVertex: the package does NO ADC discovery.
func WithCredentials(c *auth.Credentials) Option {
	return func(s *settings) {
		s.creds = c
		s.vertex = true
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

// clientConfig checks the constraints both constructors share and builds
// the genai client config: the backend (the Gemini API by key, or Vertex
// with app-supplied credentials; ambiguity fails closed) plus the endpoint
// and transport injection. It is the one place a backend is selected, so
// chat and embeddings cannot drift apart (ADR-0028).
func (s *settings) clientConfig() (*genai.ClientConfig, error) {
	switch {
	case s.model == "":
		return nil, configErr("missing model")
	case s.hasRetries && s.maxRetries < 0:
		return nil, configErr("max retries must be >= 0")
	case s.apiKey != "" && s.vertex:
		// genai treats API key and Vertex (project/location/credentials) as
		// mutually exclusive. Fail closed rather than silently pick the
		// public API-key backend and bypass Vertex/PSC.
		return nil, configErr("ambiguous backend: set WithAPIKey (Gemini API) OR WithVertex+WithCredentials (Vertex), not both")
	}
	cc := &genai.ClientConfig{}
	switch {
	case s.apiKey != "":
		cc.Backend = genai.BackendGeminiAPI
		cc.APIKey = s.apiKey
	case s.vertex:
		switch {
		case s.project == "" || s.location == "":
			return nil, configErr("Vertex backend requires WithVertex with a non-empty project and location")
		case s.creds == nil:
			return nil, configErr("Vertex backend requires WithCredentials (this package does no ADC discovery)")
		case s.creds.TokenProvider == nil:
			// Credentials that cannot mint tokens fail every call; reject
			// them up front, as anthropicvertex does.
			return nil, configErr("Google credentials have no TokenProvider (refusing to fall back to ADC)")
		}
		cc.Backend = genai.BackendVertexAI
		cc.Project, cc.Location, cc.Credentials = s.project, s.location, s.creds
	default:
		return nil, configErr("missing credentials: set WithAPIKey (Gemini API) or WithVertex+WithCredentials (Vertex)")
	}
	if err := s.applyTransport(cc); err != nil {
		return nil, err
	}
	return cc, nil
}

// applyTransport applies WithBaseURL and WithHTTPClient to cc. On Vertex the
// injected client is authenticated with the credentials: genai uses
// Credentials only for a client it builds itself, so the client would
// otherwise send no auth (ADR-0029).
func (s *settings) applyTransport(cc *genai.ClientConfig) error {
	if s.baseURL != "" {
		cc.HTTPOptions.BaseURL = s.baseURL
	}
	if s.httpClient == nil {
		return nil
	}
	cc.HTTPClient = s.httpClient
	if cc.Backend == genai.BackendVertexAI {
		hc, err := authorizeClient(s.httpClient, s.creds)
		if err != nil {
			return err
		}
		cc.HTTPClient = hc
	}
	return nil
}

// authorizeClient returns a shallow copy of c whose transport attaches
// creds' token (and quota project, if any) over c's own transport, the way
// genai authorizes the client it builds when none is injected. c keeps its
// Timeout, Jar and CheckRedirect and is not mutated.
func authorizeClient(c *http.Client, creds *auth.Credentials) (*http.Client, error) {
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	quota, err := creds.QuotaProjectID(context.Background())
	if err != nil {
		return nil, &llms.ProviderError{
			Provider: providerName, Kind: llms.ErrorKindConfig,
			Message: "reading the credentials' quota project failed", Cause: err,
		}
	}
	opts := &httptransport.Options{Credentials: creds, BaseRoundTripper: base}
	if quota != "" {
		opts.Headers = http.Header{"X-Goog-User-Project": []string{quota}}
	}
	authed, err := httptransport.NewClient(opts)
	if err != nil {
		return nil, &llms.ProviderError{
			Provider: providerName, Kind: llms.ErrorKindConfig,
			Message: "authorizing the injected HTTP client failed", Cause: err,
		}
	}
	wrapped := *c
	wrapped.Transport = authed.Transport
	return &wrapped, nil
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

// New builds a Client. The backend is selected by the options, as for
// NewEmbeddings: WithAPIKey -> the direct Gemini API; WithVertex +
// WithCredentials -> Gemini on Vertex AI (ADR-0028). WithBaseURL /
// WithHTTPClient are the Private Service Connect injection points. It
// returns a *llms.ProviderError of kind config when required values are
// missing/invalid or the SDK client cannot be created.
func New(opts ...Option) (*Client, error) {
	s := applyOptions(opts)
	if len(s.embedOnly) > 0 {
		return nil, configErr(strings.Join(s.embedOnly, ", ") + " applies only to NewEmbeddings")
	}
	cfg, err := s.clientConfig()
	if err != nil {
		return nil, err
	}
	// genai.NewClient builds transport/config only (no network call: the
	// credentials are app-supplied, never discovered); a non-nil error here
	// is a config problem.
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

// Complete sends a synchronous chat completion via the configured backend
// (the Gemini API or Vertex AI; the wire mapping is the same for both).
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
