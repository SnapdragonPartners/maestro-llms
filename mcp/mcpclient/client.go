// Package mcpclient turns the tools of an MCP server into toolloop.Tool
// values (ADR-0049): connect to a server, list its tools, and run each one
// through the tool loop beside in-process Go tools.
//
// The package sources no credentials: an HTTP server's credentials travel in
// the caller's http.Client, and a local server's environment is set on the
// caller's exec.Cmd. Grants, approval policy and namespacing across servers
// belong to the application, which can wrap the returned tools.
//
// The module is v0 and outside the root module's v1 stability contract; no
// go-sdk type appears in its API.
package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
	"github.com/SnapdragonPartners/maestro-llms/mcp/internal/convert"
)

// implementationName is how the client identifies itself in initialize.
const implementationName = "maestro-llms-mcp"

const modulePath = "github.com/SnapdragonPartners/maestro-llms/mcp"

// Transport says how to reach a server. It is opaque: build one with Command
// or StreamableHTTP. The zero Transport is invalid.
type Transport struct {
	t mcp.Transport
}

// Command runs cmd as a local server and talks to it over stdin/stdout. The
// session owns the process: Connect starts it and Session.Close stops it
// (closing its stdin, then signalling it if it doesn't exit). cmd must not
// be nil (Connect rejects it) or already started, and its Stdin and Stdout
// must be unset.
func Command(cmd *exec.Cmd) Transport {
	if cmd == nil {
		return Transport{}
	}
	return Transport{t: &mcp.CommandTransport{Command: cmd}}
}

// StreamableHTTP connects to an MCP Streamable HTTP endpoint. client carries
// any credentials, for example through its Transport; the package adds none.
// A nil client uses http.DefaultClient.
func StreamableHTTP(endpoint string, client *http.Client) Transport {
	return Transport{t: &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: client}}
}

// Options configure a session. The zero value keeps every tool under the
// server's name.
type Options struct {
	// Name maps a server's tool name to the name offered to the model. Nil
	// keeps the server's name. Execute always calls the server's name. The
	// mapped name must still satisfy the portable rule (see Session.Tools).
	Name func(serverName string) string
	// Filter selects tools by the server's name. Nil keeps every tool.
	Filter func(serverName string) bool
}

// Session is one connection to an MCP server. It is safe for concurrent use:
// tools from one session may run at once, and Tools may be called while they
// do (concurrent Tools calls run one at a time). The caller owns Close.
type Session struct {
	cs    *mcp.ClientSession
	pages map[string]rawPage // tools/list pages as the server sent them, by cursor
	opts  Options
	mu    sync.Mutex // guards pages
	// listMu serializes Tools calls, so each page's recorded raw listing is
	// the one ListTools just returned (or, for a page the go-sdk serves from
	// its cache, the fetch that filled that cache entry). Tool calls don't
	// take it.
	listMu sync.Mutex
	closed atomic.Bool
}

// rawPage is what the server sent for one tools/list page, before the
// go-sdk drops entries it considers invalid (null entries, and tools with
// invalid x-mcp-header annotations), which it only logs.
type rawPage struct {
	names []string
	nulls int
}

// Connect opens a session: it starts or dials the server and runs the MCP
// initialize handshake under ctx.
func Connect(ctx context.Context, t Transport, opts Options) (*Session, error) {
	if t.t == nil {
		return nil, errors.New("mcpclient: zero Transport (or Command(nil)); use Command with a non-nil exec.Cmd, or StreamableHTTP")
	}
	s := &Session{opts: opts, pages: map[string]rawPage{}}
	client := mcp.NewClient(&mcp.Implementation{Name: implementationName, Version: moduleVersion()}, nil)
	client.AddSendingMiddleware(s.recordListing)
	cs, err := client.Connect(ctx, t.t, nil)
	if err != nil {
		return nil, fmt.Errorf("mcpclient: connect: %w", err)
	}
	s.cs = cs
	return s, nil
}

// recordListing keeps each tools/list page as the server sent it, so Tools
// can report the tools the go-sdk drops from ListTools' result instead of
// silently offering the rest (ADR-0049 §4.5). It runs before that filter.
// A page the go-sdk serves from its cache doesn't pass through here, and
// the page recorded when it was fetched still describes it.
func (s *Session) recordListing(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		if err != nil || method != "tools/list" {
			return res, err
		}
		lr, ok := res.(*mcp.ListToolsResult)
		if !ok || lr == nil {
			return res, err
		}
		cursor := ""
		if p, ok := req.GetParams().(*mcp.ListToolsParams); ok && p != nil {
			cursor = p.Cursor
		}
		var page rawPage
		for _, t := range lr.Tools {
			if t == nil {
				page.nulls++
				continue
			}
			page.names = append(page.names, t.Name)
		}
		s.mu.Lock()
		s.pages[cursor] = page
		s.mu.Unlock()
		return res, err
	}
}

// excluded returns the names the go-sdk dropped from the page at cursor,
// given the tools it kept, and the number of null entries.
func (s *Session) excluded(cursor string, kept []*mcp.Tool) (names []string, nulls int) {
	s.mu.Lock()
	page, ok := s.pages[cursor]
	s.mu.Unlock()
	if !ok {
		return nil, 0
	}
	left := make(map[string]int, len(kept))
	for _, t := range kept {
		left[t.Name]++
	}
	for _, n := range page.names {
		if left[n] > 0 {
			left[n]--
			continue
		}
		names = append(names, n)
	}
	return names, page.nulls
}

// Close ends the session and stops a Command server. Execute on a tool from
// a closed session returns an error. Close is idempotent.
func (s *Session) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	if err := s.cs.Close(); err != nil {
		return fmt.Errorf("mcpclient: close: %w", err)
	}
	return nil
}

