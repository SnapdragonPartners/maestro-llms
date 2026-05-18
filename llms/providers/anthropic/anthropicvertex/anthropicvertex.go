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
// the caller passes *google.Credentials, and for Private Service Connect a
// custom endpoint and *http.Client.
//
// Precedence (the sharp edge — see ADR-0009): vertex.WithCredentials installs
// the Vertex request middleware (path rewrite to the rawPredict shape +
// anthropic_version injection) AND its own oauth2 HTTP client + regional base
// URL. We apply the caller's PSC endpoint/client AFTER it, so:
//
//   - the Vertex path/version middleware ALWAYS applies (it is independent of
//     the HTTP client);
//   - the network transport and base URL are whatever is applied LAST.
//
// Therefore a PSC *http.Client overrides Vertex's self-built client, which
// discards Vertex's auth layer: the caller's PSC client MUST itself carry
// Google auth (oauth2 over the PSC transport). That is the application's /
// OpenTofu infrastructure concern, not this package's.
package anthropicvertex

import (
	"context"
	"net/http"

	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/vertex"
	"golang.org/x/oauth2/google"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/anthropic"
)

const providerName = "anthropic"

// Option configures the Vertex-backed client. Applications own config
// resolution; this package accepts already-resolved values.
type Option func(*settings)

//nolint:govet // fieldalignment: internal one-shot config struct, layout irrelevant.
type settings struct {
	ctx        context.Context //nolint:containedctx // construction-time ctx for the SDK auth client
	region     string
	projectID  string
	model      string
	creds      *google.Credentials
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

// WithCredentials supplies the Google credentials. Required: the package does
// no ADC discovery. Used to install the official Vertex middleware; for PSC
// (see WithHTTPClient) the network client is replaced, so the credentials'
// token source must also be carried by that client.
func WithCredentials(c *google.Credentials) Option { return func(s *settings) { s.creds = c } }

// WithEndpoint overrides the base URL (Private Service Connect endpoint).
// Applied after the Vertex option so it wins over the regional URL.
func WithEndpoint(url string) Option { return func(s *settings) { s.endpoint = url } }

// WithHTTPClient injects the network client for PSC. It is applied after the
// Vertex option, so it REPLACES Vertex's self-built oauth2 client: this
// client MUST carry Google auth itself (oauth2 layered over the PSC
// transport). Building that client is the application's concern.
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
	}

	// Order is load-bearing (ADR-0009): Vertex option first (middleware +
	// auth client + regional URL), then the caller's PSC endpoint/client
	// LAST so they override the transport/URL while the middleware stays.
	reqOpts := []option.RequestOption{
		vertex.WithCredentials(s.ctx, s.region, s.projectID, s.creds),
	}
	if s.endpoint != "" {
		reqOpts = append(reqOpts, option.WithBaseURL(s.endpoint))
	}
	if s.httpClient != nil {
		reqOpts = append(reqOpts, option.WithHTTPClient(s.httpClient))
	}

	c, err := anthropic.New(anthropic.WithModel(s.model), anthropic.WithRequestOptions(reqOpts...))
	if err != nil {
		return nil, err //nolint:wrapcheck // already a typed *llms.ProviderError from the base package
	}
	return c, nil
}
