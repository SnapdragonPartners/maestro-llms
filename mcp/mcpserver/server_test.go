package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
	"github.com/SnapdragonPartners/maestro-llms/mcp/mcpclient"
)

// stdioServerEnv makes the test binary act as a stdio server (see TestMain).
const stdioServerEnv = "MCPSERVER_TEST_STDIO_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(stdioServerEnv) == "1" {
		s, err := New(testTools(), Options{})
		if err == nil {
			err = s.ServeStdio(context.Background())
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "stdio server:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

var png = []byte("\x89PNG\r\n\x1a\nx")

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func tool(name, schema string, exec func(context.Context, llms.ToolCall) (toolloop.ToolResult, error)) toolloop.Tool {
	return toolloop.Tool{
		Definition: llms.ToolDefinition{Name: name, Description: name + " tool", InputSchema: raw(schema)},
		Execute:    exec,
	}
}

func text(s string) (toolloop.ToolResult, error) { return toolloop.ToolResult{Content: s}, nil }

// testTools covers each result kind and failure.
func testTools() []toolloop.Tool {
	return []toolloop.Tool{
		tool("echo", `{"type":"object","properties":{"msg":{"type":"string"}}}`, func(_ context.Context, c llms.ToolCall) (toolloop.ToolResult, error) {
			return text("args: " + string(c.Parameters))
		}),
		tool("whoami", "", func(ctx context.Context, _ llms.ToolCall) (toolloop.ToolResult, error) {
			info, ok := TokenInfoFromContext(ctx)
			if !ok {
				return text("anonymous")
			}
			return text(info.Subject + " " + strings.Join(info.Scopes, ",") + " " + fmt.Sprint(info.Extra["tenant"]))
		}),
		tool("media", "", func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) {
			return toolloop.ToolResult{Parts: []llms.ContentPart{
				llms.Text("look"), llms.Image("image/png", png), llms.Document("application/pdf", []byte("%PDF")),
			}}, nil
		}),
		tool("refuse", "", func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) {
			return toolloop.ToolResult{Content: "not allowed", IsError: true}, nil
		}),
		tool("boom", "", func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) {
			return toolloop.ToolResult{}, errors.New("db at 10.0.0.7:5432 refused")
		}),
		tool("bad_result", "", func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) {
			return toolloop.ToolResult{Parts: []llms.ContentPart{llms.Document("image/png", png)}}, nil
		}),
	}
}

func TestNewInvalidTools(t *testing.T) {
	ok := func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) { return text("") }
	cases := map[string]struct {
		tools []toolloop.Tool
		bad   []string // tools reported, in order
		want  string
	}{
		"bad name":         {[]toolloop.Tool{tool("1st", "", ok)}, []string{"1st"}, "does not match"},
		"long name":        {[]toolloop.Tool{tool(strings.Repeat("a", 65), "", ok)}, []string{strings.Repeat("a", 65)}, "does not match"},
		"dotted name":      {[]toolloop.Tool{tool("a.b", "", ok)}, []string{"a.b"}, "does not match"},
		"duplicate":        {[]toolloop.Tool{tool("a", "", ok), tool("a", "", ok)}, []string{"a", "a"}, "shared by 2"},
		"nil execute":      {[]toolloop.Tool{tool("a", "", nil)}, []string{"a"}, "nil Execute"},
		"array schema":     {[]toolloop.Tool{tool("a", `{"type":"array"}`, ok)}, []string{"a"}, `"array"`},
		"untyped schema":   {[]toolloop.Tool{tool("a", `{"properties":{}}`, ok)}, []string{"a"}, `no top-level "type"`},
		"not json":         {[]toolloop.Tool{tool("a", `{`, ok)}, []string{"a"}, "not valid JSON"},
		"sdk: huge number": {[]toolloop.Tool{tool("a", `{"type":"object","default":1e1000}`, ok)}, []string{"a"}, "MCP SDK rejects"},
		"sdk: header on object": {[]toolloop.Tool{tool("a",
			`{"type":"object","properties":{"o":{"type":"object","x-mcp-header":"X-O"}}}`, ok)}, []string{"a"}, "MCP SDK rejects"},
		"sdk: empty header": {[]toolloop.Tool{tool("a",
			`{"type":"object","properties":{"s":{"type":"string","x-mcp-header":""}}}`, ok)}, []string{"a"}, "MCP SDK rejects"},
		"sdk: invalid header name": {[]toolloop.Tool{tool("a",
			`{"type":"object","properties":{"s":{"type":"string","x-mcp-header":"bad header"}}}`, ok)}, []string{"a"}, "MCP SDK rejects"},
		"sdk: duplicate header": {[]toolloop.Tool{tool("a",
			`{"type":"object","properties":{"s":{"type":"string","x-mcp-header":"X-A"},"t":{"type":"string","x-mcp-header":"x-a"}}}`, ok)}, []string{"a"}, "MCP SDK rejects"},
		"several": {[]toolloop.Tool{tool("good", "", ok), tool("9", "", ok), tool("b", "", nil)}, []string{"9", "b"}, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s, err := New(c.tools, Options{})
			if err == nil {
				t.Fatalf("New = %v, nil; want an error", s)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q doesn't contain %q", err, c.want)
			}
			var got []string
			joined, ok := errors.Unwrap(err).(interface{ Unwrap() []error })
			if !ok {
				t.Fatalf("%v doesn't wrap a joined error", err)
			}
			for _, e := range joined.Unwrap() {
				var ite *InvalidToolError
				if !errors.As(e, &ite) {
					t.Fatalf("%v is not an InvalidToolError", e)
				}
				got = append(got, ite.Tool)
			}
			if fmt.Sprint(got) != fmt.Sprint(c.bad) {
				t.Fatalf("reported tools %v, want %v", got, c.bad)
			}
		})
	}
}

