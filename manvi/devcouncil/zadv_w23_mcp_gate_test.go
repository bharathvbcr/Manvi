package devcouncil

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// Adversarial gate tests for mcp_list_tools. The fixture checks a task out, so
// the refusals below come from the command gate itself (strict posture, no
// allowed command names an MCP listing) and not from the earlier
// "no task is checked out" refusal.

func zadvDenyAllFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixtureRunner(t, &recordingRunner{})
	if out := f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"}); out["acquired"] != true {
		t.Fatalf("setup: checkout = %v", out)
	}
	return f, wireMCPStub(t, f)
}

func zadvWaitFor(t *testing.T, path string) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
	}
	t.Fatalf("%s never appeared", path)
}

func TestZadvW23ListToolsUnderADenyAllGateStartsNothingAndListsNothing(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"named server": {"server_name": "demo"},
		"every server": {},
	} {
		t.Run(name, func(t *testing.T) {
			f, marker := zadvDenyAllFixture(t)
			res := f.call("mcp_list_tools", args)
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("a server was started although the gate denied the listing")
			}
			if strings.Contains(res.Text, "run_shell") {
				t.Fatalf("the server's tools were listed under a deny-all gate: %s", res.Text)
			}
			if name == "named server" && (!res.IsError || !res.Blocked || res.Rule == "") {
				t.Fatalf("the refusal is not a recorded gate decision: %+v", res)
			}
		})
	}
}

// A server that was already running when the policy turned against listing is
// still refused: the gate is consulted whether or not Client has to start it.
func TestZadvW23AnAlreadyRunningServerIsNotListedWithoutAGateDecision(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"named server": {"server_name": "demo"},
		"every server": {},
	} {
		t.Run(name, func(t *testing.T) {
			f, marker := zadvDenyAllFixture(t)
			if _, err := f.reg.deps.MCP.Client(t.Context(), "demo"); err != nil {
				t.Fatal(err)
			}
			zadvWaitFor(t, marker)
			res := f.call("mcp_list_tools", args)
			if strings.Contains(res.Text, "run_shell") {
				t.Fatalf("a running server was listed with no gate decision: %s", res.Text)
			}
		})
	}
}

// A declined escalation is a refusal, and an accepted one names the operation
// it was asked about, once, and lets exactly that listing through.
func TestZadvW23EscalationForAListingAsksAboutTheListingAndNothingElse(t *testing.T) {
	t.Run("declined", func(t *testing.T) {
		f, marker := zadvDenyAllFixture(t)
		ap := &recordingApprover{decision: ui.Decision{Allow: false, Reason: "no"}}
		f.withApprover(ap)
		res := f.call("mcp_list_tools", map[string]any{"server_name": "demo"})
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("a declined listing started the server")
		}
		if !res.IsError || strings.Contains(res.Text, "run_shell") {
			t.Fatalf("a declined escalation was not a refusal: %+v", res)
		}
	})
	t.Run("approved", func(t *testing.T) {
		f, marker := zadvDenyAllFixture(t)
		ap := &recordingApprover{decision: ui.Decision{Allow: true, Reason: "operator reviewed", By: "test"}}
		f.withApprover(ap)
		res := f.call("mcp_list_tools", map[string]any{"server_name": "demo"})
		if res.IsError || !strings.Contains(res.Text, "run_shell") {
			t.Fatalf("an approved listing did not run: %+v", res)
		}
		zadvWaitFor(t, marker)
		if len(ap.asked) != 1 || !strings.Contains(ap.asked[0].Path, "mcp_list_tools demo") {
			t.Fatalf("the approver was asked %+v, want one request naming %q", ap.asked, "mcp_list_tools demo")
		}
		if res.Demoted == "" && len(res.Degraded) == 0 && res.GrantID == "" {
			t.Errorf("an escalated allow is indistinguishable from a plain one: %+v", res)
		}
	})
}
