package devcouncil

import (
	"strings"
	"testing"

	"manvi/mcp"
)

// A surface built without an MCP manager must refuse, not discover servers on
// its own initiative.
//
// The improvised manager this replaces made mcp.enabled unenforceable through
// this path: the setting is applied where the manager is built, so a surface
// that builds its own has already answered the question. The harness always
// passes one now, which means the nil case is another embedder — DevPrism over
// the sidecar, or a caller not yet written — and silently turning MCP on for
// exactly those callers is not a decision this package may make.
func TestASurfaceBuiltWithoutAnMCPManagerRefusesRatherThanDiscovering(t *testing.T) {
	f := newFixtureRunner(t, &recordingRunner{})
	if f.reg.deps.MCP != nil {
		t.Fatal("the fixture wired an MCP manager; this test no longer proves anything")
	}

	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"mcp_list_tools", map[string]any{}},
		{"mcp_list_resources", map[string]any{"server_name": "s"}},
		{"mcp_call_tool", map[string]any{"server_name": "s", "tool_name": "t", "arguments": map[string]any{}}},
		{"mcp_read_resource", map[string]any{"server_name": "s", "uri": "file:///x"}},
	} {
		t.Run(call.tool, func(t *testing.T) {
			res := f.call(call.tool, call.args)
			if !res.IsError {
				t.Fatalf("%s answered without a manager: %s", call.tool, res.Text)
			}
			// The refusal must name the cause, because the remedy differs from
			// the one an operator gets for mcp.enabled=false: a Deps field to
			// populate, not a setting to change.
			if !strings.Contains(res.Text, "Deps.MCP is nil") {
				t.Errorf("%s refused without naming the cause: %s", call.tool, res.Text)
			}
		})
	}
}

// And a surface that WAS given a manager must still work, or the refusal above
// is just breakage.
func TestASurfaceWithAnMCPManagerStillAnswers(t *testing.T) {
	f := newFixtureRunner(t, &recordingRunner{})
	f.reg.deps.MCP = mcp.NewManager(f.root)

	res := f.call("mcp_list_tools", map[string]any{})
	if res.IsError {
		t.Fatalf("a wired manager refused: %s", res.Text)
	}
}
