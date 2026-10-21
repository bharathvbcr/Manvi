package serve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
)

// Adversarial (dh-4-adversarial) tests of the serve plane's side of the
// prompt-input rung. They need the DevCouncil patch (see the pin note in
// zsec_prompt_inputs_test.go) and are red without it.

// The trust boundary of the Internal exemption, stated as a characterisation:
// `internal` is a field of the wire request, decoded from whatever JSON the peer
// on the stdio pair sends, and it lifts the prompt-input rung for every path and
// every posture. The exemption is therefore exactly as strong as the host's
// control of that pipe. This test pins the CURRENT behaviour so a change to it
// (narrowing internal to tool-state paths, say) is a visible decision.
func TestAdvInternalIsWireControlledAndLiftsThePromptInputRung(t *testing.T) {
	for _, hard := range []bool{true, false} {
		opts := hostOpts()
		opts.HardRules = hard
		root := t.TempDir()
		raw := `{"id":"1","op":"policy.check.file","params":{"root":` + jsonString(root) + `,"path":"pkg/deep/AGENTS.md","internal":true}}` + "\n"
		resp := runLines(t, opts, raw)
		d := decodeDecision(t, resp[0])
		if d.Blocked() {
			t.Fatalf("hard=%v: raw internal=true write to a nested AGENTS.md = %s/%s; the characterisation changed", hard, d.Action, d.Rule)
		}
		// The secret rung is not exempted by internal: it must still hold.
		if hard {
			raw = `{"id":"2","op":"policy.check.file","params":{"root":` + jsonString(root) + `,"path":".env","internal":true}}` + "\n"
			if e := decodeDecision(t, runLines(t, opts, raw)[0]); !e.Blocked() || e.Rule != policy.RuleSecretPath {
				t.Errorf("internal=true must not lift the secret rung: %s/%s", e.Action, e.Rule)
			}
		}
	}
}

// policy.check.command has no internal field and passes false for every
// redirection target, so a command can never ride the exemption, even with an
// `internal` key smuggled into its params.
func TestAdvCommandPlaneNeverHonoursInternal(t *testing.T) {
	root := t.TempDir()
	for _, hard := range []bool{true, false} {
		opts := hostOpts()
		opts.HardRules = hard
		for _, cmd := range []string{"echo x > AGENTS.md", "echo x | tee pkg/CLAUDE.md", "cp a .mcp.json", "rm AGENTS.md"} {
			raw := `{"id":"1","op":"policy.check.command","params":{"command":` + jsonString(cmd) + `,"root":` + jsonString(root) + `,"internal":true}}` + "\n"
			d := decodeDecision(t, runLines(t, opts, raw)[0])
			if !d.Blocked() || d.Rule != policy.RuleRestrictedPath {
				t.Errorf("hard=%v %q = %s/%s, want blocked by %s", hard, cmd, d.Action, d.Rule, policy.RuleRestrictedPath)
			}
		}
	}
}

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// A hard link to a prompt input is the same inode under another name. The rung
// matches the name and the resolved name, never the identity, so a write to the
// link is not seen.
func TestAdvHardLinkToPromptInputIsRefused(t *testing.T) {
	for _, hard := range []bool{true, false} {
		opts := hostOpts()
		opts.HardRules = hard
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("rules"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(root, "AGENTS.md"), filepath.Join(root, "notes.txt")); err != nil {
			t.Skipf("hard links unavailable: %v", err)
		}
		d := decodeDecision(t, roundTrip(t, opts, internalFileCheck(t, "1", root, "notes.txt", false))[0])
		if d.Action != policy.Deny || d.Rule != policy.RuleRestrictedPath {
			t.Errorf("BROKEN hard=%v: write through a hard link to AGENTS.md = %s/%s", hard, d.Action, d.Rule)
		}
	}
}

// A repository that keeps AGENTS.md as a symlink to another file: the write
// resolves to the target, whose name is not a prompt input.
func TestAdvPromptInputSymlinkedToOrdinaryFileIsRefused(t *testing.T) {
	for _, hard := range []bool{true, false} {
		opts := hostOpts()
		opts.HardRules = hard
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "docs", "rules.md"), []byte("rules"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("docs/rules.md", filepath.Join(root, "AGENTS.md")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		d := decodeDecision(t, roundTrip(t, opts, internalFileCheck(t, "1", root, "AGENTS.md", false))[0])
		if d.Action != policy.Deny || d.Rule != policy.RuleRestrictedPath {
			t.Errorf("BROKEN hard=%v: write to AGENTS.md -> docs/rules.md = %s/%s", hard, d.Action, d.Rule)
		}
	}
}
