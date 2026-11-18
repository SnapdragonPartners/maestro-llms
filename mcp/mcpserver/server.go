// Package mcpserver serves toolloop.Tool values to MCP clients such as
// ChatGPT (ADR-0050): over Streamable HTTP as an http.Handler, optionally
// behind OAuth, and over stdio.
//
// With Options.Auth the HTTP endpoint is an OAuth resource server: it checks
// bearer tokens through the application's TokenVerifier, enforces audience,
// expiry and scopes itself, and publishes protected resource metadata
// (RFC 9728). It issues no tokens and registers no clients; the
// authorization server is the application's or its identity provider's.
//
// The module is v0 and outside the root module's v1 stability contract; no
// go-sdk type appears in its API.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
	"github.com/SnapdragonPartners/maestro-llms/mcp/internal/convert"
)

// defaultName is the server's name in initialize when Options.Name is empty.
const defaultName = "maestro-llms-mcp"

const modulePath = "github.com/SnapdragonPartners/maestro-llms/mcp"

// Options configure a server. The zero value serves the tools over HTTP
// without auth.
type Options struct {
	// Annotations holds behavior hints by tool name. A name that isn't one
	// of the tools is an error from New.
	Annotations map[string]Annotations
	// Auth protects the HTTP endpoint. Nil serves it without auth.
	Auth *Auth
	// OnError receives errors the client is not shown in full: an Execute
	// error, an invalid ToolResult, and a verifier's internal error. It may
	// be called concurrently. Optional.
	OnError func(ctx context.Context, err error)
	// Name and Version identify the server in initialize. An empty Name is
	// "maestro-llms-mcp"; an empty Version is this module's version.
	Name, Version string
	// Instructions is sent in the initialize result. Optional.
	Instructions string
}

// Annotations are a tool's behavior hints for clients, which may use them
// to decide, for example, whether to ask the user before a call. Nil keeps
// MCP's default: false for ReadOnly and Idempotent, true for Destructive
// and OpenWorld.
type Annotations struct {
	ReadOnly    *bool
	Destructive *bool
	Idempotent  *bool
	OpenWorld   *bool
	Title       string
}

// InvalidToolError reports a tool New can't serve: its name breaks the
// portable rule or repeats another tool's, its Execute is nil, or its input
// schema is not an object schema the MCP SDK accepts.
type InvalidToolError struct {
	Err  error
	Tool string
}

func (e *InvalidToolError) Error() string {
	return fmt.Sprintf("mcpserver: tool %q: %v", e.Tool, e.Err)
}

func (e *InvalidToolError) Unwrap() error { return e.Err }

// Server serves a fixed set of tools. It is safe for concurrent use.
type Server struct {
	srv      *mcp.Server
	auth     *authConfig // nil without Auth
	onError  func(context.Context, error)
	metaPath string
}

// New checks every tool and builds a server. It returns an error joining one
// *InvalidToolError per bad tool (use errors.As), or an error for invalid
// Options. A valid tool has a name matching ^[a-zA-Z_][a-zA-Z0-9_-]{0,63}$
// that no other tool has, a non-nil Execute, and an input schema that is
// empty (served as {"type":"object"}) or a JSON object with top-level type
// "object" that the MCP SDK registers without complaint.
func New(tools []toolloop.Tool, opts Options) (*Server, error) {
	s := &Server{onError: opts.OnError}
	if opts.Auth != nil {
		ac, err := newAuthConfig(opts.Auth)
		if err != nil {
			return nil, err
		}
		s.auth = ac
		s.metaPath = ac.metadataPath
	}

	names := make(map[string]int, len(tools))
	for i := range tools {
		names[tools[i].Definition.Name]++
	}
	for name := range opts.Annotations {
		if names[name] == 0 {
			return nil, fmt.Errorf("mcpserver: Options.Annotations names %q, which is not one of the tools", name)
		}
	}

	version := opts.Version
	if version == "" {
		version = moduleVersion()
	}
	name := opts.Name
	if name == "" {
		name = defaultName
	}
	impl := &mcp.Implementation{Name: name, Version: version}
	s.srv = mcp.NewServer(impl, &mcp.ServerOptions{Instructions: opts.Instructions, Logger: discardLogger()})

	var errs []error
	for i := range tools {
		t := &tools[i]
		mt, invalid := mcpTool(t, names, opts.Annotations)
		if invalid == nil {
			if err := registers(impl, mt); err != nil {
				invalid = &InvalidToolError{Tool: t.Definition.Name, Err: err}
			}
		}
		if invalid != nil {
			errs = append(errs, invalid)
			continue
		}
		s.srv.AddTool(mt, s.handler(t))
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("mcpserver: %d of the tools can't be served: %w", len(errs), errors.Join(errs...))
	}
	return s, nil
}

