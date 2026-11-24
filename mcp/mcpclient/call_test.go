package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

// class returns the one class err belongs to, failing the test unless
// exactly one of the three sentinels matches (ADR-0053).
func class(t *testing.T, err error) error {
	t.Helper()
	if err == nil {
		t.Fatal("want an Execute error, got nil")
	}
	var found []error
	for _, c := range []error{ErrNotSent, ErrServerError, ErrOutcomeUnknown} {
		if errors.Is(err, c) {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("error %q matches %d classes %v, want exactly one", err, len(found), found)
	}
	var ce *CallError
	if errors.Is(found[0], ErrServerError) != errors.As(err, &ce) {
		t.Fatalf("error %q: ErrServerError iff *CallError", err)
	}
	return found[0]
}

func wantClass(t *testing.T, err, want error) {
	t.Helper()
	if got := class(t, err); !errors.Is(got, want) {
		t.Fatalf("error %q is %v, want %v", err, got, want)
	}
}

// counting returns a server tool "count" that counts its calls.
func counting(n *atomic.Int32) func(*mcp.Server) {
	return func(s *mcp.Server) {
		s.AddTool(&mcp.Tool{Name: "count", InputSchema: raw(`{"type":"object"}`)},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				n.Add(1)
				return textResult("counted"), nil
			})
	}
}

// metaTool is a server tool that returns the request's _meta as JSON.
func metaTool(s *mcp.Server) {
	s.AddTool(&mcp.Tool{Name: "meta", InputSchema: raw(`{"type":"object"}`)},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			b, err := json.Marshal(req.Params.Meta)
			if err != nil {
				return nil, err
			}
			return textResult(string(b)), nil
		})
}

type ctxKey struct{}

func TestMetaSent(t *testing.T) {
	shared := map[string]any{"com.example/static": "s"}
	var gotCall llms.ToolCall
	sess := connect(t, newServer(nil, metaTool), Options{
		Meta: func(ctx context.Context, call llms.ToolCall) (map[string]any, error) {
			gotCall = call
			m := shared // reused across calls: Execute must not let the SDK write into it
			m["com.example/run"] = ctx.Value(ctxKey{})
			return m, nil
		},
	})
	tools, err := sess.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), ctxKey{}, "run-7")
	call := llms.ToolCall{ID: "c1", Name: "meta", Parameters: raw(`{"x":1}`)}
	res, err := byName(t, tools, "meta").Execute(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(res.Content), &meta); err != nil {
		t.Fatalf("meta %q: %v", res.Content, err)
	}
	if meta["com.example/run"] != "run-7" || meta["com.example/static"] != "s" {
		t.Errorf("server got _meta %v", meta)
	}
	if gotCall.ID != "c1" || string(gotCall.Parameters) != `{"x":1}` {
		t.Errorf("Meta got call %+v", gotCall)
	}
	if len(shared) != 2 {
		t.Errorf("the app's map was modified: %v", shared)
	}
}

