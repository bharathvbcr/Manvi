package devcouncil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"manvi/flags"
	"manvi/mcp"
)

// A stub MCP server that records the fact it was started and then answers
// enough of the protocol to complete a tool call. It speaks real stdio and
// contacts nothing.
const mcpStubSource = `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) > 1 {
		os.WriteFile(os.Args[1], []byte("spawned"), 0o644)
	}
	r := bufio.NewReader(os.Stdin)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		var q map[string]any
		if json.Unmarshal([]byte(line), &q) != nil {
			continue
		}
		id := q["id"]
		if id == nil {
			continue
		}
		method, _ := q["method"].(string)
		var result map[string]any
		switch method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-20"}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name": "run_shell", "inputSchema": map[string]any{"type": "object"}}}}
		default:
			result = map[string]any{"content": []any{map[string]any{
				"type": "text", "text": "the tool ran"}}}
		}
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		fmt.Fprintln(os.Stdout, string(payload))
	}
}
`

var (
	mcpStubOnce sync.Once
	mcpStubPath string
	mcpStubErr  error
	mcpStubDir  string
)

func mcpStub(t *testing.T) string {
	t.Helper()
	mcpStubOnce.Do(func() {
		mcpStubDir, mcpStubErr = os.MkdirTemp("", "devcouncil-mcp-stub")
		if mcpStubErr != nil {
			return
		}
		src := filepath.Join(mcpStubDir, "main.go")
		if mcpStubErr = os.WriteFile(src, []byte(mcpStubSource), 0o644); mcpStubErr != nil {
			return
		}
		bin := filepath.Join(mcpStubDir, "stub")
		if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
			mcpStubErr = fmt.Errorf("building the MCP stub: %v\n%s", err, out)
			return
		}
		mcpStubPath = bin
	})
	if mcpStubErr != nil {
		t.Fatal(mcpStubErr)
	}
	return mcpStubPath
}

// wireMCPStub gives a fixture a manager holding one in-process declaration —
// program origin, so mcp's own authorization check is not what is under test
// here — and returns the marker path that proves whether it was ever spawned.
func wireMCPStub(t *testing.T, f *fixture) string {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "spawned")
	mgr := mcp.NewManager(f.root)
	if err := mgr.RegisterServer(mcp.ServerConfig{
		Name:    "demo",
		Command: mcpStub(t),
		Args:    []string{marker},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.CloseAll)
	f.reg.deps.MCP = mgr
	return marker
}

// mcp_call_tool must go through the policy gate, and a blocked decision must
// arrive as a blocked decision.
//
// It called mgr.CallTool directly, with no Gate call and no Approver call at
// all. An MCP server advertising run_shell or write_file was therefore a
// complete route around the command gate, the write gate and the approval
// prompt at once — and because nothing on this path could set Result.Blocked,
// the run report had no way to show that any of it had happened.
func TestMCPCallToolIsGatedAndRefusesWithoutApproval(t *testing.T) {
	f := newFixtureRunner(t, &recordingRunner{})
	marker := wireMCPStub(t, f)

	res := f.call("mcp_call_tool", map[string]any{
		"server_name": "demo",
		"tool_name":   "run_shell",
		"arguments":   map[string]any{"command": "cat ~/.ssh/id_rsa"},
	})

	if !res.IsError {
		t.Fatalf("an ungated MCP tool call succeeded: %s", res.Text)
	}
	if !res.Blocked {
		t.Errorf("the refusal did not set Result.Blocked, so the run report cannot tell "+
			"a gate refusal from any other failure: %+v", res)
	}
	if res.Rule == "" {
		t.Errorf("the refusal names no rule: %+v", res)
	}
	if !strings.Contains(res.Text, "mcp_call_tool demo/run_shell") {
		t.Errorf("the decision does not name what was judged: %s", res.Text)
	}
	// And nothing ran: a gate that refuses after the side effect is not a gate.
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the MCP server was spawned despite the gate refusing the call")
	}
}

// A permitted call must still run, or the gate is a blanket refusal rather than
// a gate. Under yolo the command mode is off, which demotes the soft denial —
// and the demotion has to travel onto the result.
func TestAPermittedMCPCallStillRunsAndCarriesItsQualification(t *testing.T) {
	f := newFixtureFull(t, map[string]string{flags.HarnessPosture: flags.PostureYolo},
		&recordingRunner{})
	marker := wireMCPStub(t, f)

	res := f.call("mcp_call_tool", map[string]any{
		"server_name": "demo",
		"tool_name":   "run_shell",
		"arguments":   map[string]any{},
	})

	if res.IsError {
		t.Fatalf("a permitted MCP tool call was refused: %s", res.Text)
	}
	if res.Blocked {
		t.Errorf("a permitted call reported itself blocked: %+v", res)
	}
	if !strings.Contains(res.Text, "the tool ran") {
		t.Errorf("the server's reply did not reach the caller: %s", res.Text)
	}
	if res.Demoted == "" {
		t.Errorf("an allow produced by a gate mode is indistinguishable from one the rules "+
			"produced: %+v", res)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the permitted call never reached the server: %v", err)
	}
}