// InvalidToolError reports a server tool that can't be offered to a model:
// its (mapped) name breaks the portable rule or collides with another of the
// server's tools, its input schema can't be normalized, or the MCP SDK
// excluded it from the listing as invalid.
type InvalidToolError struct {
	Err  error
	Tool string // the server's name
}

func (e *InvalidToolError) Error() string {
	return fmt.Sprintf("mcpclient: tool %q: %v", e.Tool, e.Err)
}

func (e *InvalidToolError) Unwrap() error { return e.Err }

// Tools lists the server's tools (every page) and returns them as
// toolloop.Tool values in the server's order. It is a snapshot: a caller
// that wants a later change calls Tools again.
//
// Each tool that passes Options.Filter must be valid, or Tools returns no
// tools and an error joining one *InvalidToolError per bad tool (use
// errors.As, or filter the named tools out and call again). A valid tool
// has a name, after Options.Name, matching ^[a-zA-Z_][a-zA-Z0-9_-]{0,63}$
// (accepted by every provider) and unique within the server, and an input
// schema whose type is "object" and whose references are local and
// non-recursive; those references are inlined, since Gemini rejects them.
// A tool the MCP SDK itself drops from the listing (for example for an
// invalid x-mcp-header annotation) is an *InvalidToolError too, and a null
// entry in the listing fails Tools outright, since it has no name to filter.
func (s *Session) Tools(ctx context.Context) ([]toolloop.Tool, error) {
	if s.closed.Load() {
		return nil, errClosed
	}
	var ( //nolint:prealloc // the count is unknown until every page is listed
		tools   []toolloop.Tool
		servers []string // server name of tools[i]
		errs    []error
	)
	s.listMu.Lock()
	defer s.listMu.Unlock()
	keep := func(name string) bool { return s.opts.Filter == nil || s.opts.Filter(name) }
	seen := map[string]bool{}
	for cursor := ""; ; {
		res, err := s.cs.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("mcpclient: list tools: %w", err)
		}
		dropped, nulls := s.excluded(cursor, res.Tools)
		if nulls > 0 {
			errs = append(errs, fmt.Errorf("mcpclient: the server's tool list has %d null entries", nulls))
		}
		for _, name := range dropped {
			if keep(name) {
				errs = append(errs, &InvalidToolError{Tool: name, Err: errors.New("the MCP SDK excluded it from the listing as invalid (for example an invalid x-mcp-header annotation)")})
			}
		}
		for _, t := range res.Tools {
			if !keep(t.Name) {
				continue
			}
			def, invalid := s.definition(t)
			if invalid != nil {
				errs = append(errs, invalid)
				continue
			}
			tools = append(tools, toolloop.Tool{Definition: def, Execute: s.executor(t.Name)})
			servers = append(servers, t.Name)
		}
		if res.NextCursor == "" {
			break
		}
		seen[cursor] = true
		if seen[res.NextCursor] {
			return nil, fmt.Errorf("mcpclient: list tools: the server repeated cursor %q", res.NextCursor)
		}
		cursor = res.NextCursor
	}
	errs = append(errs, duplicates(tools, servers)...)
	if len(errs) > 0 {
		return nil, fmt.Errorf("mcpclient: %d of the server's tools can't be offered: %w", len(errs), errors.Join(errs...))
	}
	return tools, nil
}

func (s *Session) definition(t *mcp.Tool) (llms.ToolDefinition, *InvalidToolError) {
	name := t.Name
	if s.opts.Name != nil {
		name = s.opts.Name(t.Name)
	}
	if err := convert.CheckName(name); err != nil {
		return llms.ToolDefinition{}, &InvalidToolError{Tool: t.Name, Err: err}
	}
	schema, err := convert.Schema(t.InputSchema)
	if err != nil {
		return llms.ToolDefinition{}, &InvalidToolError{Tool: t.Name, Err: err}
	}
	return llms.ToolDefinition{Name: name, Description: convert.Description(t), InputSchema: schema}, nil
}

// duplicates reports every tool whose offered name another tool shares.
func duplicates(tools []toolloop.Tool, servers []string) []error {
	byName := make(map[string][]int, len(tools))
	for i := range tools {
		n := tools[i].Definition.Name
		byName[n] = append(byName[n], i)
	}
	var errs []error
	for i := range tools {
		n := tools[i].Definition.Name
		if len(byName[n]) > 1 {
			errs = append(errs, &InvalidToolError{
				Tool: servers[i],
				Err:  fmt.Errorf("offered name %q is shared by %d of the server's tools", n, len(byName[n])),
			})
		}
	}
	return errs
}

var errClosed = errors.New("mcpclient: session closed")

// executor calls serverName on the session. A transport or protocol failure,
// or a malformed result, is an Execute error (a loop-visible stop); an MCP
// isError result is a model-visible IsError result (ADR-0049 §4.6).
func (s *Session) executor(serverName string) func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) {
	return func(ctx context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
		if s.closed.Load() {
			return toolloop.ToolResult{}, fmt.Errorf("mcpclient: call %q: %w", serverName, errClosed)
		}
		args := call.Parameters
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		res, err := s.cs.CallTool(ctx, &mcp.CallToolParams{Name: serverName, Arguments: args})
		if err != nil {
			return toolloop.ToolResult{}, fmt.Errorf("mcpclient: call %q: %w", serverName, err)
		}
		out, err := convert.Result(res)
		if err != nil {
			return toolloop.ToolResult{}, fmt.Errorf("mcpclient: call %q: %w", serverName, err)
		}
		return out, nil
	}
}

// moduleVersion is this module's version from the build info, for the
// initialize handshake.
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