// mcpTool checks t and builds its MCP definition.
func mcpTool(t *toolloop.Tool, names map[string]int, ann map[string]Annotations) (*mcp.Tool, *InvalidToolError) {
	name := t.Definition.Name
	invalid := func(err error) *InvalidToolError { return &InvalidToolError{Tool: name, Err: err} }
	if err := convert.CheckName(name); err != nil {
		return nil, invalid(err)
	}
	if n := names[name]; n > 1 {
		return nil, invalid(fmt.Errorf("name is shared by %d tools", n))
	}
	if t.Execute == nil {
		return nil, invalid(errors.New("nil Execute"))
	}
	schema, err := convert.ServerSchema(t.Definition.InputSchema)
	if err != nil {
		return nil, invalid(err)
	}
	mt := &mcp.Tool{Name: name, Description: t.Definition.Description, InputSchema: schema}
	if a, ok := ann[name]; ok {
		mt.Annotations = &mcp.ToolAnnotations{
			DestructiveHint: a.Destructive,
			OpenWorldHint:   a.OpenWorld,
			Title:           a.Title,
		}
		if a.ReadOnly != nil {
			mt.Annotations.ReadOnlyHint = *a.ReadOnly
		}
		if a.Idempotent != nil {
			mt.Annotations.IdempotentHint = *a.Idempotent
		}
	}
	return mt, nil
}

// registers reports whether the MCP SDK accepts mt, by adding it to a
// throwaway server. The SDK's AddTool panics on a schema it rejects (one
// that doesn't decode into a map, an invalid x-mcp-header annotation, and
// whatever a later release adds); this is the one place such a panic
// becomes an error, so New never panics on a tool (ADR-0050 §2).
func registers(impl *mcp.Implementation, mt *mcp.Tool) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("the MCP SDK rejects it: %v", r)
		}
	}()
	probe := mcp.NewServer(impl, &mcp.ServerOptions{Logger: discardLogger()})
	probe.AddTool(mt, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, errors.New("probe")
	})
	return nil
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// handler runs t for a tools/call request (ADR-0050 §3-§5).
func (s *Server) handler(t *toolloop.Tool) mcp.ToolHandler {
	name := t.Definition.Name
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := arguments(req.Params.Arguments)
		if err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("tool %q: %v", name, err)}
		}
		if req.Extra != nil {
			if info, ok := infoFromSDK(req.Extra.TokenInfo); ok {
				ctx = context.WithValue(ctx, tokenInfoKey{}, info)
			}
		}
		res, err := t.Execute(ctx, llms.ToolCall{Name: name, Parameters: args})
		if err != nil {
			return nil, s.failed(ctx, name, fmt.Errorf("mcpserver: tool %q: %w", name, err))
		}
		out, err := convert.ToMCP(&res)
		if err != nil {
			return nil, s.failed(ctx, name, fmt.Errorf("mcpserver: tool %q: %w", name, err))
		}
		return out, nil
	}
}

// arguments returns a call's arguments as a JSON object: {} when absent or
// null, an error when they are anything but an object.
func arguments(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("{}"), nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("arguments are not valid JSON: %w", err)
	}
	switch v.(type) {
	case nil:
		return json.RawMessage("{}"), nil
	case map[string]any:
		return raw, nil
	default:
		return nil, errors.New("arguments must be a JSON object")
	}
}

// failed reports err to OnError and returns the fixed-message JSON-RPC error
// the client sees: the error's text may name internal hosts or paths, and
// the client can't act on it (ADR-0050 §5).
func (s *Server) failed(ctx context.Context, name string, err error) error {
	s.report(ctx, err)
	return &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: fmt.Sprintf("tool %q failed", name)}
}

func (s *Server) report(ctx context.Context, err error) {
	if s.onError != nil {
		s.onError(ctx, err)
	}
}

// Handler is the Streamable HTTP endpoint (stateless, JSON responses),
// behind the bearer-token checks when Options.Auth is set. The app mounts
// it at the path of Auth.Resource.
func (s *Server) Handler() http.Handler {
	h := http.Handler(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s.srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	))
	if s.auth != nil {
		h = s.requireToken(h)
	}
	return h
}

// ServeStdio serves over the process's stdin and stdout until ctx ends or
// the client closes the connection. Options.Auth does not apply: MCP has a
// stdio server take its credentials from its environment.
func (s *Server) ServeStdio(ctx context.Context) error {
	return s.serve(ctx, &mcp.StdioTransport{})
}

func (s *Server) serve(ctx context.Context, t mcp.Transport) error {
	if err := s.srv.Run(ctx, t); err != nil {
		return fmt.Errorf("mcpserver: %w", err)
	}
	return nil
}

// moduleVersion is this module's version from the build info.
func moduleVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		if bi.Main.Path == modulePath && bi.Main.Version != "" {
			return bi.Main.Version
		}
		for _, d := range bi.Deps {
			if d.Path == modulePath {
				return d.Version
			}
		}
	}
	return "(devel)"
}
