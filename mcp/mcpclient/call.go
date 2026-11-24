package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
	"github.com/SnapdragonPartners/maestro-llms/mcp/internal/convert"
)

// Every Execute error matches exactly one of ErrNotSent, ErrServerError and
// ErrOutcomeUnknown under errors.Is (ADR-0053). They tell a caller whether
// a failed call may be retried safely:
//
//   - ErrNotSent: the request was never handed to the transport, so the
//     server can't have acted on it. A retry is safe (after reconnecting, if
//     the session is closed or its connection has ended).
//   - ErrServerError: the server answered this call with a JSON-RPC error,
//     or with a result the client finds malformed. The error is a
//     *CallError with the details. Asking again would not help.
//   - ErrOutcomeUnknown: the request may have been sent and no answer was
//     matched to it (a timeout or cancellation after the call started, a
//     dropped connection, a failed HTTP request, an expired session).
//     The server may or may not have acted on it.
//
// When the classification can't be certain, the error is ErrOutcomeUnknown,
// never ErrNotSent: a wrong "not sent" would repeat a write. The original
// error stays wrapped, so a context error is still found by errors.Is.
var (
	ErrNotSent        = errors.New("not sent")
	ErrServerError    = errors.New("server error")
	ErrOutcomeUnknown = errors.New("outcome unknown")
)

var (
	errClosed = errors.New("mcpclient: session closed")
	errEnded  = errors.New("mcpclient: the connection to the server has ended")
)

// CallError is a call the server answered with a JSON-RPC error, or with a
// result the client finds malformed (ADR-0053). It matches ErrServerError.
type CallError struct {
	// Err is the underlying error.
	Err error
	// Tool is the server's name for the tool.
	Tool string
	// Message is the JSON-RPC error message, or what was malformed.
	Message string
	// Data is the JSON-RPC error's data member, as the server sent it.
	Data json.RawMessage
	// Code is the JSON-RPC error code (the go-sdk server sends 0 for a tool
	// handler's error); zero when Malformed.
	Code int64
	// Malformed is set when the server returned a result the client can't
	// use rather than a JSON-RPC error.
	Malformed bool
}

func (e *CallError) Error() string {
	if e.Malformed {
		return fmt.Sprintf("mcpclient: call %q: %v: malformed result: %s", e.Tool, ErrServerError, e.Message)
	}
	return fmt.Sprintf("mcpclient: call %q: %v %d: %s", e.Tool, ErrServerError, e.Code, e.Message)
}

func (e *CallError) Unwrap() error { return e.Err }

// Is reports ErrServerError.
func (e *CallError) Is(target error) bool { return target == ErrServerError }

// notSent and unknown build the other two classes, keeping cause wrapped.
func notSent(tool string, cause error) error {
	return fmt.Errorf("mcpclient: call %q: %w: %w", tool, ErrNotSent, cause)
}

func unknown(tool string, cause error) error {
	return fmt.Errorf("mcpclient: call %q: %w: %w", tool, ErrOutcomeUnknown, cause)
}

// executor calls serverName on the session. A transport or protocol failure,
// or a malformed result, is an Execute error (a loop-visible stop) in one of
// the three classes above; an MCP isError result is a model-visible IsError
// result (ADR-0049 §4.6).
//
// Everything that can fail before the request is handed to the SDK is
// checked here first, so ErrNotSent never depends on reading the SDK's
// errors: they can't tell a request that was never written from one that
// was written and not answered (ADR-0053).
func (s *Session) executor(serverName string) func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) {
	return func(ctx context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
		if s.closed.Load() {
			return toolloop.ToolResult{}, notSent(serverName, errClosed)
		}
		if s.ended.Load() {
			return toolloop.ToolResult{}, notSent(serverName, errEnded)
		}
		if err := ctx.Err(); err != nil {
			return toolloop.ToolResult{}, notSent(serverName, err)
		}
		args := call.Parameters
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		if !json.Valid(args) {
			return toolloop.ToolResult{}, notSent(serverName, errors.New("the arguments are not valid JSON"))
		}
		params := &mcp.CallToolParams{Name: serverName, Arguments: args}
		if s.opts.Meta != nil {
			meta, err := s.opts.Meta(ctx, call)
			if err != nil {
				return toolloop.ToolResult{}, notSent(serverName, fmt.Errorf("Options.Meta: %w", err))
			}
			if err := checkMeta(meta); err != nil {
				return toolloop.ToolResult{}, notSent(serverName, err)
			}
			if len(meta) > 0 {
				params.Meta = maps.Clone(meta) // the SDK adds its own keys to the map
			}
		}
		res, err := s.cs.CallTool(ctx, params)
		if err != nil {
			return toolloop.ToolResult{}, classify(serverName, err)
		}
		out, err := convert.Result(res)
		if err != nil {
			// convert's errors already say "malformed result"; Error adds it.
			msg := strings.TrimPrefix(err.Error(), "malformed result: ")
			return toolloop.ToolResult{}, &CallError{Tool: serverName, Message: msg, Malformed: true, Err: err}
		}
		return out, nil
	}
}