func TestMetaNilSendsNone(t *testing.T) {
	for name, opts := range map[string]Options{
		"no Meta": {},
		"nil map": {Meta: func(context.Context, llms.ToolCall) (map[string]any, error) {
			var none map[string]any
			return none, nil
		}},
		"empty map": {Meta: func(context.Context, llms.ToolCall) (map[string]any, error) { return map[string]any{}, nil }},
	} {
		t.Run(name, func(t *testing.T) {
			sess := connect(t, newServer(nil, metaTool), opts)
			tools, err := sess.Tools(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			res, err := byName(t, tools, "meta").Execute(context.Background(), llms.ToolCall{ID: "1", Name: "meta"})
			if err != nil {
				t.Fatal(err)
			}
			var meta map[string]any
			_ = json.Unmarshal([]byte(res.Content), &meta)
			for k := range meta {
				if !strings.HasPrefix(k, "io.modelcontextprotocol/") {
					t.Errorf("unexpected _meta key %q (all: %s)", k, res.Content)
				}
			}
		})
	}
}

// TestMetaRefused: a Meta error or an invalid map fails the call as
// ErrNotSent and the server never sees it.
func TestMetaRefused(t *testing.T) {
	boom := errors.New("no run in context")
	cases := map[string]struct {
		meta map[string]any
		err  error
	}{
		"error":         {err: boom},
		"reserved key":  {meta: map[string]any{"io.modelcontextprotocol/protocolVersion": "x"}},
		"progressToken": {meta: map[string]any{"progressToken": 1}},
		"invalid key":   {meta: map[string]any{"bad key": 1}},
		"unmarshalable": {meta: map[string]any{"ok": make(chan int)}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var n atomic.Int32
			meta := func(context.Context, llms.ToolCall) (map[string]any, error) { return tc.meta, tc.err }
			sess := connect(t, newServer(nil, counting(&n)), Options{Meta: meta})
			tools, err := sess.Tools(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			_, err = byName(t, tools, "count").Execute(context.Background(), llms.ToolCall{ID: "1", Name: "count"})
			wantClass(t, err, ErrNotSent)
			if name == "error" && !errors.Is(err, boom) {
				t.Errorf("error %q does not wrap Meta's error", err)
			}
			if n.Load() != 0 {
				t.Errorf("the server ran the tool %d times", n.Load())
			}
		})
	}
}

func TestCheckMetaKey(t *testing.T) {
	valid := []string{
		"run", "run_id", "a", "x.y-z_1", "com.example/run", "example/x", "a-b.c1/n",
		"com.example.mcpish/x", "mcpx/y", "progressToken2", "app/progressToken",
	}
	invalid := []string{
		"", "/x", "com.example/", "-a", "a-", "_a", "a b", "a/b/c",
		"1com/x", "com-/x", "com..example/x",
		"io.modelcontextprotocol/protocolVersion", "modelcontextprotocol.io/x",
		"mcp/x", "mcp.dev/x", "tools.mcp.com/x", "api.MCP.org/x",
		"progressToken",
	}
	for _, k := range valid {
		if err := checkMetaKey(k); err != nil {
			t.Errorf("checkMetaKey(%q) = %v, want valid", k, err)
		}
	}
	for _, k := range invalid {
		if err := checkMetaKey(k); err == nil {
			t.Errorf("checkMetaKey(%q) = nil, want an error", k)
		}
	}
}

func TestNotSentBeforeTheSDK(t *testing.T) {
	var n atomic.Int32
	open := func(t *testing.T) (*Session, toolloop.Tool) {
		t.Helper()
		sess := connect(t, newServer(nil, counting(&n)), Options{})
		tools, err := sess.Tools(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return sess, byName(t, tools, "count")
	}
	call := llms.ToolCall{ID: "1", Name: "count"}

	t.Run("closed session", func(t *testing.T) {
		sess, tool := open(t)
		_ = sess.Close()
		_, err := tool.Execute(context.Background(), call)
		wantClass(t, err, ErrNotSent)
		if !errors.Is(err, errClosed) {
			t.Errorf("error %q", err)
		}
	})
	t.Run("done context", func(t *testing.T) {
		_, tool := open(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := tool.Execute(ctx, call)
		wantClass(t, err, ErrNotSent)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error %q does not wrap context.Canceled", err)
		}
	})
	t.Run("invalid arguments", func(t *testing.T) {
		_, tool := open(t)
		_, err := tool.Execute(context.Background(), llms.ToolCall{ID: "1", Name: "count", Parameters: raw(`{"a":`)})
		wantClass(t, err, ErrNotSent)
	})
	if n.Load() != 0 {
		t.Errorf("the server ran the tool %d times", n.Load())
	}
}

// waitEnded waits for the session's connection watcher to see the end.
func waitEnded(t *testing.T, sess *Session) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !sess.ended.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the session never saw its connection end")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestNotSentAfterConnectionEnded(t *testing.T) {
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := newServer(nil).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := Connect(ctx, Transport{t: ct}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	tools, err := sess.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = ss.Close() // the server goes away
	waitEnded(t, sess)
	_, err = byName(t, tools, "echo").Execute(ctx, llms.ToolCall{ID: "1", Name: "echo", Parameters: raw(`{"msg":"x"}`)})
	wantClass(t, err, ErrNotSent)
	if !errors.Is(err, errEnded) {
		t.Errorf("error %q", err)
	}
}

func TestServerErrors(t *testing.T) {
	s := newServer(nil)
	sess := connect(t, s, Options{})
	tools, err := sess.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	t.Run("handler error", func(t *testing.T) {
		_, err := byName(t, tools, "protocol_error").Execute(ctx, llms.ToolCall{ID: "1", Name: "protocol_error"})
		wantClass(t, err, ErrServerError)
		var ce *CallError
		errors.As(err, &ce)
		if ce.Tool != "protocol_error" || !strings.Contains(ce.Message, "handler exploded") || ce.Malformed {
			t.Errorf("CallError = %+v", ce)
		}
	})
	t.Run("tool removed from the server", func(t *testing.T) {
		s.RemoveTools("echo")
		_, err := byName(t, tools, "echo").Execute(ctx, llms.ToolCall{ID: "1", Name: "echo", Parameters: raw(`{"msg":"x"}`)})
		wantClass(t, err, ErrServerError)
	})
}

func rawCallTool(t *testing.T, call func(json.RawMessage) string) toolloop.Tool {
	t.Helper()
	ctx := context.Background()
	sess, err := Connect(ctx, rawServerCalls(ctx, t, pagesOf(`{"tools":[{"name":"t","inputSchema":{"type":"object"}}]}`), call), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	tools, err := sess.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return byName(t, tools, "t")
}

func TestRawServerReplies(t *testing.T) {
	cases := []struct {
		want   error
		check  func(t *testing.T, ce *CallError)
		name   string
		member string
	}{
		{name: "custom error code", member: `"error":{"code":-32050,"message":"quota","data":{"retry":false}}`, want: ErrServerError,
			check: func(t *testing.T, ce *CallError) {
				if ce.Code != -32050 || ce.Message != "quota" || string(ce.Data) != `{"retry":false}` {
					t.Errorf("CallError = %+v", ce)
				}
			}},
		{name: "invalid params", member: `"error":{"code":-32602,"message":"bad"}`, want: ErrServerError},
		{name: "malformed result", member: `"result":{"content":[{"type":"image","data":""}]}`, want: ErrServerError,
			check: func(t *testing.T, ce *CallError) {
				if !ce.Malformed || ce.Code != 0 {
					t.Errorf("CallError = %+v", ce)
				}
				if msg := ce.Error(); strings.Count(msg, "malformed result") != 1 || strings.HasPrefix(ce.Message, "malformed result") {
					t.Errorf("Error() = %q, Message = %q: want one \"malformed result\", in Error only", msg, ce.Message)
				}
			}},
		// Codes the go-sdk also creates itself are never trusted as the
		// server's answer.
		{name: "code -32005 rejected", member: `"error":{"code":-32005,"message":"rejected by transport"}`, want: ErrOutcomeUnknown},
		{name: "code -32600 invalid request", member: `"error":{"code":-32600,"message":"invalid request"}`, want: ErrOutcomeUnknown},
		{name: "code -32700 parse error", member: `"error":{"code":-32700,"message":"parse error"}`, want: ErrOutcomeUnknown},
		{name: "code -32004 server closing", member: `"error":{"code":-32004,"message":"server is closing"}`, want: ErrOutcomeUnknown},
		{name: "code -32003 client closing", member: `"error":{"code":-32003,"message":"client is closing"}`, want: ErrOutcomeUnknown},
		{name: "connection dropped mid-call", member: rawCloseConn, want: ErrOutcomeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := rawCallTool(t, func(json.RawMessage) string { return tc.member })
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := tool.Execute(ctx, llms.ToolCall{ID: "1", Name: "t"})
			wantClass(t, err, tc.want)
			if tc.check != nil {
				var ce *CallError
				errors.As(err, &ce)
				tc.check(t, ce)
			}
		})
	}
}

func TestUnknownWhenNoAnswer(t *testing.T) {
	tool := rawCallTool(t, func(json.RawMessage) string { return "" }) // never answers
	for name, mk := range map[string]func() (context.Context, context.CancelFunc){
		"deadline": func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 50*time.Millisecond)
		},
		"cancel": func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(50*time.Millisecond, cancel)
			return ctx, cancel
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := mk()
			defer cancel()
			_, err := tool.Execute(ctx, llms.ToolCall{ID: "1", Name: "t"})
			wantClass(t, err, ErrOutcomeUnknown)
			if !errors.Is(err, ctx.Err()) {
				t.Errorf("error %q does not wrap %v", err, ctx.Err())
			}
		})
	}
}

