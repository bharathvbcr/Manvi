package devcouncil

import (
	"os"
	"strings"
	"testing"

	"manvi/flags"
)

// --- E2: the two resource tools were ungated ---------------------------------
//
// mcp_call_tool was gated; mcp_list_resources and mcp_read_resource were not.
// The argument for leaving them alone was that after the trust fix they can
// only reach a server an operator authorized — but an authorized server is
// still a separate program the harness does not control, and a resource read
// returns bytes of that program's choosing straight into the model's context.
// Both also reach mgr.Client, which spawns the server process: the effect the
// gate exists to decide about happened before any gate was consulted.

// A refusal has to arrive as a refusal, and nothing may run first.
func TestMCPResourceToolsAreGatedAndRefuseWithoutApproval(t *testing.T) {
	for _, tc := range []struct {
		tool   string
		args   map[string]any
		target string
	}{
		{"mcp_list_resources", map[string]any{"server_name": "demo"},
			"mcp_list_resources demo"},
		{"mcp_read_resource", map[string]any{"server_name": "demo", "uri": "file:///secret.txt"},
			"mcp_read_resource demo file:///secret.txt"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			f := newFixtureRunner(t, &recordingRunner{})
			marker := wireMCPStub(t, f)

			res := f.call(tc.tool, tc.args)
			if !res.IsError {
				t.Fatalf("an ungated %s succeeded: %s", tc.tool, res.Text)
			}
			if !res.Blocked {
				t.Errorf("the refusal did not set Result.Blocked, so the run report cannot tell "+
					"a gate refusal from any other failure: %+v", res)
			}
			if res.Rule == "" {
				t.Errorf("the refusal names no rule: %+v", res)
			}
			if !strings.Contains(res.Text, tc.target) {
				t.Errorf("the decision does not name what was judged (want %q): %s",
					tc.target, res.Text)
			}
			// And nothing ran: a gate that refuses after the server has been
			// spawned with the harness's environment is not a gate.
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the MCP server was spawned despite the gate refusing")
			}
		})
	}
}

// A permitted listing must still work, or the gate is a blanket refusal rather
// than a gate. Under yolo the command mode is off, which demotes the soft
// denial — and the demotion has to travel onto the result.
func TestAPermittedMCPResourceListingStillRunsAndCarriesItsQualification(t *testing.T) {
	f := newFixtureFull(t, map[string]string{flags.HarnessPosture: flags.PostureYolo},
		&recordingRunner{})
	marker := wireMCPStub(t, f)

	res := f.call("mcp_list_resources", map[string]any{"server_name": "demo"})
	if res.IsError {
		t.Fatalf("a permitted resource listing was refused: %s", res.Text)
	}
	if res.Blocked {
		t.Errorf("a permitted listing reported itself blocked: %+v", res)
	}
	if res.Demoted == "" {
		t.Errorf("an allow produced by a gate mode is indistinguishable from one the rules "+
			"produced: %+v", res)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the permitted listing never reached the server: %v", err)
	}
}

// The resource URI reaches this surface from the model or from a server's own
// listing, so it is attacker-influenced on both routes. The gate splits its
// subject into a command chain before judging it, so a URI carrying ";", "&&",
// a newline or "$(" would be split into parts that were never one operation:
// the thing judged would not be the thing read.
func TestMCPResourceURIsCarryingShellMetacharactersAreRefused(t *testing.T) {
	f := newFixtureRunner(t, &recordingRunner{})
	marker := wireMCPStub(t, f)

	for _, bad := range []string{
		"file:///x; rm -rf /",
		"file:///x && echo pwned",
		"file:///x\nmcp_read_resource demo file:///y",
		"file:///x$(whoami)",
		"file:///x`id`",
		"file:///x | cat",
		strings.Repeat("f", maxMCPResourceURI+1),
	} {
		res := f.call("mcp_read_resource", map[string]any{
			"server_name": "demo", "uri": bad,
		})
		if !res.IsError {
			t.Errorf("uri %q was accepted: %s", bad, res.Text)
		}
	}
	// A server name is restricted the same way the tool-call path restricts it.
	for _, bad := range []string{"demo; git status", "demo\nother", "demo$(whoami)"} {
		for _, call := range []struct {
			tool string
			args map[string]any
		}{
			{"mcp_list_resources", map[string]any{"server_name": bad}},
			{"mcp_read_resource", map[string]any{"server_name": bad, "uri": "file:///x"}},
		} {
			if res := f.call(call.tool, call.args); !res.IsError {
				t.Errorf("%s accepted server_name %q: %s", call.tool, bad, res.Text)
			}
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a call with an unusable name or uri still reached a server")
	}
}

// A harness built without a gate must not read resources. A check that could
// not run has to be an error, never the answer a check that ran and passed
// gives.
func TestMCPResourceToolsWithNoGateRefuseRatherThanReading(t *testing.T) {
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"mcp_list_resources", map[string]any{"server_name": "demo"}},
		{"mcp_read_resource", map[string]any{"server_name": "demo", "uri": "file:///x"}},
	} {
		t.Run(call.tool, func(t *testing.T) {
			f := newFixtureRunner(t, &recordingRunner{})
			marker := wireMCPStub(t, f)
			f.reg.deps.Gate = nil

			res := f.call(call.tool, call.args)
			if !res.IsError {
				t.Fatalf("%s answered with no policy gate present: %s", call.tool, res.Text)
			}
			if !strings.Contains(res.Text, "the check did not run") {
				t.Errorf("the refusal reads like a negative result rather than an absent check: %s",
					res.Text)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the MCP server was spawned with no gate to judge the call")
			}
		})
	}
}

// The gate must not displace the configuration fault. A surface with no manager
// has a Deps field to populate, not a policy to change, and the two refusals
// must stay distinguishable — mcpdeps_test pins that text, and this pins the
// ordering that keeps it reachable now that a gate call sits on the same path.
func TestAMissingManagerIsReportedBeforeThePolicyRefusal(t *testing.T) {
	f := newFixtureRunner(t, &recordingRunner{})
	if f.reg.deps.MCP != nil {
		t.Fatal("the fixture wired an MCP manager; this test no longer proves anything")
	}
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"mcp_list_resources", map[string]any{"server_name": "demo"}},
		{"mcp_read_resource", map[string]any{"server_name": "demo", "uri": "file:///x"}},
	} {
		res := f.call(call.tool, call.args)
		if !res.IsError {
			t.Fatalf("%s answered without a manager: %s", call.tool, res.Text)
		}
		if !strings.Contains(res.Text, "Deps.MCP is nil") {
			t.Errorf("%s refused without naming the cause: %s", call.tool, res.Text)
		}
	}
}