// classify sorts an error from the SDK's CallTool into ErrServerError or
// ErrOutcomeUnknown (ADR-0053). Nothing from the SDK is ErrNotSent.
//
// A server error is only a JSON-RPC error that is this call's own matched
// response: the go-sdk (v1.8.0) returns that as the error its call layer
// wraps directly ("calling %q: %w"). Every other shape is unknown,
// including a JSON-RPC error inside an HTTP error response, because the
// SDK also hands that same error to every call still waiting when it fails
// the connection (for example when a stream reconnect gets a 404 for an
// expired session), and those calls may have run. The codes the SDK creates
// itself are excluded even when they arrive in that shape: -32600 (a stdio
// read of a message that is neither request nor response, which is handed
// to every waiting call), -32700, and -32003 to -32005 (its "closing" and
// "rejected by transport" sentinels).
func classify(tool string, err error) error {
	var we *jsonrpc.Error
	// Deliberately not errors.As: only the directly wrapped error is this
	// call's own response.
	if direct, ok := errors.Unwrap(err).(*jsonrpc.Error); ok && direct != nil { //nolint:errorlint // see above
		we = direct
	}
	if we != nil && !sdkLocalCode(we.Code) {
		return &CallError{Tool: tool, Code: we.Code, Message: we.Message, Data: we.Data, Err: err}
	}
	return unknown(tool, err)
}

func sdkLocalCode(code int64) bool {
	switch code {
	case jsonrpc.CodeParseError, jsonrpc.CodeInvalidRequest, -32003, -32004, -32005:
		return true
	}
	return false
}

// checkMeta validates the app's _meta keys against the MCP key format and
// the reservations, and checks that the values marshal to JSON.
func checkMeta(meta map[string]any) error {
	for k := range meta {
		if err := checkMetaKey(k); err != nil {
			return fmt.Errorf("Options.Meta: key %q: %w", k, err)
		}
	}
	if len(meta) == 0 {
		return nil
	}
	if _, err := json.Marshal(meta); err != nil {
		return fmt.Errorf("Options.Meta: %w", err)
	}
	return nil
}

// checkMetaKey applies MCP's _meta key rule: an optional prefix of
// dot-separated labels followed by a slash, then a name. A label starts with
// a letter, ends with a letter or digit, and has letters, digits or hyphens
// inside. A name starts and ends with a letter or digit and has letters,
// digits, hyphens, underscores or dots inside. A prefix with a label
// "modelcontextprotocol" or "mcp" is reserved for MCP (the go-sdk's own keys
// are io.modelcontextprotocol/...), and progressToken is a field MCP defines.
func checkMetaKey(k string) error {
	prefix, name, hasPrefix := strings.Cut(k, "/")
	if !hasPrefix {
		prefix, name = "", k
	}
	if hasPrefix {
		if prefix == "" {
			return errors.New("empty prefix")
		}
		for label := range strings.SplitSeq(prefix, ".") {
			if !validLabel(label) {
				return fmt.Errorf("invalid prefix label %q", label)
			}
			if l := strings.ToLower(label); l == "modelcontextprotocol" || l == "mcp" {
				return errors.New("the prefix is reserved for MCP")
			}
		}
	}
	if !validName(name) {
		return errors.New("invalid name")
	}
	if !hasPrefix && name == "progressToken" {
		return errors.New("progressToken is defined by MCP")
	}
	return nil
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlnum(c byte) bool { return isAlpha(c) || isDigit(c) }

func validLabel(l string) bool {
	if l == "" || !isAlpha(l[0]) || !isAlnum(l[len(l)-1]) {
		return false
	}
	for i := range len(l) {
		if c := l[i]; !isAlnum(c) && c != '-' {
			return false
		}
	}
	return true
}

func validName(n string) bool {
	if n == "" || !isAlnum(n[0]) || !isAlnum(n[len(n)-1]) {
		return false
	}
	for i := range len(n) {
		if c := n[i]; !isAlnum(c) && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}
