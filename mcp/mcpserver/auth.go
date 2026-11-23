package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// Auth makes the HTTP endpoint an OAuth resource server (ADR-0050 §7).
type Auth struct {
	// Verifier checks a bearer token. Required.
	Verifier TokenVerifier
	// Resource is the server's canonical URI (RFC 8707, RFC 9728): absolute,
	// https (http only for a loopback host), with no user info, query or
	// fragment. Required.
	Resource string
	// Audience is the value a token's Audience must contain. Empty means
	// Resource, as MCP requires; set it only for an authorization server that
	// names this resource by another identifier.
	Audience string
	// ResourceName and ResourceDocumentation go in the metadata. Optional.
	ResourceName, ResourceDocumentation string
	// AuthorizationServers are the issuer URLs published in the metadata. At
	// least one, each absolute.
	AuthorizationServers []string
	// Scopes are required on every request; all must be present.
	Scopes []string
	// ScopesSupported is published in the metadata. Nil publishes Scopes.
	ScopesSupported []string
	// ClockSkew is the tolerance applied to Expiration. Default 0.
	ClockSkew time.Duration
	// AllowMissingExpiration accepts a token whose TokenInfo has a zero
	// Expiration. By default such a token is rejected.
	AllowMissingExpiration bool
}

// TokenVerifier verifies a bearer token: its signature (or introspection),
// issuer, expiry and not-before. It returns what the token asserts; the
// server then checks the subject, audience, expiration and scopes itself.
// An error wrapping ErrInvalidToken is the token's fault (401); any other
// error is the server's (500, reported to Options.OnError). Its text never
// reaches the client.
type TokenVerifier func(ctx context.Context, token string, r *http.Request) (TokenInfo, error)

// ErrInvalidToken is what a TokenVerifier wraps for a token it rejects.
var ErrInvalidToken = errors.New("invalid token")

// TokenInfo is what a verified token asserts. Tools read it with
// TokenInfoFromContext; they must not modify its slices or map.
type TokenInfo struct {
	// Expiration is when the token expires. Zero is rejected unless
	// Auth.AllowMissingExpiration is set.
	Expiration time.Time
	// Extra carries anything else the verifier wants tools to see.
	Extra map[string]any
	// Subject identifies the user. Required.
	Subject string
	// Audience is the token's audiences: for a JWT its aud claim, for
	// introspection its aud (or resource) field. It must contain
	// Auth.Audience (default Auth.Resource).
	Audience []string
	// Scopes are the token's granted scopes.
	Scopes []string
}

type tokenInfoKey struct{}

// TokenInfoFromContext returns the verified token's info inside a tool's
// Execute. ok is false on a server without Auth, and over stdio.
func TokenInfoFromContext(ctx context.Context) (TokenInfo, bool) {
	info, ok := ctx.Value(tokenInfoKey{}).(TokenInfo)
	return info, ok
}

// sdkExtraKey is the key under which the verified TokenInfo rides in the
// go-sdk's TokenInfo.Extra, a map this package builds itself, from the
// HTTP request to the tool handler.
const sdkExtraKey = "github.com/SnapdragonPartners/maestro-llms/mcp/mcpserver.TokenInfo"

func infoFromSDK(ti *auth.TokenInfo) (TokenInfo, bool) {
	if ti == nil {
		return TokenInfo{}, false
	}
	info, ok := ti.Extra[sdkExtraKey].(TokenInfo)
	return info, ok
}

// wellKnownPrefix is RFC 9728's well-known URI suffix.
const wellKnownPrefix = "/.well-known/oauth-protected-resource"

// authConfig is Auth checked and with its derived values.
type authConfig struct {
	verifier     TokenVerifier
	metadata     []byte // the RFC 9728 document
	audience     string
	metadataPath string // where the metadata is mounted
	metadataURL  string // resource_metadata in challenges
	resourceHost string // Resource's host, for the default Host check
	scopes       []string
	skew         time.Duration
	allowNoExp   bool
}