func TestNewValid(t *testing.T) {
	// A valid header annotation passes the SDK probe.
	tools := append(testTools(), tool("hdr",
		`{"type":"object","properties":{"s":{"type":"string","x-mcp-header":"X-Region"}}}`,
		func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) { return text("") }))
	if _, err := New(tools, Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(nil, Options{}); err != nil {
		t.Fatalf("New with no tools: %v", err)
	}
}

func TestAnnotationsUnknownTool(t *testing.T) {
	_, err := New(testTools(), Options{Annotations: map[string]Annotations{"nope": {}}})
	if err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Fatalf("New error = %v, want one naming nope", err)
	}
}

// sdkSession runs s over the in-memory transport and connects a go-sdk
// client, for checks below mcpclient's level.
func sdkSession(t *testing.T, s *Server) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, ct := mcp.NewInMemoryTransports()
	done := make(chan struct{})
	go func() { defer close(done); _ = s.serve(ctx, st) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "v0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close(); cancel(); <-done })
	return cs
}

func TestListing(t *testing.T) {
	yes, no := true, false
	s, err := New(testTools(), Options{
		Name: "dot", Version: "v9", Instructions: "be nice",
		Annotations: map[string]Annotations{
			"echo":   {ReadOnly: &yes, Idempotent: &yes, Destructive: &no, OpenWorld: &no, Title: "Echo"},
			"whoami": {},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cs := sdkSession(t, s)
	init := cs.InitializeResult()
	if init.ServerInfo.Name != "dot" || init.ServerInfo.Version != "v9" || init.Instructions != "be nice" {
		t.Fatalf("initialize = %+v", init)
	}
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]*mcp.Tool{}
	var order []string
	for _, tl := range res.Tools {
		by[tl.Name] = tl
		order = append(order, tl.Name)
	}
	if got := strings.Join(order, ","); got != "bad_result,boom,echo,media,refuse,whoami" {
		t.Fatalf("order %s; the go-sdk lists tools by name", got)
	}
	echo := by["echo"]
	if echo.Description != "echo tool" {
		t.Fatalf("description %q", echo.Description)
	}
	b, _ := json.Marshal(echo.InputSchema)
	if string(b) != `{"properties":{"msg":{"type":"string"}},"type":"object"}` {
		t.Fatalf("schema %s", b)
	}
	a := echo.Annotations
	if a == nil || !a.ReadOnlyHint || !a.IdempotentHint || *a.DestructiveHint || *a.OpenWorldHint || a.Title != "Echo" {
		t.Fatalf("annotations %+v", a)
	}
	if w := by["whoami"]; w.Annotations == nil || w.Annotations.ReadOnlyHint || w.Annotations.DestructiveHint != nil {
		t.Fatalf("zero annotations %+v", w.Annotations)
	}
	if by["media"].Annotations != nil {
		t.Fatal("unannotated tool has annotations")
	}
	b, _ = json.Marshal(by["whoami"].InputSchema)
	if string(b) != `{"type":"object"}` {
		t.Fatalf("empty schema served as %s", b)
	}
}

func TestCallArguments(t *testing.T) {
	var calls int
	var mu sync.Mutex
	tools := []toolloop.Tool{tool("a", "", func(_ context.Context, c llms.ToolCall) (toolloop.ToolResult, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		if c.ID != "" || c.Name != "a" {
			return toolloop.ToolResult{}, fmt.Errorf("call %+v", c)
		}
		return text(string(c.Parameters))
	})}
	s, err := New(tools, Options{})
	if err != nil {
		t.Fatal(err)
	}
	cs := sdkSession(t, s)
	ctx := context.Background()
	for _, c := range []struct {
		args any
		want string
	}{
		{nil, "{}"},
		{map[string]any{"x": 1}, `{"x":1}`},
		{raw(`{"n":12345678901234567890}`), `{"n":12345678901234567890}`},
		{raw(`{"n":1e1000,"m":-1e-1000}`), `{"n":1e1000,"m":-1e-1000}`},
	} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "a", Arguments: c.args})
		if err != nil {
			t.Fatal(err)
		}
		tc, _ := res.Content[0].(*mcp.TextContent)
		if got := tc.Text; got != c.want {
			t.Errorf("args %v reached Execute as %s, want %s", c.args, got, c.want)
		}
	}
	mu.Lock()
	before := calls
	mu.Unlock()
	for _, args := range []any{[]int{1}, "str", 3} {
		_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "a", Arguments: args})
		var je *jsonrpc.Error
		if !errors.As(err, &je) || je.Code != jsonrpc.CodeInvalidParams {
			t.Errorf("args %v: error %v, want invalid params", args, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != before {
		t.Fatal("Execute was called with non-object arguments")
	}
}

func TestExecuteErrors(t *testing.T) {
	var mu sync.Mutex
	var reported []error
	s, err := New(testTools(), Options{OnError: func(_ context.Context, err error) {
		mu.Lock()
		reported = append(reported, err)
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	cs := sdkSession(t, s)
	for _, c := range []struct{ tool, original string }{
		{"boom", "10.0.0.7"},
		{"bad_result", "document part has image media type"},
	} {
		_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: c.tool})
		var je *jsonrpc.Error
		if !errors.As(err, &je) || je.Code != jsonrpc.CodeInternalError || je.Message != fmt.Sprintf("tool %q failed", c.tool) {
			t.Fatalf("%s: error %#v, want the fixed internal error", c.tool, err)
		}
		if strings.Contains(err.Error(), c.original) {
			t.Fatalf("%s: client saw the original error: %v", c.tool, err)
		}
		mu.Lock()
		last := reported[len(reported)-1]
		mu.Unlock()
		if !strings.Contains(last.Error(), c.original) {
			t.Fatalf("%s: OnError got %v", c.tool, last)
		}
	}
}

func TestCancellationReachesExecute(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	s, err := New([]toolloop.Tool{tool("slow", "", func(ctx context.Context, _ llms.ToolCall) (toolloop.ToolResult, error) {
		close(started)
		select {
		case <-ctx.Done():
			close(canceled)
			return toolloop.ToolResult{}, ctx.Err()
		case <-time.After(10 * time.Second):
			return text("late")
		}
	})}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	cs := sdkSession(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "slow"}); err == nil {
		t.Fatal("canceled call succeeded")
	}
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("Execute's context was not canceled")
	}
}

func TestStdio(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), stdioServerEnv+"=1")
	ctx := context.Background()
	sess, err := mcpclient.Connect(ctx, mcpclient.Command(cmd), mcpclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	tools, err := sess.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != len(testTools()) {
		t.Fatalf("got %d tools", len(tools))
	}
	byName := map[string]toolloop.Tool{}
	for _, tl := range tools {
		byName[tl.Definition.Name] = tl
	}
	get := func(name string) toolloop.Tool {
		t.Helper()
		tl, ok := byName[name]
		if !ok {
			t.Fatalf("no tool %s in the stdio listing", name)
		}
		return tl
	}
	res, err := get("whoami").Execute(ctx, llms.ToolCall{Name: "whoami"})
	if err != nil || res.Content != "anonymous" {
		t.Fatalf("whoami over stdio = %+v, %v", res, err)
	}
	// Out-of-range numbers pass through unchanged over stdio (ADR-0050 §3).
	const huge = `{"n":1e1000,"m":12345678901234567890}`
	res, err = get("echo").Execute(ctx, llms.ToolCall{Name: "echo", Parameters: raw(huge)})
	if err != nil || res.Content != "args: "+huge {
		t.Fatalf("echo over stdio = %+v, %v; want args: %s", res, err, huge)
	}
}

