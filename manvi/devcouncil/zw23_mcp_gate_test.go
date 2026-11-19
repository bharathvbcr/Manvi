package devcouncil

import (
	"os"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
)

// mcp_list_tools records a gate decision before it starts a server.
//
// Asking a server for its tools starts it, and the other three MCP tools were
// gated; this one was ReadOnly and answered with whatever the process did, so
// under a policy that refuses commands it still ran one. The fixture here has
// no task checked out, which is a command policy that refuses every command.
func TestMCPListToolsIsGatedBeforeItStartsAServer(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"every server":   {},
		"a named server": {"server_name": "demo"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixtureRunner(t, &recordingRunner{})
			marker := wireMCPStub(t, f)

			res := f.call("mcp_list_tools", args)
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("mcp_list_tools started a server although the gate had not allowed it")
			}
			if name == "a named server" {
				if !res.IsError || !res.Blocked || res.Rule == "" {
					t.Fatalf("the refusal is not a recorded gate decision: %+v", res)
				}
				if !strings.Contains(res.Text, "mcp_list_tools demo") {
					t.Errorf("the decision does not name what was judged: %s", res.Text)
				}
				return
			}
			// The survey as a whole is not an error: the refused server is in it,
			// with the refusal as its reason, so it is not mistaken for a server
			// that has no tools.
			if !strings.Contains(res.Text, "demo") || !strings.Contains(res.Text, "mcp_list_tools demo") {
				t.Errorf("the refused server is not reported with its refusal: %s", res.Text)
			}
		})
	}
}

// And a permitted listing still runs, with the decision on the result.
func TestAPermittedMCPListToolsStillRunsAndCarriesItsQualification(t *testing.T) {
	f := newFixtureFull(t, map[string]string{flags.HarnessPosture: flags.PostureYolo}, &recordingRunner{})
	marker := wireMCPStub(t, f)

	res := f.call("mcp_list_tools", map[string]any{"server_name": "demo"})
	if res.IsError || res.Blocked {
		t.Fatalf("a permitted listing was refused: %+v", res)
	}
	if !strings.Contains(res.Text, "run_shell") {
		t.Errorf("the server's tools did not reach the caller: %s", res.Text)
	}
	if res.Demoted == "" {
		t.Errorf("the allow is indistinguishable from one the rules produced: %+v", res)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the permitted listing never reached the server: %v", err)
	}
}