// TestUnknownThroughToolLoop: an unknown outcome under a canceled loop still
// stops the run as OutcomeCanceled (classifyStop finds the context error).
func TestUnknownThroughToolLoop(t *testing.T) {
	tool := rawCallTool(t, func(json.RawMessage) string { return "" })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	out := toolloop.Run(ctx, toolloop.Config{
		Client:  &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.ToolCallResponse("1", "t", `{}`)}},
		Request: llms.ChatRequest{Messages: []llms.Message{llms.UserText("go")}},
		Tools:   []toolloop.Tool{tool},
	})
	if out.Kind != toolloop.OutcomeCanceled || !errors.Is(out.Err, ErrOutcomeUnknown) {
		t.Fatalf("outcome %v, err %v", out.Kind, out.Err)
	}
}

// TestClassifyShapes pins the go-sdk error shapes classify relies on.
func TestClassifyShapes(t *testing.T) {
	direct := func(code int64) error {
		return fmt.Errorf("calling %q: %w", "tools/call", &jsonrpc.Error{Code: code, Message: "m"})
	}
	cases := []struct {
		err  error
		want error
		name string
	}{
		{name: "direct server error", err: direct(-32603), want: ErrServerError},
		{name: "direct custom code", err: direct(7), want: ErrServerError},
		{name: "direct sdk-local -32600", err: direct(jsonrpc.CodeInvalidRequest), want: ErrOutcomeUnknown},
		{name: "direct sdk-local -32005", err: direct(-32005), want: ErrOutcomeUnknown},
		// A JSON-RPC error in an HTTP error response: the SDK hands the
		// same error to every waiting call when it fails the connection.
		{name: "inside an HTTP error", err: fmt.Errorf("calling %q: %w", "tools/call",
			fmt.Errorf("sending %q: %w: %w: %v", "tools/call", &jsonrpc.Error{Code: -32602, Message: "m"}, &jsonrpc.Error{Code: -32005}, "Bad Request")), want: ErrOutcomeUnknown},
		{name: "connection closed", err: fmt.Errorf("%w: calling %q: %v", mcp.ErrConnectionClosed, "tools/call", "server is closing"), want: ErrOutcomeUnknown},
		{name: "session missing", err: fmt.Errorf("calling %q: %w", "tools/call", fmt.Errorf("x: %w", mcp.ErrSessionMissing)), want: ErrOutcomeUnknown},
		{name: "plain", err: errors.New("boom"), want: ErrOutcomeUnknown},
		{name: "context", err: context.DeadlineExceeded, want: ErrOutcomeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantClass(t, classify("t", tc.err), tc.want)
		})
	}
}