func TestArgumentsShape(t *testing.T) {
	for in, want := range map[string]string{
		"":                 "{}",
		" null ":           "{}",
		`{"n":1e1000}`:     `{"n":1e1000}`,
		" {\"a\":[1,{}]} ": " {\"a\":[1,{}]} ",
		"[1]":              "error",
		`"x"`:              "error",
		"1e1000":           "error",
		"true":             "error",
		"{":                "error",
		`{"a":1} {}`:       "error",
	} {
		got, err := arguments(raw(in))
		switch {
		case want == "error" && err == nil:
			t.Errorf("arguments(%q) = %s, want an error", in, got)
		case want != "error" && (err != nil || string(got) != want):
			t.Errorf("arguments(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
}

// TestToolsSnapshot checks that New keeps its own copy of each tool, so the
// caller can reuse or clear its slice.
func TestToolsSnapshot(t *testing.T) {
	tools := []toolloop.Tool{tool("a", "", func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) { return text("original") })}
	s, err := New(tools, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tools[0].Execute = nil
	tools[0] = tool("a", "", func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) { return text("replaced") })
	res, err := sdkSession(t, s).CallTool(context.Background(), &mcp.CallToolParams{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if tc, _ := res.Content[0].(*mcp.TextContent); tc == nil || tc.Text != "original" {
		t.Fatalf("result %#v, want the original executor's", res.Content)
	}
}

// TestHugeArgumentsOverHTTP pins a go-sdk v1.8.0 limit (ADR-0050 §3): under
// protocol 2026-07-28 the SDK's Mcp-Name header handling decodes the whole
// tools/call params, arguments included, into float64 values, so a number
// beyond float64's range fails before Execute. This test pins the go-sdk
// client's side (it can't set the header); TestHugeArgumentsServerSide pins
// the server transport's. The paths that pass
// such numbers through unchanged are pinned by TestCallArguments (in
// memory), TestStdio (stdio) and TestHugeArgumentsOlderProtocol (HTTP,
// 2025-06-18). Upstream: modelcontextprotocol/go-sdk#1534. If this starts
// passing, update the ADR and README.
func TestHugeArgumentsOverHTTP(t *testing.T) {
	var mu sync.Mutex
	var received []string
	s, err := New([]toolloop.Tool{tool("rec", "", func(_ context.Context, c llms.ToolCall) (toolloop.ToolResult, error) {
		mu.Lock()
		received = append(received, string(c.Parameters))
		mu.Unlock()
		return text("ok")
	})}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	ctx := context.Background()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "v0"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	if v := cs.InitializeResult().ProtocolVersion; v != "2026-07-28" {
		t.Skipf("negotiated %s; the limit is specific to 2026-07-28", v)
	}
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "rec", Arguments: raw(`{"n":1e1000}`)}); err == nil {
		t.Fatal("go-sdk now accepts out-of-range numbers over HTTP; update ADR-0050 §3 and the README")
	}
	// Integers beyond 2^53 reach Execute exactly as sent, not re-encoded.
	const big = `{"n":12345678901234567890}`
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "rec", Arguments: raw(big)}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 || received[0] != big {
		t.Fatalf("Execute received %q; want only %s (the 1e1000 call must fail before Execute)", received, big)
	}
}

// TestHugeArgumentsOlderProtocol pins that over HTTP with a protocol older
// than 2026-07-28 (no Mcp-Name header) arguments reach Execute unchanged,
// out-of-range numbers included (ADR-0050 §3).
func TestHugeArgumentsOlderProtocol(t *testing.T) {
	s, err := New(testTools(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	const huge = `{"n":1e1000,"m":12345678901234567890}`
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":` + huge + `}}`
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &out); err != nil || len(out.Result.Content) != 1 {
		t.Fatalf("response %d %s (%v)", resp.StatusCode, b, err)
	}
	if got := out.Result.Content[0].Text; got != "args: "+huge {
		t.Fatalf("Execute got %s, want args: %s", got, huge)
	}
}

// TestHugeArgumentsServerSide pins the server half of the go-sdk limit
// (ADR-0050 §3, go-sdk#1534) with a raw 2026-07-28 request that sets the
// Mcp-Name header itself, as a non-Go client such as ChatGPT would: the
// server transport rejects an out-of-range number before Execute. A control
// request with an ordinary number shows the request shape is accepted.
func TestHugeArgumentsServerSide(t *testing.T) {
	var mu sync.Mutex
	var received []string
	s, err := New([]toolloop.Tool{tool("rec", "", func(_ context.Context, c llms.ToolCall) (toolloop.ToolResult, error) {
		mu.Lock()
		received = append(received, string(c.Parameters))
		mu.Unlock()
		return text("ok")
	})}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	post := func(args string) (int, string) {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"rec","arguments":` + args +
			`,"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"raw","version":"v0"},"io.modelcontextprotocol/clientCapabilities":{}}}}`
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2026-07-28")
		req.Header.Set("Mcp-Method", "tools/call")
		req.Header.Set("Mcp-Name", "rec")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	const ordinary = `{"n":12345678901234567890}`
	if code, body := post(ordinary); code != http.StatusOK || !strings.Contains(body, `"ok"`) {
		t.Fatalf("control request: %d %s", code, body)
	}
	code, body := post(`{"n":1e1000}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "failed to extract name") {
		t.Fatalf("go-sdk server behavior changed for out-of-range numbers under 2026-07-28 (%d %s); update ADR-0050 §3 and the README", code, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 || received[0] != ordinary {
		t.Fatalf("Execute received %q; want only the control request's arguments", received)
	}
}
