package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
	"github.com/SnapdragonPartners/maestro-llms/mcp/mcpclient"
)

const resource = "https://dot.example.com/mcp"

func okVerifier(context.Context, string, *http.Request) (TokenInfo, error) {
	return TokenInfo{Subject: "u", Audience: []string{resource}, Expiration: time.Now().Add(time.Hour)}, nil
}

func validAuth() *Auth {
	return &Auth{
		Resource:             resource,
		AuthorizationServers: []string{"https://auth.example.com"},
		Verifier:             okVerifier,
	}
}

func TestAuthConfigInvalid(t *testing.T) {
	cases := map[string]struct {
		mod  func(*Auth)
		want string
	}{
		"no verifier":       {func(a *Auth) { a.Verifier = nil }, "Verifier is required"},
		"no resource":       {func(a *Auth) { a.Resource = "" }, "absolute URI"},
		"relative resource": {func(a *Auth) { a.Resource = "/mcp" }, "absolute URI"},
		"opaque resource":   {func(a *Auth) { a.Resource = "urn:x:y" }, "absolute URI"},
		"http resource":     {func(a *Auth) { a.Resource = "http://dot.example.com/mcp" }, "https"},
		"ftp resource":      {func(a *Auth) { a.Resource = "ftp://dot.example.com" }, "https"},
		"fragment":          {func(a *Auth) { a.Resource = resource + "#x" }, "fragment"},
		"empty fragment":    {func(a *Auth) { a.Resource = resource + "#" }, "fragment"},
		"query":             {func(a *Auth) { a.Resource = resource + "?a=b" }, "query"},
		"empty query":       {func(a *Auth) { a.Resource = resource + "?" }, "query"},
		"user info":         {func(a *Auth) { a.Resource = "https://u:p@dot.example.com" }, "user info"},
		"quote":             {func(a *Auth) { a.Resource = `https://dot.example.com/"` }, "quote"},
		"no issuers":        {func(a *Auth) { a.AuthorizationServers = nil }, "at least one"},
		"relative issuer":   {func(a *Auth) { a.AuthorizationServers = []string{"auth.example.com"} }, "absolute"},
		"empty scope":       {func(a *Auth) { a.Scopes = []string{""} }, "empty scope"},
		"spaced scope":      {func(a *Auth) { a.Scopes = []string{"a b"} }, "RFC 6749"},
		"quoted scope":      {func(a *Auth) { a.ScopesSupported = []string{`a"`} }, "RFC 6749"},
		"negative skew":     {func(a *Auth) { a.ClockSkew = -time.Second }, "ClockSkew"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			a := validAuth()
			c.mod(a)
			_, err := New(nil, Options{Auth: a})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("New error = %v, want it to contain %q", err, c.want)
			}
		})
	}
	for _, r := range []string{"http://localhost:8080/mcp", "http://127.0.0.1:9", "http://[::1]/x", "https://dot.example.com"} {
		a := validAuth()
		a.Resource = r
		if _, err := New(nil, Options{Auth: a}); err != nil {
			t.Errorf("Resource %q: %v", r, err)
		}
	}
}

func TestMetadata(t *testing.T) {
	cases := []struct{ resource, path, url string }{
		{"https://dot.example.com/mcp", "/.well-known/oauth-protected-resource/mcp", "https://dot.example.com/.well-known/oauth-protected-resource/mcp"},
		{"https://dot.example.com", "/.well-known/oauth-protected-resource", "https://dot.example.com/.well-known/oauth-protected-resource"},
		{"https://dot.example.com/", "/.well-known/oauth-protected-resource", "https://dot.example.com/.well-known/oauth-protected-resource"},
		{"https://dot.example.com:8443/a/b c", "/.well-known/oauth-protected-resource/a/b%20c", "https://dot.example.com:8443/.well-known/oauth-protected-resource/a/b%20c"},
	}
	for _, c := range cases {
		a := validAuth()
		a.Resource = c.resource
		s, err := New(nil, Options{Auth: a})
		if err != nil {
			t.Fatal(err)
		}
		if s.MetadataPath() != c.path || s.auth.metadataURL != c.url {
			t.Errorf("%s: path %q url %q, want %q %q", c.resource, s.MetadataPath(), s.auth.metadataURL, c.path, c.url)
		}
	}

	a := validAuth()
	a.Scopes = []string{"vault:read"}
	a.ResourceName = "Dot"
	s, err := New(nil, Options{Auth: a})
	if err != nil {
		t.Fatal(err)
	}
	h := s.MetadataHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, s.MetadataPath(), nil))
	want := `{"resource":"https://dot.example.com/mcp","resource_name":"Dot","authorization_servers":["https://auth.example.com"],"scopes_supported":["vault:read"],"bearer_methods_supported":["header"]}`
	if rec.Code != 200 || rec.Body.String() != want || rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("GET: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, s.MetadataPath(), nil))
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Fatalf("OPTIONS: %d %v", rec.Code, rec.Header())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, s.MetadataPath(), nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rec.Code)
	}

	// ScopesSupported overrides Scopes in the metadata only.
	a.ScopesSupported = []string{"vault:read", "vault:write"}
	s, _ = New(nil, Options{Auth: a})
	var doc map[string]any
	_ = json.Unmarshal(s.auth.metadata, &doc)
	if fmt.Sprint(doc["scopes_supported"]) != "[vault:read vault:write]" {
		t.Fatalf("scopes_supported %v", doc["scopes_supported"])
	}

	plain, _ := New(nil, Options{})
	if plain.MetadataHandler() != nil || plain.MetadataPath() != "" {
		t.Fatal("metadata without Auth")
	}
}