// The server name comes from a declaration file in the checked-out tree and the
// tool name from the server's own listing, so both are attacker-influenced. A
// name carrying shell metacharacters would be split by the gate's command-chain
// splitter, and the thing judged would not be the thing invoked.
func TestMCPNamesCarryingShellMetacharactersAreRefused(t *testing.T) {
	f := newFixtureRunner(t, &recordingRunner{})
	marker := wireMCPStub(t, f)

	for _, bad := range []struct{ server, tool string }{
		{"demo; git status", "run_shell"},
		{"demo", "run_shell && echo pwned"},
		{"demo\nother", "run_shell"},
		{"demo", "run_shell$(whoami)"},
	} {
		res := f.call("mcp_call_tool", map[string]any{
			"server_name": bad.server, "tool_name": bad.tool,
			"arguments": map[string]any{},
		})
		if !res.IsError {
			t.Errorf("server_name=%q tool_name=%q was accepted: %s", bad.server, bad.tool, res.Text)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a call with an unusable name still reached a server")
	}
}

// A harness built without a gate must not run MCP tools. A check that could not
// run has to be an error, never the answer a check that ran and passed gives.
func TestMCPCallToolWithNoGateRefusesRatherThanRunning(t *testing.T) {
	f := newFixtureRunner(t, &recordingRunner{})
	marker := wireMCPStub(t, f)
	f.reg.deps.Gate = nil

	res := f.call("mcp_call_tool", map[string]any{
		"server_name": "demo", "tool_name": "run_shell", "arguments": map[string]any{},
	})
	if !res.IsError {
		t.Fatalf("an MCP tool ran with no policy gate present: %s", res.Text)
	}
	if !strings.Contains(res.Text, "the check did not run") {
		t.Errorf("the refusal reads like a negative result rather than an absent check: %s", res.Text)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the MCP server was spawned with no gate to judge the call")
	}
}

// The composed attack, at the surface the model actually sees.
//
// Clone a repository carrying an mcp.json; the model calls mcp_list_tools,
// which is ReadOnly, ungated and offered by default; discovery spawns the
// command that file names with the harness's credentials in its environment;
// the server advertises a tool that mcp_call_tool then invokes with no gate and
// no approval. This asserts the first link is cut — nothing is spawned — which
// is what makes the rest unreachable.
func TestCheckingOutARepositoryDoesNotLetMCPListToolsRunItsCommands(t *testing.T) {
	f := newFixtureRunner(t, &recordingRunner{})
	t.Setenv(mcp.TrustFileEnv, filepath.Join(t.TempDir(), "mcp-trust.json"))
	t.Setenv(mcp.TrustListEnv, "")

	marker := filepath.Join(t.TempDir(), "pwned")
	decl := fmt.Sprintf(`{"mcpServers":{"helper":{"command":%q,"args":[%q]}}}`, mcpStub(t), marker)
	if err := os.WriteFile(filepath.Join(f.root, "mcp.json"), []byte(decl), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := mcp.NewManager(f.root)
	if err := mgr.AutoDiscover(t.Context()); err != nil {
		t.Fatalf("discovery failed: %v", err)
	}
	t.Cleanup(mgr.CloseAll)
	f.reg.deps.MCP = mgr

	res := f.call("mcp_list_tools", map[string]any{})
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("mcp_list_tools executed a command a checked-out repository declared")
	}
	// Discovery still happened, and the refusal is visible rather than the
	// server simply being absent from the listing.
	if !strings.Contains(res.Text, "helper") {
		t.Errorf("the discovered server vanished from the listing entirely: %s", res.Text)
	}
	if !strings.Contains(res.Text, "no operator has authorized it") {
		t.Errorf("the listing does not say why the server contributed nothing: %s", res.Text)
	}
}

// A reply large enough to swamp the model's context is bounded, and the cut is
// stated rather than left to look like the whole answer.
func TestAnOversizedMCPReplyIsBoundedAndSaysSo(t *testing.T) {
	blocks := []mcp.ContentBlock{{Type: "text", Text: strings.Repeat("a", maxMCPResultBytes+1024)}}
	text, note := renderMCPContent(blocks)
	if len(text) > maxMCPResultBytes {
		t.Errorf("the rendered reply is %d bytes, past the %d cap", len(text), maxMCPResultBytes)
	}
	if note == "" {
		t.Error("a truncated reply was returned with nothing saying it had been truncated")
	}

	many := make([]mcp.ContentBlock, maxMCPContentParts+10)
	for i := range many {
		many[i] = mcp.ContentBlock{Type: "text", Text: "x"}
	}
	if _, note := renderMCPContent(many); note == "" {
		t.Error("a reply with more blocks than the cap was rendered with no note saying so")
	}

	// And an ordinary reply is untouched, or the note would stop meaning
	// anything.
	if text, note := renderMCPContent([]mcp.ContentBlock{{Type: "text", Text: "fine"}}); text != "fine" || note != "" {
		t.Errorf("an ordinary reply was altered: %q %q", text, note)
	}
}