// httpServer serves newServer over Streamable HTTP; fail makes every
// request after initialization fail with status (and body, if set).
func httpServer(t *testing.T) (ts *httptest.Server, fail func(status int, body string)) {
	t.Helper()
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return newServer(nil) }, nil)
	var status atomic.Int32
	var body atomic.Pointer[string]
	empty := ""
	body.Store(&empty)
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if st := int(status.Load()); st != 0 {
			if b := *body.Load(); b != "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(st)
				_, _ = w.Write([]byte(b))
				return
			}
			http.Error(w, http.StatusText(st), st)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts, func(st int, b string) {
		body.Store(&b)
		status.Store(int32(st))
	}
}

func httpEcho(ctx context.Context, t *testing.T, ts *httptest.Server) (*Session, toolloop.Tool) {
	t.Helper()
	sess, err := Connect(ctx, StreamableHTTP(ts.URL, ts.Client()), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	tools, err := sess.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return sess, byName(t, tools, "echo")
}

func TestStreamableHTTPClasses(t *testing.T) {
	call := llms.ToolCall{ID: "1", Name: "echo", Parameters: raw(`{"msg":"x"}`)}
	ctx := context.Background()

	t.Run("success", func(t *testing.T) {
		ts, _ := httpServer(t)
		_, echo := httpEcho(ctx, t, ts)
		res, err := echo.Execute(ctx, call)
		if err != nil || res.Content != "echo: x" {
			t.Fatalf("Execute = %+v, %v", res, err)
		}
	})
	t.Run("server error", func(t *testing.T) {
		ts, _ := httpServer(t)
		sess, _ := httpEcho(ctx, t, ts)
		tools, _ := sess.Tools(ctx)
		_, err := byName(t, tools, "protocol_error").Execute(ctx, llms.ToolCall{ID: "1", Name: "protocol_error"})
		wantClass(t, err, ErrServerError)
	})
	t.Run("unreachable server is unknown", func(t *testing.T) {
		ts, _ := httpServer(t)
		_, echo := httpEcho(ctx, t, ts)
		ts.CloseClientConnections()
		ts.Close()
		_, err := echo.Execute(ctx, call)
		wantClass(t, err, ErrOutcomeUnknown)
	})
	t.Run("expired session is unknown, then not sent", func(t *testing.T) {
		ts, fail := httpServer(t)
		sess, echo := httpEcho(ctx, t, ts)
		fail(http.StatusNotFound, "")
		_, err := echo.Execute(ctx, call)
		wantClass(t, err, ErrOutcomeUnknown)
		if !errors.Is(err, mcp.ErrSessionMissing) {
			t.Errorf("error %q: want the SDK's session-missing error (the path under test)", err)
		}
		waitEnded(t, sess)
		_, err = echo.Execute(ctx, call)
		wantClass(t, err, ErrNotSent)
	})
	t.Run("JSON-RPC error in an HTTP error is unknown", func(t *testing.T) {
		ts, fail := httpServer(t)
		_, echo := httpEcho(ctx, t, ts)
		fail(http.StatusBadRequest, `{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"bad"}}`)
		_, err := echo.Execute(ctx, call)
		wantClass(t, err, ErrOutcomeUnknown)
	})
	t.Run("HTTP 500 is unknown", func(t *testing.T) {
		ts, fail := httpServer(t)
		_, echo := httpEcho(ctx, t, ts)
		fail(http.StatusInternalServerError, "")
		_, err := echo.Execute(ctx, call)
		wantClass(t, err, ErrOutcomeUnknown)
	})
}