func TestRequireToken(t *testing.T) {
	now := time.Now()
	const md = `resource_metadata="https://dot.example.com/.well-known/oauth-protected-resource/mcp"`
	info := func(mod func(*TokenInfo)) TokenVerifier {
		return func(context.Context, string, *http.Request) (TokenInfo, error) {
			ti := TokenInfo{Subject: "u", Audience: []string{"other", resource}, Scopes: []string{"a", "b"}, Expiration: now.Add(time.Minute)}
			if mod != nil {
				mod(&ti)
			}
			return ti, nil
		}
	}
	cases := map[string]struct {
		verifier  TokenVerifier
		mod       func(*Auth)
		header    string
		status    int
		challenge string
		reported  string
	}{
		"no header":  {info(nil), nil, "", 401, "Bearer " + md, ""},
		"basic":      {info(nil), nil, "Basic dTpw", 401, "Bearer " + md, ""},
		"no token":   {info(nil), nil, "Bearer", 401, "Bearer " + md, ""},
		"two tokens": {info(nil), nil, "Bearer a b", 401, "Bearer " + md, ""},
		"scope in challenge": {info(nil), func(a *Auth) { a.Scopes = []string{"a", "c"} }, "", 401,
			`Bearer scope="a c", ` + md, ""},
		"invalid token": {func(context.Context, string, *http.Request) (TokenInfo, error) {
			return TokenInfo{}, fmt.Errorf("sig: %w", ErrInvalidToken)
		}, nil, "Bearer t", 401, `Bearer error="invalid_token", ` + md, ""},
		"verifier failure": {func(context.Context, string, *http.Request) (TokenInfo, error) {
			return TokenInfo{}, errors.New("jwks fetch from 10.1.2.3 failed")
		}, nil, "Bearer t", 500, "", "10.1.2.3"},
		"no subject":      {info(func(ti *TokenInfo) { ti.Subject = "" }), nil, "Bearer t", 401, `Bearer error="invalid_token", ` + md, ""},
		"wrong audience":  {info(func(ti *TokenInfo) { ti.Audience = []string{"https://other.example.com/mcp"} }), nil, "Bearer t", 401, `Bearer error="invalid_token", ` + md, ""},
		"no audience":     {info(func(ti *TokenInfo) { ti.Audience = nil }), nil, "Bearer t", 401, `Bearer error="invalid_token", ` + md, ""},
		"audience prefix": {info(func(ti *TokenInfo) { ti.Audience = []string{resource + "/x"} }), nil, "Bearer t", 401, `Bearer error="invalid_token", ` + md, ""},
		"custom audience needed": {info(nil), func(a *Auth) { a.Audience = "api://dot" }, "Bearer t", 401,
			`Bearer error="invalid_token", ` + md, ""},
		"expired":       {info(func(ti *TokenInfo) { ti.Expiration = now.Add(-time.Minute) }), nil, "Bearer t", 401, `Bearer error="invalid_token", ` + md, ""},
		"no expiration": {info(func(ti *TokenInfo) { ti.Expiration = time.Time{} }), nil, "Bearer t", 401, `Bearer error="invalid_token", ` + md, ""},
		"missing scope": {info(nil), func(a *Auth) { a.Scopes = []string{"a", "c"} }, "Bearer t", 403,
			`Bearer error="insufficient_scope", scope="a c", ` + md, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			a := validAuth()
			a.Verifier = c.verifier
			if c.mod != nil {
				c.mod(a)
			}
			var reported []error
			s, err := New(nil, Options{Auth: a, OnError: func(_ context.Context, err error) { reported = append(reported, err) }})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d (%s)", rec.Code, c.status, rec.Body)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != c.challenge {
				t.Fatalf("WWW-Authenticate\n got %s\nwant %s", got, c.challenge)
			}
			if c.reported != "" {
				if len(reported) != 1 || !strings.Contains(reported[0].Error(), c.reported) {
					t.Fatalf("OnError got %v", reported)
				}
				if strings.Contains(rec.Body.String(), c.reported) {
					t.Fatalf("body leaks the verifier error: %q", rec.Body)
				}
			} else if len(reported) != 0 {
				t.Fatalf("OnError called: %v", reported)
			}
		})
	}
}