func newAuthConfig(a *Auth) (*authConfig, error) {
	if a.Verifier == nil {
		return nil, errors.New("mcpserver: Auth.Verifier is required")
	}
	u, err := checkResource(a.Resource)
	if err != nil {
		return nil, fmt.Errorf("mcpserver: Auth.Resource: %w", err)
	}
	if len(a.AuthorizationServers) == 0 {
		return nil, errors.New("mcpserver: Auth.AuthorizationServers needs at least one issuer URL")
	}
	for _, as := range a.AuthorizationServers {
		if aerr := checkAbsolute(as); aerr != nil {
			return nil, fmt.Errorf("mcpserver: Auth.AuthorizationServers: %q: %w", as, aerr)
		}
	}
	supported := a.ScopesSupported
	if supported == nil {
		supported = a.Scopes
	}
	for _, list := range [][]string{a.Scopes, supported} {
		for _, sc := range list {
			if serr := checkScope(sc); serr != nil {
				return nil, fmt.Errorf("mcpserver: Auth scopes: %w", serr)
			}
		}
	}
	if a.ClockSkew < 0 {
		return nil, errors.New("mcpserver: Auth.ClockSkew must not be negative")
	}

	path := u.EscapedPath()
	if path == "/" {
		path = ""
	}
	c := &authConfig{
		verifier:     a.Verifier,
		audience:     a.Audience,
		metadataPath: wellKnownPrefix + path,
		scopes:       slices.Clone(a.Scopes),
		skew:         a.ClockSkew,
		allowNoExp:   a.AllowMissingExpiration,
		resourceHost: u.Hostname(),
	}
	if c.audience == "" {
		c.audience = a.Resource
	}
	c.metadataURL = (&url.URL{Scheme: u.Scheme, Host: u.Host}).String() + c.metadataPath

	doc := struct {
		Resource              string   `json:"resource"`
		ResourceName          string   `json:"resource_name,omitempty"`
		ResourceDocumentation string   `json:"resource_documentation,omitempty"`
		AuthorizationServers  []string `json:"authorization_servers"`
		ScopesSupported       []string `json:"scopes_supported,omitempty"`
		BearerMethods         []string `json:"bearer_methods_supported"`
	}{
		Resource:              a.Resource,
		ResourceName:          a.ResourceName,
		ResourceDocumentation: a.ResourceDocumentation,
		AuthorizationServers:  a.AuthorizationServers,
		ScopesSupported:       supported,
		BearerMethods:         []string{"header"},
	}
	if c.metadata, err = json.Marshal(doc); err != nil {
		return nil, fmt.Errorf("mcpserver: metadata: %w", err)
	}
	return c, nil
}

