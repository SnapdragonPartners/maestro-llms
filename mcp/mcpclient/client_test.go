package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

// stdioServerEnv makes the test binary act as a stdio MCP server (see
// TestMain), so Command is tested against a real child process.
const stdioServerEnv = "MCPCLIENT_TEST_STDIO_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(stdioServerEnv) == "1" {
		s := newServer(nil)
		if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			fmt.Fprintln(os.Stderr, "stdio server:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// newServer builds a go-sdk server with a fixed set of tools. extra adds
// more (or replaces them by name).
func newServer(opts *mcp.ServerOptions, extra ...func(*mcp.Server)) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v0"}, opts)
	s.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: "Echo the msg argument.",
		InputSchema: raw(`{"type":"object","properties":{"msg":{"type":"string"}},"required":["msg"]}`),
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return textResult("echo: " + args.Msg), nil
	})
	s.AddTool(&mcp.Tool{
		Name:        "args",
		Title:       "Raw arguments",
		InputSchema: raw(`{"type":"object"}`),
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return textResult(string(req.Params.Arguments)), nil
	})
	s.AddTool(&mcp.Tool{
		Name:        "fail",
		InputSchema: raw(`{"type":"object"}`),
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "it broke"}}}, nil
	})
	s.AddTool(&mcp.Tool{
		Name:        "protocol_error",
		InputSchema: raw(`{"type":"object"}`),
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, errors.New("handler exploded")
	})
	s.AddTool(&mcp.Tool{
		Name:        "image",
		InputSchema: raw(`{"type":"object"}`),
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{},
			&mcp.TextContent{Text: "a picture"},
			&mcp.ImageContent{MIMEType: "image/png", Data: []byte("\x89PNG\r\n\x1a\nx")},
		}}, nil
	})
	s.AddTool(&mcp.Tool{
		Name:        "with_refs",
		InputSchema: raw(`{"type":"object","properties":{"p":{"$ref":"#/$defs/P"}},"$defs":{"P":{"type":"string"}}}`),
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return textResult("ok"), nil
	})
	for _, f := range extra {
		f(s)
	}
	return s
}

// connect runs s over the in-memory transport and opens a session to it.
func connect(t *testing.T, s *mcp.Server, opts Options) *Session {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	sess, err := Connect(ctx, Transport{t: ct}, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func byName(t *testing.T, tools []toolloop.Tool, name string) toolloop.Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.Definition.Name == name {
			return tl
		}
	}
	t.Fatalf("no tool %q", name)
	return toolloop.Tool{}
}

func names(tools []toolloop.Tool) []string {
	out := make([]string, len(tools))
	for i := range tools {
		out[i] = tools[i].Definition.Name
	}
	return out
}

func TestToolsDefinitions(t *testing.T) {
	sess := connect(t, newServer(nil), Options{})
	tools, err := sess.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(tools), ","); got != "args,echo,fail,image,protocol_error,with_refs" {
		t.Errorf("tools = %s", got) // the go-sdk server lists tools sorted by name
	}
	echo := byName(t, tools, "echo")
	if echo.Definition.Description != "Echo the msg argument." {
		t.Errorf("description = %q", echo.Definition.Description)
	}
	if d := byName(t, tools, "args").Definition.Description; d != "Raw arguments" {
		t.Errorf("title fallback = %q", d)
	}
	var schema map[string]any
	if err := json.Unmarshal(byName(t, tools, "with_refs").Definition.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if _, ok := schema["$defs"]; ok {
		t.Errorf("$defs not removed: %v", schema)
	}
	props, _ := schema["properties"].(map[string]any)
	if p, _ := props["p"].(map[string]any); p["type"] != "string" {
		t.Errorf("ref not inlined: %v", schema)
	}
}

func TestToolsPaging(t *testing.T) {
	sess := connect(t, newServer(&mcp.ServerOptions{PageSize: 2}), Options{})
	tools, err := sess.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 6 {
		t.Fatalf("got %d tools across pages, want 6: %v", len(tools), names(tools))
	}
}