// TestRequireTokenAccepts covers the passing side of each rule, through a
// real MCP call so the TokenInfo reaching Execute is checked too.
func TestRequireTokenAccepts(t *testing.T) {
	now := time.Now()
	cases := map[string]struct {
		mod  func(*Auth)
		info TokenInfo
	}{
		"plain":           {nil, TokenInfo{Subject: "u", Audience: []string{resource}, Expiration: now.Add(time.Minute)}},
		"custom audience": {func(a *Auth) { a.Audience = "api://dot" }, TokenInfo{Subject: "u", Audience: []string{"api://dot"}, Expiration: now.Add(time.Minute)}},
		"within skew":     {func(a *Auth) { a.ClockSkew = time.Minute }, TokenInfo{Subject: "u", Audience: []string{resource}, Expiration: now.Add(-30 * time.Second)}},
		"no expiration allowed": {func(a *Auth) { a.AllowMissingExpiration = true },
			TokenInfo{Subject: "u", Audience: []string{resource}}},
		"scopes": {func(a *Auth) { a.Scopes = []string{"b", "a"} },
			TokenInfo{Subject: "u", Audience: []string{resource}, Scopes: []string{"a", "b", "c"}, Expiration: now.Add(time.Minute)}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			a := validAuth()
			a.Verifier = func(context.Context, string, *http.Request) (TokenInfo, error) { return c.info, nil }
			if c.mod != nil {
				c.mod(a)
			}
			res := callOverHTTP(t, a, "t", "whoami")
			if !strings.HasPrefix(res, "u ") {
				t.Fatalf("whoami = %q", res)
			}
		})
	}
}