// checkResource requires an absolute https URI (http for a loopback host)
// with a host and no user info, query or fragment.
func checkResource(s string) (*url.URL, error) {
	if strings.ContainsAny(s, "#\"\\") {
		return nil, fmt.Errorf("%q has a fragment, quote or backslash", s)
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	switch {
	case u.Host == "" || u.Opaque != "":
		return nil, fmt.Errorf("%q is not an absolute URI with a host", s)
	case u.User != nil:
		return nil, fmt.Errorf("%q has user info", s)
	case u.RawQuery != "" || u.ForceQuery:
		return nil, fmt.Errorf("%q has a query", s)
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopback(u.Hostname()):
	default:
		return nil, fmt.Errorf("%q must use https (http only for a loopback host)", s)
	}
	return u, nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func checkAbsolute(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return errors.New("not an absolute URL")
	}
	return nil
}

// checkScope applies RFC 6749 §3.3's scope-token grammar, so a scope can be
// quoted in a WWW-Authenticate header as it is.
func checkScope(s string) error {
	if s == "" {
		return errors.New("empty scope")
	}
	for _, r := range s {
		if r < 0x21 || r > 0x7e || r == '"' || r == '\\' {
			return fmt.Errorf("scope %q has a character RFC 6749 doesn't allow", s)
		}
	}
	return nil
}

// MetadataHandler serves the protected resource metadata (RFC 9728) for a
// server with Options.Auth, and is nil without it. It answers GET and the
// CORS preflight; the metadata is public.
func (s *Server) MetadataHandler() http.Handler {
	if s.auth == nil {
		return nil
	}
	doc := s.auth.metadata
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, MCP-Protocol-Version")
		switch r.Method {
		case http.MethodOptions:
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet, http.MethodHead:
			h.Set("Content-Type", "application/json")
			_, _ = w.Write(doc)
		default:
			h.Set("Allow", "GET, HEAD, OPTIONS")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// MetadataPath is where the app mounts MetadataHandler, at the root of
// Auth.Resource's origin: the RFC 9728 well-known path with Resource's path
// appended (for https://dot.example.com/mcp,
// /.well-known/oauth-protected-resource/mcp). Empty without Options.Auth.
func (s *Server) MetadataPath() string { return s.metaPath }

// requireToken is the bearer-token check in front of the MCP handler
// (ADR-0050 §7). Every decision a client sees is made here, with fixed
// response bodies; the go-sdk's RequireBearerToken runs after it only to
// carry the verified info into the SDK's per-request TokenInfo.
func (s *Server) requireToken(next http.Handler) http.Handler {
	c := s.auth
	sdk := auth.RequireBearerToken(func(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
		info, ok := ctx.Value(tokenInfoKey{}).(TokenInfo)
		if !ok {
			return nil, errors.New("mcpserver: token info missing")
		}
		return &auth.TokenInfo{
			UserID: info.Subject,
			Scopes: info.Scopes,
			Extra:  map[string]any{sdkExtraKey: info},
		}, nil
	}, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(next)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			c.challenge(w, http.StatusUnauthorized, "", "missing bearer token")
			return
		}
		info, err := c.verifier(r.Context(), token, r)
		if err != nil {
			if errors.Is(err, ErrInvalidToken) {
				c.challenge(w, http.StatusUnauthorized, "invalid_token", "invalid token")
				return
			}
			s.report(r.Context(), fmt.Errorf("mcpserver: verify token: %w", err))
			http.Error(w, "token verification failed", http.StatusInternalServerError)
			return
		}
		if err := c.check(&info, time.Now()); err != nil {
			c.challenge(w, http.StatusUnauthorized, "invalid_token", "invalid token")
			return
		}
		if !hasAll(info.Scopes, c.scopes) {
			c.challenge(w, http.StatusForbidden, "insufficient_scope", "insufficient scope")
			return
		}
		sdk.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenInfoKey{}, info)))
	})
}

// bearerToken extracts the token from "Authorization: Bearer <token>", with
// the same parsing as the go-sdk (case-insensitive scheme, one token).
func bearerToken(r *http.Request) (string, bool) {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "bearer") {
		return "", false
	}
	return fields[1], true
}

// check applies the server's own token rules: a subject, the audience, and
// the expiration.
func (c *authConfig) check(info *TokenInfo, now time.Time) error {
	switch {
	case info.Subject == "":
		return errors.New("no subject")
	case !slices.Contains(info.Audience, c.audience):
		return errors.New("wrong audience")
	case info.Expiration.IsZero():
		if !c.allowNoExp {
			return errors.New("no expiration")
		}
	case info.Expiration.Add(c.skew).Before(now):
		return errors.New("expired")
	}
	return nil
}

func hasAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// challenge writes an RFC 6750 bearer challenge with the metadata URL
// (RFC 9728), the required scopes, and errCode when set.
func (c *authConfig) challenge(w http.ResponseWriter, status int, errCode, body string) {
	params := make([]string, 0, 3)
	if errCode != "" {
		params = append(params, `error="`+errCode+`"`)
	}
	if len(c.scopes) > 0 {
		params = append(params, `scope="`+strings.Join(c.scopes, " ")+`"`)
	}
	params = append(params, `resource_metadata="`+c.metadataURL+`"`)
	w.Header().Set("WWW-Authenticate", "Bearer "+strings.Join(params, ", "))
	http.Error(w, body, status)
}