func TestFilterAndName(t *testing.T) {
	sess := connect(t, newServer(nil), Options{
		Filter: func(n string) bool { return n == "echo" || n == "args" },
		Name:   func(n string) string { return "srv_" + n },
	})
	tools, err := sess.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(tools), ","); got != "srv_args,srv_echo" {
		t.Fatalf("tools = %s", got)
	}
	// Execute calls the server's name, not the offered one.
	res, err := byName(t, tools, "srv_echo").Execute(context.Background(), llms.ToolCall{ID: "1", Name: "srv_echo", Parameters: raw(`{"msg":"hi"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "echo: hi" {
		t.Errorf("result = %+v", res)
	}
}

func TestToolsFailClosed(t *testing.T) {
	bad := func(s *mcp.Server) {
		noop := func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return textResult(""), nil }
		s.AddTool(&mcp.Tool{Name: "fs.read", InputSchema: raw(`{"type":"object"}`)}, noop)
		s.AddTool(&mcp.Tool{Name: "loop", InputSchema: raw(`{"type":"object","properties":{"n":{"$ref":"#/$defs/N"}},"$defs":{"N":{"type":"object","properties":{"n":{"$ref":"#/$defs/N"}}}}}`)}, noop)
	}
	sess := connect(t, newServer(nil, bad), Options{})
	tools, err := sess.Tools(context.Background())
	if err == nil || tools != nil {
		t.Fatalf("Tools = %v, %v; want no tools and an error", names(tools), err)
	}
	got := map[string]bool{}
	joined, ok := errors.Unwrap(err).(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("error does not wrap a joined error: %v", err)
	}
	for _, e := range joined.Unwrap() {
		var ite *InvalidToolError
		if !errors.As(e, &ite) {
			t.Fatalf("not an InvalidToolError: %v", e)
		}
		got[ite.Tool] = true
	}
	if len(got) != 2 || !got["fs.read"] || !got["loop"] {
		t.Errorf("invalid tools = %v, want fs.read and loop", got)
	}
	var ite *InvalidToolError
	if !errors.As(err, &ite) {
		t.Error("errors.As does not find an InvalidToolError")
	}

	// Filtering the named tools out recovers the rest.
	sess2 := connect(t, newServer(nil, bad), Options{Filter: func(n string) bool { return n != "fs.read" && n != "loop" }})
	tools, err = sess2.Tools(context.Background())
	if err != nil || len(tools) != 6 {
		t.Fatalf("filtered Tools = %v, %v", names(tools), err)
	}

	// A mapping can repair a name.
	sess3 := connect(t, newServer(nil, bad), Options{
		Filter: func(n string) bool { return n != "loop" },
		Name:   func(n string) string { return strings.ReplaceAll(n, ".", "_") },
	})
	if _, err := sess3.Tools(context.Background()); err != nil {
		t.Fatalf("mapped Tools: %v", err)
	}
}

func TestToolsMappedNameCollision(t *testing.T) {
	sess := connect(t, newServer(nil), Options{
		Filter: func(n string) bool { return n == "echo" || n == "args" || n == "fail" },
		Name: func(n string) string {
			if n == "fail" {
				return "fail"
			}
			return "same"
		},
	})
	_, err := sess.Tools(context.Background())
	if err == nil {
		t.Fatal("want a collision error")
	}
	for _, want := range []string{`tool "args"`, `tool "echo"`, `"same"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %s", err, want)
		}
	}
	if strings.Contains(err.Error(), `tool "fail"`) {
		t.Errorf("error names a tool that did not collide: %v", err)
	}
}

