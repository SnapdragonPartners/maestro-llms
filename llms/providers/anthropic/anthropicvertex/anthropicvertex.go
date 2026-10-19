// Package anthropicvertex constructs an Anthropic chat client that talks to
// Claude via Google Vertex AI. It is a separate leaf package on purpose: it
// imports github.com/anthropics/anthropic-sdk-go/vertex (which pulls Google
// auth dependencies), so only consumers that actually use Vertex pay that
// cost — the base llms/providers/anthropic package stays SDK-backend-free
// (ADR-0009).
//
// It returns the same *anthropic.Client as the base package, so all request/
// response/tool/cache translation is reused unchanged; only construction,
// auth, and endpoint differ.
//
// Auth and endpoint are app-supplied. The package performs no ADC discovery:
// the caller passes *auth.Credentials (cloud.google.com/go/auth), and for
// Private Service Connect a custom endpoint and *http.Client.
//
// Precedence (ADR-0029, amending ADR-0009): vertex.WithCredentials installs
// the Vertex request middleware (path rewrite to the rawPredict shape +
// anthropic_version injection), its own oauth2 HTTP client and the regional
// base URL. The caller's PSC client is applied BEFORE it, so the SDK wraps
// that client's transport with the credentials (on a shallow copy; the
// caller's client is not mutated); the caller's endpoint is applied AFTER
// it, so it wins over the regional URL. So:
//
//   - the Vertex path/version middleware ALWAYS applies;
//   - WithCredentials ALWAYS authenticates, with or without a PSC client;
//   - the PSC client supplies only the network layer (dialer, CA, proxy).
//
// This matches every other provider, where WithHTTPClient never removes
// auth.
package anthropicvertex

import (
	"context"
	"net/http"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/oauth2adapt"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/vertex"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/anthropic"
)

const providerName = llms.ProviderAnthropic

// Option configures the Vertex-backed client. Applications own config
// resolution; this package accepts already-resolved values.
type Option func(*settings)

//nolint:govet // fieldalignment: internal one-shot config struct, layout irrelevant.
type settings struct {
	ctx        context.Context //nolint:containedctx // construction-time ctx for the SDK auth client
	region     string
	projectID  string
	model      string
	creds      *auth.Credentials
	endpoint   string
	httpClient *http.Client
}

// WithContext sets the context used to build the Vertex auth client; defaults
// to context.Background().
func WithContext(ctx context.Context) Option { return func(s *settings) { s.ctx = ctx } }

// WithRegion sets the Vertex region (e.g. "us-central1", "global"). Required.
func WithRegion(r string) Option { return func(s *settings) { s.region = r } }

// WithProjectID sets the GCP project ID. Required (the Vertex path includes it).
func WithProjectID(p string) Option { return func(s *settings) { s.projectID = p } }

// WithModel sets the Vertex Anthropic model identifier (Vertex naming, e.g.
// "claude-sonnet-4@20250514"). Required.
func WithModel(m string) Option { return func(s *settings) { s.model = m } }

// WithCredentials supplies the Google credentials, as the
// cloud.google.com/go/auth type that google.WithCredentials also takes
// (ADR-0027; e.g. from credentials.DetectDefault). Required: the package does
// no ADC discovery. The adapter converts them for the Anthropic Vertex SDK
// and uses them to install the official Vertex middleware. They also
// authenticate requests sent through a WithHTTPClient client (ADR-0029).
func WithCredentials(c *auth.Credentials) Option { return func(s *settings) { s.creds = c } }

// WithEndpoint overrides the base URL (Private Service Connect endpoint).
// Applied after the Vertex option so it wins over the regional URL.
func WithEndpoint(url string) Option { return func(s *settings) { s.endpoint = url } }

// WithHTTPClient injects the network client for PSC (dialer, custom CA,
// proxy, observability). It needs no Google auth of its own: the SDK wraps a
// shallow copy of it with WithCredentials (ADR-0029). The caller's client is
// not mutated.
func WithHTTPClient(c *http.Client) Option { return func(s *settings) { s.httpClient = c } }

func cfgErr(msg string) error {
	return &llms.ProviderError{Provider: providerName, Kind: llms.ErrorKindConfig, Message: msg}
}

// New builds an Anthropic-via-Vertex chat client. It returns a
// *llms.ProviderError of kind config when required values are missing.
func New(opts ...Option) (*anthropic.Client, error) {
	var s settings
	for _, o := range opts {
		o(&s)
	}
	if s.ctx == nil {
		s.ctx = context.Background()
	}
	// vertex.WithCredentials panics on an empty region or nil creds; validate
	// up front and return a typed config error instead.
	switch {
	case s.region == "":
		return nil, cfgErr("missing Vertex region")
	case s.projectID == "":
		return nil, cfgErr("missing GCP project ID")
	case s.model == "":
		return nil, cfgErr("missing model")
	case s.creds == nil:
		return nil, cfgErr("missing Google credentials (this package does no ADC discovery)")
	case s.creds.TokenProvider == nil:
		// A Credentials with no TokenProvider converts to an oauth2
		// Credentials whose token source cannot mint tokens, so every call
		// would fail (or, with a nil TokenSource, Google's transport would
		// fall back to ambient ADC, violating the app-supplied-credentials
		// contract). Reject it up front.
		return nil, cfgErr("Google credentials have no TokenProvider (refusing to fall back to ADC)")
	}
	// The Anthropic Vertex SDK takes the older golang.org/x/oauth2 type; the
	// conversion is internal so the public API names one credential type.
	creds := oauth2adapt.Oauth2CredentialsFromAuthCredentials(s.creds)

	// Order is load-bearing (ADR-0029): the caller's PSC client BEFORE the
	// Vertex option, which wraps it with the credentials (the SDK's
	// documented behavior; passed after, it would replace the authenticated
	// client and drop auth). The PSC endpoint AFTER it, so it wins over the
	// regional URL. The Vertex middleware applies either way.
	// The client is passed as a shallow copy because the SDK treats the
	// pointer http.DefaultClient as "no client" and would substitute its
	// own, silently dropping an app's PSC transport set on DefaultClient.
	var reqOpts []option.RequestOption
	if s.httpClient != nil {
		hc := *s.httpClient
		reqOpts = append(reqOpts, option.WithHTTPClient(&hc))
	}
	reqOpts = append(reqOpts, vertex.WithCredentials(s.ctx, s.region, s.projectID, creds))
	if s.endpoint != "" {
		reqOpts = append(reqOpts, option.WithBaseURL(s.endpoint))
	}

	c, err := anthropic.New(anthropic.WithModel(s.model), anthropic.WithRequestOptions(reqOpts...))
	if err != nil {
		return nil, err //nolint:wrapcheck // already a typed *llms.ProviderError from the base package
	}
	return c, nil
}
