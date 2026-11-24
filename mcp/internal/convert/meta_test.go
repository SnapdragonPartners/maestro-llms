package convert

import "testing"

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
		if err := CheckMetaKey(k); err != nil {
			t.Errorf("CheckMetaKey(%q) = %v, want valid", k, err)
		}
	}
	for _, k := range invalid {
		if err := CheckMetaKey(k); err == nil {
			t.Errorf("CheckMetaKey(%q) = nil, want an error", k)
		}
	}
}

func TestReservedMetaKey(t *testing.T) {
	reserved := []string{
		"io.modelcontextprotocol/protocolVersion", "modelcontextprotocol.io/x",
		"mcp/x", "mcp.dev/x", "tools.mcp.com/x", "api.MCP.org/x", "progressToken",
	}
	kept := []string{
		"run", "com.example/run", "mcpx/y", "com.example.mcpish/x",
		"progressToken2", "app/progressToken", "bad key", "/x", "",
	}
	for _, k := range reserved {
		if !ReservedMetaKey(k) {
			t.Errorf("ReservedMetaKey(%q) = false, want true", k)
		}
	}
	for _, k := range kept {
		if ReservedMetaKey(k) {
			t.Errorf("ReservedMetaKey(%q) = true, want false", k)
		}
	}
}
