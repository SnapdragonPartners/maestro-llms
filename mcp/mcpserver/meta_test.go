package mcpserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
	"github.com/SnapdragonPartners/maestro-llms/mcp/mcpclient"
)

// metaTools returns a tool "meta" that answers with the _meta it sees as
// JSON ("none" when MetaFromContext reports nothing), then modifies the map.
func metaTools() []toolloop.Tool {
	return []toolloop.Tool{tool("meta", "", func(ctx context.Context, _ llms.ToolCall) (toolloop.ToolResult, error) {
		m, ok := MetaFromContext(ctx)
		if !ok {
			return text("none")
		}
		b, err := json.Marshal(m)
		if err != nil {
			return toolloop.ToolResult{}, err
		}
		m["added/by-tool"] = true // each call owns its map
		return text(string(b))
	})}
}

func callMeta(ctx context.Context, t *testing.T, cs *mcp.ClientSession, meta map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "meta", Meta: meta})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || len(res.Content) != 1 {
		t.Fatalf("result = %+v", res)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content = %T", res.Content[0])
	}
	return tc.Text
}

func TestMetaFromContext(t *testing.T) {
	s, err := New(metaTools(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	cs := sdkSession(t, s)
	ctx := context.Background()

	t.Run("app keys pass, MCP's own are left out", func(t *testing.T) {
		got := callMeta(ctx, t, cs, map[string]any{
			"com.example/run":                "run-7",
			"key":                            map[string]any{"n": 3},
			"odd key":                        []any{1, "x"}, // not a valid key name, but not reserved: passed as sent
			"io.modelcontextprotocol/appKey": "x",           // not the SDK's protocolVersion, which would replace the SDK's own
			"mcp.dev/thing":                  1,
			"progressToken":                  "p",
		})
		var m map[string]any
		if err := json.Unmarshal([]byte(got), &m); err != nil {
			t.Fatalf("meta %q: %v", got, err)
		}
		want := `{"com.example/run":"run-7","key":{"n":3},"odd key":[1,"x"]}`
		if b, _ := json.Marshal(m); string(b) != want {
			t.Errorf("tool saw %s, want %s", b, want)
		}
	})
	t.Run("numbers are float64", func(t *testing.T) {
		s2, err := New([]toolloop.Tool{tool("meta", "", func(ctx context.Context, _ llms.ToolCall) (toolloop.ToolResult, error) {
			m, _ := MetaFromContext(ctx)
			if _, ok := m["n"].(float64); !ok {
				return text("not float64")
			}
			return text("float64")
		})}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		cs2 := sdkSession(t, s2) //nolint:contextcheck // the session's lifetime is the test's, not ctx's
		if got := callMeta(ctx, t, cs2, map[string]any{"n": 3}); got != "float64" {
			t.Errorf("number decoded as: %s", got)
		}
	})
	t.Run("none sent", func(t *testing.T) {
		if got := callMeta(ctx, t, cs, nil); got != "none" {
			t.Errorf("tool saw %s, want none", got)
		}
	})
	t.Run("only reserved keys", func(t *testing.T) {
		if got := callMeta(ctx, t, cs, map[string]any{"progressToken": 1, "io.modelcontextprotocol/x": 2}); got != "none" {
			t.Errorf("tool saw %s, want none", got)
		}
	})
	t.Run("each call owns its map", func(t *testing.T) {
		meta := map[string]any{"a": "1"}
		for range 2 {
			if got := callMeta(ctx, t, cs, meta); got != `{"a":"1"}` {
				t.Errorf("tool saw %s; a previous call's change leaked", got)
			}
		}
	})
	t.Run("absent outside a call", func(t *testing.T) {
		if m, ok := MetaFromContext(ctx); ok || m != nil {
			t.Errorf("MetaFromContext(background) = %v, %v", m, ok)
		}
	})
}

// TestMetaRoundTrip: what mcpclient's Options.Meta sends is what a tool
// served by mcpserver reads, over Streamable HTTP (ADR-0053, ADR-0050).
func TestMetaRoundTrip(t *testing.T) {
	s, err := New(metaTools(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	type runKey struct{}
	ctx := context.Background()
	sess, err := mcpclient.Connect(ctx, mcpclient.StreamableHTTP(ts.URL, ts.Client()), mcpclient.Options{
		Meta: func(ctx context.Context, call llms.ToolCall) (map[string]any, error) {
			return map[string]any{
				"com.example/run":             ctx.Value(runKey{}),
				"com.example/idempotency-key": "k-" + call.ID,
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	tools, err := sess.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tools[0].Execute(context.WithValue(ctx, runKey{}, "run-9"), llms.ToolCall{ID: "c1", Name: "meta"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"com.example/idempotency-key":"k-c1","com.example/run":"run-9"}`
	if res.Content != want {
		t.Errorf("tool saw %s, want %s", res.Content, want)
	}
}
