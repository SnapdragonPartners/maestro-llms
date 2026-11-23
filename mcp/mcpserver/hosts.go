package mcpserver

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// hostCheck is the DNS-rebinding protection in front of the MCP handler
// when this package, not the go-sdk, owns it (ADR-0050 §6, amended for
// #188): with Options.AllowedHosts, or by default with Options.Auth.
type hostCheck struct {
	allowed map[string]bool // normalized hosts
	// loopback also accepts any loopback Host (localhost, 127.0.0.0/8, ::1).
	loopback bool
	// loopbackConnOnly applies the check only to requests that arrive on a
	// loopback address, as the go-sdk's check does; others pass.
	loopbackConnOnly bool
}

// setHosts picks the Host check from Options.AllowedHosts and Auth (set
// first). Without either, s.hosts stays nil and the go-sdk's own loopback
// check stays in place.
func (s *Server) setHosts(allowedHosts []string) error {
	switch {
	case len(allowedHosts) > 0:
		c, err := allowlistCheck(allowedHosts)
		if err != nil {
			return err
		}
		s.hosts = c
	case s.auth != nil:
		s.hosts = resourceCheck(s.auth.resourceHost)
	}
	return nil
}

// allowlistCheck is the check for Options.AllowedHosts: exactly those
// hosts, on every request.
func allowlistCheck(allowedHosts []string) (*hostCheck, error) {
	c := &hostCheck{allowed: make(map[string]bool, len(allowedHosts))}
	for _, h := range allowedHosts {
		n, err := checkAllowedHost(h)
		if err != nil {
			return nil, fmt.Errorf("mcpserver: Options.AllowedHosts: %q: %w", h, err)
		}
		c.allowed[n] = true
	}
	return c, nil
}

// resourceCheck is the default with Options.Auth: on a request arriving on
// a loopback address, a loopback Host or Resource's host; other requests
// pass. This is the go-sdk's check with Resource's host added, so a server
// listening on loopback behind a reverse proxy answers for its public name.
func resourceCheck(resourceHost string) *hostCheck {
	return &hostCheck{
		allowed:          map[string]bool{normalizeHost(resourceHost): true},
		loopback:         true,
		loopbackConnOnly: true,
	}
}

// checkAllowedHost validates and normalizes an AllowedHosts entry: a host
// name or IP address (IPv6 with or without brackets), no port, scheme, path
// or wildcard.
func checkAllowedHost(h string) (string, error) {
	if h == "" {
		return "", errors.New("empty host")
	}
	n := strings.TrimSuffix(strings.ToLower(trimBrackets(h)), ".")
	if _, err := netip.ParseAddr(n); err == nil {
		return normalizeHost(n), nil
	}
	if strings.Contains(n, ":") {
		return "", errors.New("has a port or scheme; list the host name only")
	}
	if n == "" || n[0] == '.' || n[0] == '-' {
		return "", errors.New("not a host name")
	}
	for _, r := range n {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '-' && r != '_' {
			return "", fmt.Errorf("character %q is not allowed in a host name (no wildcards or paths)", r)
		}
	}
	return n, nil
}

// normalizeHost reduces a Host header (or host) to the form AllowedHosts
// entries are stored in: port and IPv6 brackets dropped, lower case, no
// trailing dot, IP addresses in canonical form (IPv4-mapped IPv6 as IPv4).
func normalizeHost(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.TrimSuffix(strings.ToLower(trimBrackets(h)), ".")
	if ip, err := netip.ParseAddr(h); err == nil {
		return ip.Unmap().String()
	}
	return h
}

func trimBrackets(h string) string {
	if len(h) >= 2 && h[0] == '[' && h[len(h)-1] == ']' {
		return h[1 : len(h)-1]
	}
	return h
}

// allows reports whether r may reach the MCP handler.
func (c *hostCheck) allows(r *http.Request) bool {
	if c.loopbackConnOnly {
		// As the go-sdk: no local address (a handler called directly) or a
		// non-loopback one is not what this check guards.
		local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
		if !ok || local == nil || !isLoopback(normalizeHost(local.String())) {
			return true
		}
	}
	host := normalizeHost(r.Host)
	return c.allowed[host] || (c.loopback && isLoopback(host))
}

// wrap puts the check in front of next. The 403 body is fixed: the Host
// header is not echoed back.
func (c *hostCheck) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !c.allows(r) {
			http.Error(w, "host not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