type bearer struct {
	base  http.RoundTripper
	token string
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// serveHTTP mounts a server like an app would, with Resource on the test
// server's loopback URL, and returns that URL.
func serveHTTP(t *testing.T, a *Auth, opts Options) string {
	t.Helper()
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	if a != nil {
		a.Resource = ts.URL + "/mcp"
		if a.Audience == "" {
			v := a.Verifier
			a.Verifier = func(ctx context.Context, tok string, r *http.Request) (TokenInfo, error) {
				ti, err := v(ctx, tok, r)
				for i, aud := range ti.Audience {
					if aud == resource {
						ti.Audience[i] = ts.URL + "/mcp"
					}
				}
				return ti, err
			}
		}
	}
	opts.Auth = a
	s, err := New(testTools(), opts)
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle("/mcp", s.Handler())
	if a != nil {
		mux.Handle(s.MetadataPath(), s.MetadataHandler())
	}
	return ts.URL
}

func connectHTTP(t *testing.T, url, token string) *mcpclient.Session {
	t.Helper()
	hc := &http.Client{Transport: bearer{http.DefaultTransport, token}}
	sess, err := mcpclient.Connect(context.Background(), mcpclient.StreamableHTTP(url+"/mcp", hc), mcpclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func callOverHTTP(t *testing.T, a *Auth, token, name string) string {
	t.Helper()
	url := serveHTTP(t, a, Options{})
	sess := connectHTTP(t, url, token)
	tools, err := sess.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools {
		if tl.Definition.Name == name {
			res, err := tl.Execute(context.Background(), llms.ToolCall{Name: name})
			if err != nil {
				t.Fatal(err)
			}
			return res.Content
		}
	}
	t.Fatalf("no tool %s", name)
	return ""
}

// TestEndToEnd drives every result kind through mcpclient over HTTP with
// OAuth: the two halves of the module against each other.
func TestEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var reported []error
	a := validAuth()
	a.Scopes = []string{"vault:read"}
	a.Verifier = func(_ context.Context, tok string, _ *http.Request) (TokenInfo, error) {
		if tok != "good" {
			return TokenInfo{}, ErrInvalidToken
		}
		return TokenInfo{
			Subject: "alice", Audience: []string{resource}, Scopes: []string{"vault:read"},
			Expiration: time.Now().Add(time.Hour), Extra: map[string]any{"tenant": "t1"},
		}, nil
	}
	url := serveHTTP(t, a, Options{OnError: func(_ context.Context, err error) {
		mu.Lock()
		reported = append(reported, err)
		mu.Unlock()
	}})

	// The metadata is where the challenge says.
	ctx := context.Background()
	post, _ := http.NewRequestWithContext(ctx, http.MethodPost, url+"/mcp", strings.NewReader("{}"))
	resp, err := http.DefaultClient.Do(post)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	ch := resp.Header.Get("WWW-Authenticate")
	mdURL := ch[strings.Index(ch, `resource_metadata="`)+len(`resource_metadata="`) : len(ch)-1]
	get, _ := http.NewRequestWithContext(ctx, http.MethodGet, mdURL, http.NoBody)
	resp, err = http.DefaultClient.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"resource":"`+url+`/mcp"`) {
		t.Fatalf("metadata at %s: %d %s", mdURL, resp.StatusCode, body)
	}

	// A bad token can't connect.
	hc := &http.Client{Transport: bearer{http.DefaultTransport, "bad"}}
	if _, cerr := mcpclient.Connect(ctx, mcpclient.StreamableHTTP(url+"/mcp", hc), mcpclient.Options{}); cerr == nil {
		t.Fatal("connected with a bad token")
	}

	sess := connectHTTP(t, url, "good")
	tools, err := sess.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]func(json.RawMessage) (string, bool, error){}
	for _, tl := range tools {
		by[tl.Definition.Name] = func(args json.RawMessage) (string, bool, error) {
			res, xerr := tl.Execute(ctx, llms.ToolCall{Name: tl.Definition.Name, Parameters: args})
			if len(res.Parts) > 0 {
				var kinds []string
				for _, p := range res.Parts {
					kinds = append(kinds, string(p.Type))
				}
				return strings.Join(kinds, ","), res.IsError, xerr
			}
			return res.Content, res.IsError, xerr
		}
	}
	check := func(name, args, want string, wantIsError bool) {
		t.Helper()
		got, isErr, xerr := by[name](json.RawMessage(args))
		if xerr != nil || got != want || isErr != wantIsError {
			t.Fatalf("%s = %q, %v, %v; want %q, %v", name, got, isErr, xerr, want, wantIsError)
		}
	}
	check("echo", `{"msg":"hi"}`, `args: {"msg":"hi"}`, false)
	check("whoami", "", "alice vault:read t1", false)
	check("media", "", "text,image,document", false)
	check("refuse", "", "not allowed", true)

	_, _, err = by["boom"](nil)
	if err == nil || !strings.Contains(err.Error(), `tool "boom" failed`) || strings.Contains(err.Error(), "10.0.0.7") {
		t.Fatalf("boom error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 1 || !strings.Contains(reported[0].Error(), "10.0.0.7") {
		t.Fatalf("OnError got %v", reported)
	}
}

// TestDroppedConnection pins what the go-sdk does when the HTTP client goes
// away mid-call (ADR-0050 §3): the package adds no cancellation of its own,
// and in v1.8.0 the call runs on.
func TestDroppedConnection(t *testing.T) {
	started := make(chan struct{})
	outcome := make(chan string, 1)
	s, err := New([]toolloop.Tool{tool("slow", "", func(ctx context.Context, _ llms.ToolCall) (toolloop.ToolResult, error) {
		close(started)
		select {
		case <-ctx.Done():
			outcome <- "canceled"
		case <-time.After(time.Second):
			outcome <- "ran to completion"
		}
		return text("done")
	})}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow","arguments":{}}}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	errc := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			err = fmt.Errorf("unexpected response %d %s", resp.StatusCode, b)
		}
		errc <- err
	}()
	select {
	case <-started:
	case err := <-errc:
		t.Fatalf("call never reached Execute: %v", err)
	}
	cancel()
	<-errc
	select {
	case got := <-outcome:
		t.Logf("dropped connection: Execute %s", got)
		// go-sdk v1.8.0 detaches the call from the HTTP request, so a client
		// that goes away does not cancel Execute. If this changes, update
		// ADR-0050 §3 and the package doc.
		if got != "ran to completion" {
			t.Fatalf("go-sdk behavior changed: Execute %s after the client went away", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Execute never returned")
	}
}
