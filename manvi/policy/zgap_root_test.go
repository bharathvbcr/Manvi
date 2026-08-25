package policy

import (
	"testing"

	"manvi/dc"
)

// --- E4: an absolute .venv/bin/dev outside the repo was laundered ------------
//
// Normalisation is a claim that two spellings name the same program, and the
// string that actually gets executed is the un-normalised one. So every
// spelling accepted here inherits `dev`'s allowlist entries. The relative and
// bare forms were tightened earlier; the absolute virtualenv layout was left,
// because telling /repo/.venv/bin/dev from /tmp/attacker/.venv/bin/dev needs
// the working tree and the signature did not carry it. It does now.

const testRoot = "/repo"

func TestAnAbsoluteVenvDevOutsideTheRootIsNotThisReposCLI(t *testing.T) {
	foreign := []string{
		"/tmp/attacker/.venv/bin/dev status",
		"/tmp/attacker/venv/bin/dev run-cmd anything",
		"/tmp/attacker/.venv/Scripts/dev.exe status",
		// The prefix trap: "/repo2" starts with "/repo" and is a different
		// directory. A string-prefix containment check accepts it.
		"/repo2/.venv/bin/dev status",
		"/repository-of-someone-else/.venv/bin/dev status",
		// Climbing out of the root and back down again.
		"/repo/../tmp/attacker/.venv/bin/dev status",
	}
	gate := CommandGate{Root: testRoot, HardRules: true}
	task := &dc.Task{ID: "TASK-001", AllowedCommands: []string{"dev *"}}
	for _, cmd := range foreign {
		if got := NormalizeAllowlistCommandInRoot(cmd, testRoot); got != collapseSpaces(cmd) {
			t.Errorf("NormalizeAllowlistCommandInRoot(%q, %q) = %q; a binary outside the working "+
				"tree was rewritten into this repo's dev CLI", cmd, testRoot, got)
		}
		if d := gate.EvaluateCommand(cmd, task); d.Action != Deny {
			t.Errorf("EvaluateCommand(%q) = %v (%s); the allowlist was reached through a "+
				"laundered path", cmd, d.Action, d.Reason)
		}
	}
}

// The other half: the fix must not simply stop normalising absolute paths, or
// every venv-installed CLI invoked by its full path breaks.
func TestAnAbsoluteVenvDevInsideTheRootIsStillThisReposCLI(t *testing.T) {
	ours := map[string]string{
		"/repo/.venv/bin/dev status":        "dev status",
		"/repo/.venv/bin/dev map query x":   "dev map query x",
		"/repo/venv/bin/devcouncil status":  "dev status",
		"/repo/.venv/Scripts/dev.exe map":   "dev map",
		"/repo/nested/.venv/bin/dev status": "dev status",
	}
	for cmd, want := range ours {
		if got := NormalizeAllowlistCommandInRoot(cmd, testRoot); got != want {
			t.Errorf("NormalizeAllowlistCommandInRoot(%q, %q) = %q, want %q",
				cmd, testRoot, got, want)
		}
	}
	// A trailing separator on the root is the same root.
	if got := NormalizeAllowlistCommandInRoot("/repo/.venv/bin/dev status", "/repo/"); got != "dev status" {
		t.Errorf("a root with a trailing separator was treated as a different tree: %q", got)
	}
	// And it reaches the allowlist as `dev`, which is the whole point.
	gate := CommandGate{Root: testRoot, HardRules: true}
	task := &dc.Task{ID: "TASK-001", AllowedCommands: []string{}}
	if d := gate.EvaluateCommand("/repo/.venv/bin/dev map", task); d.Action == Deny {
		t.Errorf("this repo's own venv CLI was denied: %v (%s)", d.Action, d.Reason)
	}
}

// An unknown root must not launder. A gate built without a Root has no tree to
// compare against, and the only answer that is not a guess is "no absolute
// path is this repository's" — stricter, never looser.
func TestAnUnknownRootLaundersNoAbsolutePath(t *testing.T) {
	cmds := []string{
		"/repo/.venv/bin/dev status",
		"/tmp/attacker/.venv/bin/dev status",
	}
	for _, cmd := range cmds {
		if got := NormalizeAllowlistCommand(cmd); got != collapseSpaces(cmd) {
			t.Errorf("NormalizeAllowlistCommand(%q) = %q; an unknown root laundered an "+
				"absolute path", cmd, got)
		}
		if got := NormalizeAllowlistCommandInRoot(cmd, ""); got != collapseSpaces(cmd) {
			t.Errorf("an empty root laundered %q into %q", cmd, got)
		}
		// A relative root cannot decide containment for an absolute token, and
		// resolving it against the process's cwd would make the verdict depend
		// on where the harness was started.
		if got := NormalizeAllowlistCommandInRoot(cmd, "repo"); got != collapseSpaces(cmd) {
			t.Errorf("a relative root laundered %q into %q", cmd, got)
		}
	}
	gate := CommandGate{HardRules: true}
	task := &dc.Task{ID: "TASK-001", AllowedCommands: []string{"dev *"}}
	if d := gate.EvaluateCommand("/tmp/attacker/.venv/bin/dev status", task); d.Action != Deny {
		t.Errorf("a rootless gate allowed a foreign venv binary: %v (%s)", d.Action, d.Reason)
	}
}

// Naming the root must not widen anything else. The relative and bare forms
// keep the verdicts the earlier hardening gave them, root or no root.
func TestNamingTheRootDoesNotWidenTheOtherSpellings(t *testing.T) {
	for _, root := range []string{"", testRoot} {
		for _, cmd := range []string{
			"/tmp/attacker/bin/dev status",
			"../../../../tmp/attacker/bin/dev status",
			"attacker/scripts/dev run-cmd anything",
			"/bin/dev status",
			`attacker\bin\dev status`,
			"/repo/bin/dev status",
		} {
			if got := NormalizeAllowlistCommandInRoot(cmd, root); got != collapseSpaces(cmd) {
				t.Errorf("root %q: NormalizeAllowlistCommandInRoot(%q) = %q; a foreign path was "+
					"rewritten", root, cmd, got)
			}
		}
		for cmd, want := range map[string]string{
			"dev status":                 "dev status",
			"devcouncil status":          "dev status",
			".venv/bin/dev map":          "dev map",
			"./scripts/dev map":          "dev map",
			"bin/dev map":                "dev map",
			"uv run --project . dev map": "uv run dev map",
			"DevCouncil/dev map":         "DevCouncil/dev map",
		} {
			if got := NormalizeAllowlistCommandInRoot(cmd, root); got != want {
				t.Errorf("root %q: NormalizeAllowlistCommandInRoot(%q) = %q, want %q",
					root, cmd, got, want)
			}
		}
	}
}
