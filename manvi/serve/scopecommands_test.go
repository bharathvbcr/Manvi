package serve

import (
	"encoding/json"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
)

// gitpulseScope is the scope GitPulse sends, field for field: its Rust
// HostScope (src-tauri/src/harness/policy.rs) serialises all five, always.
func gitpulseScope(root string, allowed ...string) map[string]any {
	if allowed == nil {
		allowed = []string{}
	}
	return map[string]any{
		"task_id": "task-1", "planned_files": []string{"src/lib.rs"}, "forbidden_changes": []string{},
		"worktree": root, "allowed_commands": allowed,
	}
}

func rawRequest(t *testing.T, op string, params map[string]any) Request {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return Request{ID: "1", Op: op, Params: raw}
}

// A bound task's allowed_commands arrive inside the scope, because that is
// where the host keeps them. They used to be decoded into a struct with no
// field for them and dropped, so a command the task authorised came back
// command.not_allowed — refused with the allowlist enforced, demoted without.
func TestTheScopesAllowedCommandsAuthoriseACommand(t *testing.T) {
	root := t.TempDir()
	for _, enforce := range []bool{true, false} {
		d := decodeDecision(t, roundTrip(t, hostOpts(), rawRequest(t, OpPolicyCheckCommand, map[string]any{
			"command": "cargo test --workspace", "root": root,
			"scope": gitpulseScope(root, "cargo test*"), "enforce_allowlist": enforce,
		}))[0])
		if d.Action != policy.Allow || d.Demoted != "" {
			t.Errorf("enforce=%v: a command the task authorised got action=%q rule=%q demoted=%q",
				enforce, d.Action, d.Rule, d.Demoted)
		}
		if d.TaskID != "task-1" {
			t.Errorf("enforce=%v: decision names task %q, want the scope's task-1", enforce, d.TaskID)
		}
	}
}

// The scope's allowlist is a statement of intent and widens nothing a hard
// rule refuses: those rungs run before any allowlist is consulted.
func TestTheScopesAllowedCommandsCannotLiftAHardRule(t *testing.T) {
	root := t.TempDir()
	d := decodeDecision(t, roundTrip(t, hostOpts(), rawRequest(t, OpPolicyCheckCommand, map[string]any{
		"command": "git push --force origin main", "root": root,
		"scope": gitpulseScope(root, "git push*", "*"), "enforce_allowlist": true,
	}))[0])
	if d.Action != policy.Deny || d.Severity != policy.Hard {
		t.Fatalf("a force push was let through by a scope allowlist: action=%q severity=%q rule=%q", d.Action, d.Severity, d.Rule)
	}
}

// Both allowlists count: the top-level one a host declares for itself and the
// one its task declares.
func TestTopLevelAndScopeAllowlistsCombine(t *testing.T) {
	root := t.TempDir()
	for _, command := range []string{"go vet ./...", "cargo test"} {
		d := decodeDecision(t, roundTrip(t, hostOpts(), rawRequest(t, OpPolicyCheckCommand, map[string]any{
			"command": command, "root": root, "allowed_commands": []string{"go vet *"},
			"scope": gitpulseScope(root, "cargo test*"), "enforce_allowlist": true,
		}))[0])
		if d.Action != policy.Allow {
			t.Errorf("%q: action=%q rule=%q, want allowed by one of the two lists", command, d.Action, d.Rule)
		}
	}
}

// The exact requests GitPulse sends decode and are answered.
func TestGitPulsesPolicyRequestsDecode(t *testing.T) {
	root := t.TempDir()
	for _, req := range []Request{
		rawRequest(t, OpPolicyCheckFile, map[string]any{"root": root, "path": "src/lib.rs", "op": "modify", "scope": gitpulseScope(root)}),
		rawRequest(t, OpPolicyCheckCommand, map[string]any{"command": "cargo build", "root": root, "scope": gitpulseScope(root)}),
		rawRequest(t, OpPolicyCheckCommand, map[string]any{"command": "cargo build", "root": root, "allowed_commands": []string{"cargo *"}}),
	} {
		if resp := roundTrip(t, hostOpts(), req)[0]; !resp.OK {
			t.Errorf("%s: GitPulse's request was refused: %+v", req.Op, resp.Error)
		}
	}
}
