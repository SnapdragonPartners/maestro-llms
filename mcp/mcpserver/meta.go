package mcpserver

import (
	"context"

	"github.com/SnapdragonPartners/maestro-llms/mcp/internal/convert"
)

type metaKey struct{}

// MetaFromContext returns the _meta a client sent with the tools/call
// request, inside the tool's Execute: per-call context that is not a tool
// argument, such as a run ID or an idempotency key (mcpclient sends it
// through Options.Meta, ADR-0053). ok is false when the request carried no
// keys besides MCP's own.
//
// Keys MCP reserves are left out: a prefix with a label
// "modelcontextprotocol" or "mcp" (the go-sdk's io.modelcontextprotocol/...
// keys) and progressToken. Every other key is passed as the client sent it,
// without checking its format. Values are as the MCP SDK decoded them from
// JSON: numbers are float64, objects map[string]any, arrays []any.
//
// The map belongs to this call: each request gets its own, so a tool may
// modify it.
func MetaFromContext(ctx context.Context) (map[string]any, bool) {
	m, ok := ctx.Value(metaKey{}).(map[string]any)
	return m, ok
}

// withMeta puts the request's app _meta keys in ctx, if there are any.
func withMeta(ctx context.Context, meta map[string]any) context.Context {
	var app map[string]any
	for k, v := range meta {
		if convert.ReservedMetaKey(k) {
			continue
		}
		if app == nil {
			app = make(map[string]any, len(meta))
		}
		app[k] = v
	}
	if app == nil {
		return ctx
	}
	return context.WithValue(ctx, metaKey{}, app)
}
