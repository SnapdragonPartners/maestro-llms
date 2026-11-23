package mcpserver

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAllowedHostsInvalid(t *testing.T) {
	for _, h := range []string{
		"", ".", "-x.example.com", ".example.com", "mcp.example.com:443", "https://mcp.example.com",
		"mcp.example.com/mcp", "*.example.com", "user@mcp.example.com", "[::1]:8080", "mcp example.com",
	} {
		_, err := New(testTools(), Options{AllowedHosts: []string{"ok.example.com", h}})
		if err == nil || !strings.Contains(err.Error(), "AllowedHosts") {
			t.Errorf("AllowedHosts %q: err = %v", h, err)
		}
	}
	for _, h := range []string{"MCP.Example.com", "mcp.example.com.", "127.0.0.1", "::1", "[::1]", "localhost", "my_host"} {
		if _, err := New(testTools(), Options{AllowedHosts: []string{h}}); err != nil {
			t.Errorf("AllowedHosts %q: %v", h, err)
		}
	}
}

type addr string

func (a addr) Network() string { return "tcp" }
func (a addr) String() string  { return string(a) }

// TestHostCheckAllows covers the matching rules without a listener, so the
// local address can be a non-loopback one.
func TestHostCheckAllows(t *testing.T) {
	explicit, err := allowlistCheck([]string{"MCP.example.com", "::1", "10.0.0.5"})
	if err != nil {
		t.Fatal(err)
	}
	byAuth := resourceCheck("dot.example.com")
	for name, opts := range map[string]Options{
		"zero":               {},
		"empty AllowedHosts": {AllowedHosts: []string{}},
	} {
		if s, _ := New(testTools(), opts); s.hosts != nil {
			t.Fatalf("%s: want the SDK's check (nil hosts)", name)
		}
	}

	const loop, public = "127.0.0.1:8080", "10.0.0.5:8080"
	cases := []struct {
		check *hostCheck
		name  string
		local string // "" = no local address in the context
		host  string
		want  bool
	}{
		{explicit, "listed", loop, "mcp.example.com", true},
		{explicit, "case and port", loop, "Mcp.Example.COM:443", true},
		{explicit, "trailing dot", loop, "mcp.example.com.", true},
		{explicit, "ipv6 listed", loop, "[::1]:9000", true},
		{explicit, "ip listed", public, "10.0.0.5", true},
		{explicit, "not listed", loop, "evil.example.com", false},
		{explicit, "loopback not implied", loop, "127.0.0.1:8080", false},
		{explicit, "localhost not implied", loop, "localhost", false},
		{explicit, "applies off loopback", public, "evil.example.com", false},
		{explicit, "applies without local addr", "", "evil.example.com", false},
		{explicit, "suffix is not a match", loop, "x.mcp.example.com", false},
		{explicit, "empty host", loop, "", false},
		{byAuth, "resource host", loop, "dot.example.com", true},
		{byAuth, "resource host with port", loop, "DOT.example.com:443", true},
		{byAuth, "loopback ip", loop, "127.0.0.1:8080", true},
		{byAuth, "localhost", loop, "localhost:8080", true},
		{byAuth, "ipv6 loopback", "[::1]:8080", "[::1]:8080", true},
		{byAuth, "other host on loopback", loop, "evil.example.com", false},
		{byAuth, "other host on ipv6 loopback", "[::1]:8080", "evil.example.com", false},
		{byAuth, "other host off loopback", public, "evil.example.com", true},
		{byAuth, "no local addr", "", "evil.example.com", true},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/mcp", http.NoBody)
		r.Host = c.host
		if c.local != "" {
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, net.Addr(addr(c.local))))
		}
		if got := c.check.allows(r); got != c.want {
			t.Errorf("%s: allows(Host %q, local %q) = %v, want %v", c.name, c.host, c.local, got, c.want)
		}
	}
}

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

// post sends initialize to url with the given Host header and token ("" for
// none) and returns the status and body.
func post(t *testing.T, url, host, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(initializeBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestHostsOverLoopback drives Handler on a real loopback listener, where a
// reverse proxy would forward the public Host (#188).
func TestHostsOverLoopback(t *testing.T) {
	serve := func(t *testing.T, opts Options) string {
		t.Helper()
		s, err := New(testTools(), opts)
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(s.Handler())
		t.Cleanup(ts.Close)
		return ts.URL
	}
	local := func(url string) string { return strings.TrimPrefix(url, "http://") }

	t.Run("default without auth is the SDK's check", func(t *testing.T) {
		url := serve(t, Options{})
		if code, body := post(t, url, local(url), ""); code != http.StatusOK {
			t.Fatalf("loopback Host: %d %s", code, body)
		}
		code, body := post(t, url, "dot.example.com", "")
		if code != http.StatusForbidden || !strings.Contains(body, "dot.example.com") {
			t.Fatalf("public Host: %d %s; want the SDK's 403", code, body)
		}
	})

	t.Run("default with auth adds the resource host", func(t *testing.T) {
		url := serve(t, Options{Auth: validAuth()}) // Resource https://dot.example.com/mcp
		if code, body := post(t, url, "dot.example.com", "good"); code != http.StatusOK {
			t.Fatalf("resource Host: %d %s", code, body)
		}
		if code, body := post(t, url, local(url), "good"); code != http.StatusOK {
			t.Fatalf("loopback Host: %d %s", code, body)
		}
		// A wrong Host is refused before the token is looked at.
		for _, tok := range []string{"good", ""} {
			code, body := post(t, url, "evil.example.com", tok)
			if code != http.StatusForbidden || strings.Contains(body, "evil") {
				t.Fatalf("other Host, token %q: %d %s; want a fixed 403", tok, code, body)
			}
		}
		if code, _ := post(t, url, "dot.example.com", ""); code != http.StatusUnauthorized {
			t.Fatalf("resource Host without a token: %d; want 401", code)
		}
	})

	t.Run("explicit list replaces the default", func(t *testing.T) {
		url := serve(t, Options{Auth: validAuth(), AllowedHosts: []string{"mcp.example.org"}})
		if code, body := post(t, url, "mcp.example.org:443", "good"); code != http.StatusOK {
			t.Fatalf("listed Host: %d %s", code, body)
		}
		for _, h := range []string{"dot.example.com", local(url)} {
			if code, _ := post(t, url, h, "good"); code != http.StatusForbidden {
				t.Fatalf("Host %q: %d; want 403", h, code)
			}
		}
	})

	t.Run("explicit list without auth", func(t *testing.T) {
		url := serve(t, Options{AllowedHosts: []string{"mcp.example.org", "127.0.0.1"}})
		for _, h := range []string{"mcp.example.org", local(url)} {
			if code, body := post(t, url, h, ""); code != http.StatusOK {
				t.Fatalf("Host %q: %d %s", h, code, body)
			}
		}
		if code, _ := post(t, url, "evil.example.com", ""); code != http.StatusForbidden {
			t.Fatalf("other Host: %d; want 403", code)
		}
	})
}