func TestExecute(t *testing.T) {
	sess := connect(t, newServer(nil), Options{})
	tools, err := sess.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	t.Run("empty parameters send {}", func(t *testing.T) {
		res, err := byName(t, tools, "args").Execute(ctx, llms.ToolCall{ID: "1", Name: "args"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Content != "{}" {
			t.Errorf("arguments = %q, want {}", res.Content)
		}
	})
	t.Run("parameters pass through", func(t *testing.T) {
		res, err := byName(t, tools, "args").Execute(ctx, llms.ToolCall{ID: "1", Name: "args", Parameters: raw(`{"a":[1,2]}`)})
		if err != nil {
			t.Fatal(err)
		}
		if res.Content != `{"a":[1,2]}` {
			t.Errorf("arguments = %q", res.Content)
		}
	})
	t.Run("isError is model-visible", func(t *testing.T) {
		res, err := byName(t, tools, "fail").Execute(ctx, llms.ToolCall{ID: "1", Name: "fail"})
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || res.Content != "it broke" || res.Stop {
			t.Errorf("result = %+v", res)
		}
	})
	t.Run("protocol error is an Execute error", func(t *testing.T) {
		_, err := byName(t, tools, "protocol_error").Execute(ctx, llms.ToolCall{ID: "1", Name: "protocol_error"})
		if err == nil || !strings.Contains(err.Error(), "handler exploded") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("image becomes parts", func(t *testing.T) {
		res, err := byName(t, tools, "image").Execute(ctx, llms.ToolCall{ID: "1", Name: "image"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Content != "" || len(res.Parts) != 2 || res.Parts[0].Text != "a picture" || res.Parts[1].Type != llms.ContentImage {
			t.Errorf("result = %+v", res)
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := byName(t, tools, "echo").Execute(cctx, llms.ToolCall{ID: "1", Name: "echo", Parameters: raw(`{"msg":"x"}`)}); err == nil {
			t.Fatal("want an error on a canceled context")
		}
	})
}

func TestUnknownToolOnServer(t *testing.T) {
	// A tool removed from the server after listing: the call is the
	// server's error, returned as an Execute error.
	s := newServer(nil)
	sess := connect(t, s, Options{})
	tools, err := sess.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.RemoveTools("echo")
	_, err = byName(t, tools, "echo").Execute(context.Background(), llms.ToolCall{ID: "1", Name: "echo", Parameters: raw(`{"msg":"x"}`)})
	if err == nil {
		t.Fatal("want an error for a removed tool")
	}
}

func TestConcurrentCalls(t *testing.T) {
	sess := connect(t, newServer(nil), Options{})
	tools, err := sess.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	echo := byName(t, tools, "echo")
	var wg sync.WaitGroup
	errs := make(chan error, 24) // one per worker: 20 calls and 4 listings
	for i := range 20 {
		wg.Go(func() {
			msg := fmt.Sprint(i)
			res, err := echo.Execute(context.Background(), llms.ToolCall{ID: msg, Name: "echo", Parameters: raw(`{"msg":"` + msg + `"}`)})
			switch {
			case err != nil:
				errs <- err
			case res.Content != "echo: "+msg:
				errs <- fmt.Errorf("call %s got %q", msg, res.Content)
			}
		})
		if i%5 == 0 {
			wg.Go(func() {
				if _, err := sess.Tools(context.Background()); err != nil {
					errs <- err
				}
			})
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestClose(t *testing.T) {
	sess := connect(t, newServer(nil), Options{})
	tools, err := sess.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
	if _, err := byName(t, tools, "echo").Execute(context.Background(), llms.ToolCall{ID: "1", Name: "echo", Parameters: raw(`{"msg":"x"}`)}); !errors.Is(err, errClosed) {
		t.Errorf("Execute after Close = %v", err)
	}
	if _, err := sess.Tools(context.Background()); !errors.Is(err, errClosed) {
		t.Errorf("Tools after Close = %v", err)
	}
}

func TestZeroTransport(t *testing.T) {
	if _, err := Connect(context.Background(), Transport{}, Options{}); err == nil {
		t.Fatal("want an error for the zero Transport")
	}
	if _, err := Connect(context.Background(), Command(nil), Options{}); err == nil {
		t.Fatal("want an error for Command(nil), not a panic")
	}
}

// rawServer answers initialize and tools/list over the in-memory transport
// with hand-written JSON, for listings the go-sdk's own server refuses to
// produce. list returns the tools/list result for a cursor.
func rawServer(ctx context.Context, t *testing.T, list func(cursor string) string) Transport {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	st, ct := mcp.NewInMemoryTransports()
	conn, err := st.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		for {
			msg, err := conn.Read(ctx)
			if err != nil {
				return
			}
			b, err := jsonrpc.EncodeMessage(msg)
			if err != nil {
				return
			}
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params struct {
					ProtocolVersion string `json:"protocolVersion"`
					Cursor          string `json:"cursor"`
				} `json:"params"`
			}
			if json.Unmarshal(b, &req) != nil || len(req.ID) == 0 {
				continue // a notification or a response
			}
			var result string
			switch req.Method {
			case "initialize":
				result = `{"protocolVersion":"` + req.Params.ProtocolVersion + `","capabilities":{"tools":{}},"serverInfo":{"name":"raw","version":"v0"}}`
			case "tools/list":
				result = list(req.Params.Cursor)
			default:
				result = `{}`
			}
			resp, err := jsonrpc.DecodeMessage([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":` + result + `}`))
			if err != nil || conn.Write(ctx, resp) != nil {
				return
			}
		}
	}()
	return Transport{t: ct}
}

// pagesOf serves pages as tools/list results; page i's nextCursor is
// "c<i+1>".
func pagesOf(pages ...string) func(string) string {
	return func(cursor string) string {
		i := 0
		if cursor != "" {
			i, _ = strconv.Atoi(strings.TrimPrefix(cursor, "c"))
		}
		result := pages[i]
		if i+1 < len(pages) {
			result = result[:len(result)-1] + fmt.Sprintf(`,"nextCursor":"c%d"}`, i+1)
		}
		return result
	}
}

const badHeaderTool = `{"name":"bad_header","inputSchema":{"type":"object","properties":{"a":{"type":"object","x-mcp-header":"X-A"}}}}`

// TestSDKExcludedToolFailsClosed: the go-sdk drops a tool with an invalid
// x-mcp-header annotation, and null entries, from ListTools' result and only
// logs them; Tools reports them instead of offering the rest.
func TestSDKExcludedToolFailsClosed(t *testing.T) {
	ctx := context.Background()
	open := func(t *testing.T, opts Options, pages ...string) *Session {
		t.Helper()
		sess, err := Connect(ctx, rawServer(ctx, t, pagesOf(pages...)), opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sess.Close() })
		return sess
	}

	t.Run("excluded tool", func(t *testing.T) {
		sess := open(t, Options{}, `{"tools":[{"name":"good","inputSchema":{"type":"object"}},`+badHeaderTool+`]}`)
		tools, err := sess.Tools(ctx)
		var ite *InvalidToolError
		if tools != nil || !errors.As(err, &ite) || ite.Tool != "bad_header" || !strings.Contains(err.Error(), "excluded") {
			t.Fatalf("Tools = %v, %v; want no tools and an InvalidToolError for bad_header", names(tools), err)
		}
	})
	t.Run("excluded tool on a later page", func(t *testing.T) {
		sess := open(t, Options{},
			`{"tools":[{"name":"good","inputSchema":{"type":"object"}}]}`,
			`{"tools":[`+badHeaderTool+`]}`)
		var ite *InvalidToolError
		if _, err := sess.Tools(ctx); !errors.As(err, &ite) || ite.Tool != "bad_header" {
			t.Fatalf("Tools error = %v", err)
		}
	})
	t.Run("filter recovers", func(t *testing.T) {
		sess := open(t, Options{Filter: func(n string) bool { return n != "bad_header" }},
			`{"tools":[{"name":"good","inputSchema":{"type":"object"}},`+badHeaderTool+`]}`)
		tools, err := sess.Tools(ctx)
		if err != nil || strings.Join(names(tools), ",") != "good" {
			t.Fatalf("filtered Tools = %v, %v", names(tools), err)
		}
	})
	t.Run("null entry", func(t *testing.T) {
		sess := open(t, Options{}, `{"tools":[{"name":"good","inputSchema":{"type":"object"}},null]}`)
		if _, err := sess.Tools(ctx); err == nil || !strings.Contains(err.Error(), "null entries") {
			t.Fatalf("Tools error = %v, want the null-entry error", err)
		}
	})
	t.Run("repeated cursor", func(t *testing.T) {
		// Page 0 points at c1 and page 1 back at c1's own cursor via a
		// hand-written nextCursor, so the listing never ends.
		sess := open(t, Options{},
			`{"tools":[{"name":"a","inputSchema":{"type":"object"}}]}`,
			`{"tools":[{"name":"b","inputSchema":{"type":"object"}}],"nextCursor":"c1"}`)
		if _, err := sess.Tools(ctx); err == nil || !strings.Contains(err.Error(), "repeated cursor") {
			t.Fatalf("Tools error = %v, want the repeated-cursor error", err)
		}
	})
}

func TestExcluded(t *testing.T) {
	s := &Session{pages: map[string]rawPage{
		"":  {names: []string{"a", "b", "b", "c"}, nulls: 2},
		"x": {names: []string{"d"}},
	}}
	kept := []*mcp.Tool{{Name: "a"}, {Name: "b"}}
	got, nulls := s.excluded("", kept)
	if strings.Join(got, ",") != "b,c" || nulls != 2 {
		t.Errorf("excluded = %v, %d nulls; want [b c] and 2", got, nulls)
	}
	if got, nulls := s.excluded("x", []*mcp.Tool{{Name: "d"}}); got != nil || nulls != 0 {
		t.Errorf("nothing excluded: got %v, %d", got, nulls)
	}
	if got, nulls := s.excluded("unrecorded", nil); got != nil || nulls != 0 {
		t.Errorf("unrecorded page: got %v, %d", got, nulls)
	}
}

func TestStdio(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), stdioServerEnv+"=1")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err := Connect(ctx, Command(cmd), Options{})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := sess.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res, err := byName(t, tools, "echo").Execute(ctx, llms.ToolCall{ID: "1", Name: "echo", Parameters: raw(`{"msg":"over stdio"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "echo: over stdio" {
		t.Errorf("result = %+v", res)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Errorf("server process not reaped after Close: %v", cmd.ProcessState)
	}
}

// TestThroughToolLoop checks the outcome mapping where it matters: in Run.
func TestThroughToolLoop(t *testing.T) {
	sess := connect(t, newServer(nil), Options{})
	tools, err := sess.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run := func(responses ...llms.ChatResponse) toolloop.Outcome {
		return toolloop.Run(context.Background(), toolloop.Config{
			Client:  &testllm.FakeChatClient{Responses: responses},
			Request: llms.ChatRequest{Messages: []llms.Message{llms.UserText("go")}},
			Tools:   tools,
		})
	}

	out := run(testllm.ToolCallResponse("c1", "echo", `{"msg":"loop"}`), testllm.TextResponse("done"))
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("echo: %s %v", out.Kind, out.Err)
	}
	if got := toolResultText(out.Messages); got != "echo: loop" {
		t.Errorf("tool result in transcript = %q", got)
	}

	out = run(testllm.ToolCallResponse("c1", "fail", `{}`), testllm.TextResponse("recovered"))
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Errorf("isError should let the loop continue: %s %v", out.Kind, out.Err)
	}

	out = run(testllm.ToolCallResponse("c1", "image", `{}`), testllm.TextResponse("seen"))
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Errorf("image result rejected by the loop: %s %v", out.Kind, out.Err)
	}

	out = run(testllm.ToolCallResponse("c1", "protocol_error", `{}`), testllm.TextResponse("unreachable"))
	if out.Kind != toolloop.OutcomeToolError {
		t.Errorf("protocol error: %s, want %s", out.Kind, toolloop.OutcomeToolError)
	}
}

func toolResultText(msgs []llms.Message) string {
	for _, m := range msgs {
		for _, p := range m.Content {
			if p.ToolResult != nil {
				return p.ToolResult.Content
			}
		}
	}
	return ""
}

// TestConcurrentListingsStayAssociated: concurrent Tools calls that get
// different listings for the same cursor each judge their own listing. The
// server alternates between a listing with an SDK-excluded tool (must fail)
// and one without it (must succeed).
func TestConcurrentListingsStayAssociated(t *testing.T) {
	ctx := context.Background()
	var served sync.Mutex
	withBad, withoutBad := 0, 0
	list := func(string) string {
		served.Lock()
		defer served.Unlock()
		if (withBad+withoutBad)%2 == 0 {
			withBad++
			return `{"tools":[{"name":"good","inputSchema":{"type":"object"}},` + badHeaderTool + `]}`
		}
		withoutBad++
		return `{"tools":[{"name":"good","inputSchema":{"type":"object"}}]}`
	}
	sess, err := Connect(ctx, rawServer(ctx, t, list), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()

	const calls = 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, failed := 0, 0
	for range calls {
		wg.Go(func() {
			_, err := sess.Tools(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
			} else {
				ok++
			}
		})
	}
	wg.Wait()
	served.Lock()
	defer served.Unlock()
	if withBad+withoutBad != calls {
		t.Fatalf("server answered %d listings for %d calls", withBad+withoutBad, calls)
	}
	if ok != withoutBad || failed != withBad {
		t.Errorf("%d succeeded and %d failed; want %d and %d (one per listing served)", ok, failed, withoutBad, withBad)
	}
}

// TestQueuedToolsHonorsContext: a Tools call queued behind a listing that is
// stuck on the server returns when its own context ends.
func TestQueuedToolsHonorsContext(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	list := func(string) string {
		entered <- struct{}{}
		<-release
		return `{"tools":[{"name":"good","inputSchema":{"type":"object"}}]}`
	}
	sess, err := Connect(ctx, rawServer(ctx, t, list), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()

	first := make(chan error, 1)
	go func() {
		_, ferr := sess.Tools(ctx) // no deadline: holds the gate until released
		first <- ferr
	}()
	<-entered

	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = sess.Tools(short)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued Tools error = %v, want context.DeadlineExceeded", err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("queued Tools returned after %v", waited)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first Tools: %v", err)
	}
}
