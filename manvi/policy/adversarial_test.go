package policy

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"manvi/dc"
)

// gateFor builds a hard-rules-on gate rooted at a real temp directory.
func gateFor(t *testing.T) (FileGate, string) {
	t.Helper()
	root := t.TempDir()
	// t.TempDir on macOS lives under /var -> /private/var, a symlink. Resolving
	// it here means the test compares like with like.
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return FileGate{Root: resolved, HardRules: true, AllowNeighbors: true}, resolved
}

func plannedTask(paths ...string) *dc.Task {
	task := &dc.Task{ID: "TASK-1"}
	for _, p := range paths {
		task.PlannedFiles = append(task.PlannedFiles, dc.PlannedFile{Path: p, AllowedChange: dc.ChangeModify})
	}
	return task
}

// TestSecretPathsResistCaseVariation is a filesystem-reality attack. On macOS
// (APFS, case-insensitive by default) and on Windows, writing ".ENV" writes to
// ".env". A case-sensitive pattern match is therefore not a check of what the
// filesystem will do.
func TestSecretPathsResistCaseVariation(t *testing.T) {
	g, _ := gateFor(t)
	task := plannedTask("**")
	for _, path := range []string{".ENV", ".Env", ".env.PRODUCTION", "config/CREDENTIALS/aws", "keys/server.PEM", "ID_RSA", ".NPMRC"} {
		d := g.EvaluateFileChange(path, task, dc.OpWrite, false)
		if d.Rule != RuleSecretPath {
			t.Errorf("%q: rule = %q action = %q, want the secret-path rule to fire", path, d.Rule, d.Action)
		}
	}
}

// TestRestrictedPathsResistCaseVariation is the same attack on the agent
// client configs. ".CLAUDE/settings.json" is the same file as
// ".claude/settings.json" on this machine.
func TestRestrictedPathsResistCaseVariation(t *testing.T) {
	g, _ := gateFor(t)
	task := plannedTask("**")
	for _, path := range []string{".GIT/config", ".Claude/settings.json", ".DevCouncil/state.json", "OPENCODE.JSON"} {
		d := g.EvaluateFileChange(path, task, dc.OpWrite, false)
		if d.Rule != RuleRestrictedPath {
			t.Errorf("%q: rule = %q action = %q, want the restricted-path rule to fire", path, d.Rule, d.Action)
		}
	}
}

// TestBareGitFileIsRestricted covers the git worktree and submodule case: there
// ".git" is a *file* containing "gitdir: ...". Rewriting it repoints the
// repository, which is exactly what the restricted rung exists to prevent.
func TestBareGitFileIsRestricted(t *testing.T) {
	g, _ := gateFor(t)
	d := g.EvaluateFileChange(".git", plannedTask("**"), dc.OpWrite, false)
	if d.Rule != RuleRestrictedPath {
		t.Fatalf("rule = %q action = %q, want the restricted rule for a bare .git", d.Rule, d.Action)
	}
}

// TestTraversalInsideRepoStillNormalizes checks that a ".." segment cannot be
// used to reach a secret path by a route the pattern set does not literally list.
func TestTraversalInsideRepoStillNormalizes(t *testing.T) {
	g, _ := gateFor(t)
	task := plannedTask("**")
	for _, path := range []string{"src/../.env", "./src/./../.env", "a/b/../../.env"} {
		d := g.EvaluateFileChange(path, task, dc.OpWrite, false)
		if d.Rule != RuleSecretPath {
			t.Errorf("%q: rule = %q, want the secret rule after normalization (target=%q)", path, d.Rule, d.Target)
		}
	}
}

// TestSymlinkEscapeIsOutsideRoot is the classic containment attack: a symlink
// inside the repo pointing at a directory outside it.
func TestSymlinkEscapeIsOutsideRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ")
	}
	g, root := gateFor(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	d := g.EvaluateFileChange("escape/loot.txt", plannedTask("**"), dc.OpWrite, false)
	if d.Rule != RuleOutsideRoot {
		t.Fatalf("rule = %q target = %q, want outside-root through a symlink", d.Rule, d.Target)
	}
}

// TestSymlinkedFileEscapeIsOutsideRoot covers the narrower case where the leaf
// itself is the symlink, so the parent directory resolves cleanly.
func TestSymlinkedFileEscapeIsOutsideRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ")
	}
	g, root := gateFor(t)
	outside := filepath.Join(t.TempDir(), "target.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	d := g.EvaluateFileChange("link.txt", plannedTask("**"), dc.OpWrite, false)
	if d.Rule != RuleOutsideRoot {
		t.Fatalf("rule = %q target = %q, want outside-root for a symlinked leaf", d.Rule, d.Target)
	}
}

// TestEmptyAndDegeneratePathsFailClosed: a path the gate cannot make sense of
// must never come back as an allow.
func TestEmptyAndDegeneratePathsFailClosed(t *testing.T) {
	g, _ := gateFor(t)
	task := plannedTask("**")
	// A lone `"` is deliberately absent: it is a legal filename, and it used to
	// be denied only as a side effect of the normalizer stripping quotes from
	// either end independently. That was a bug — it also mangled real paths
	// ending in a quote — and denying a valid name was never the intent.
	for _, path := range []string{"", "   ", ".", "./", "..", "/"} {
		d := g.EvaluateFileChange(path, task, dc.OpWrite, false)
		if d.Action == Allow {
			t.Errorf("%q: action = allow (rule %q, target %q), want a denial", path, d.Rule, d.Target)
		}
	}
}

// TestNulByteInPathIsRefused: a NUL truncates the path at every syscall
// boundary, so "src/a.go\x00/../../.env" is a different file to the kernel than
// it is to the matcher.
func TestNulByteInPathIsRefused(t *testing.T) {
	g, _ := gateFor(t)
	d := g.EvaluateFileChange("src/a.go\x00.env", plannedTask("**"), dc.OpWrite, false)
	if d.Rule != RuleMalformedPath {
		t.Fatalf("rule = %q action = %q, want the malformed-path rule so the diagnostic names the real problem", d.Rule, d.Action)
	}
	// Defence in depth: the normalizer refuses it too, for the other caller
	// that does not run the ladder.
	if _, outside := NormalizeRepoPath(t.TempDir(), "a\x00b"); !outside {
		t.Fatal("NormalizeRepoPath accepted a NUL-bearing path as contained")
	}
}

// TestPlannedGlobCannotAuthorizeASecret re-asserts ladder order under the most
// hostile plan available: a task that plans literally everything.
func TestPlannedGlobCannotAuthorizeASecret(t *testing.T) {
	g, _ := gateFor(t)
	for _, plan := range []string{"**", "*", ".env", "**/*"} {
		d := g.EvaluateFileChange(".env", plannedTask(plan), dc.OpWrite, false)
		if d.Rule != RuleSecretPath {
			t.Errorf("plan %q: rule = %q, want the secret rung to run before the task rung", plan, d.Rule)
		}
	}
}

// TestBackslashPathsNormalize: a Windows-style separator must not route around
// a forward-slash pattern.
func TestBackslashPathsNormalize(t *testing.T) {
	g, _ := gateFor(t)
	d := g.EvaluateFileChange(`.claude\settings.json`, plannedTask("**"), dc.OpWrite, false)
	if d.Rule != RuleRestrictedPath {
		t.Fatalf("rule = %q target = %q, want restricted after separator normalization", d.Rule, d.Target)
	}
}

// TestDegradedIsNeverEmptyWhenTheMapIsMissing is the governing invariant in its
// narrowest form: a check that could not run must not look like one that ran.
func TestDegradedIsNeverEmptyWhenTheMapIsMissing(t *testing.T) {
	g, _ := gateFor(t)
	g.Subsystems = nil
	d := g.EvaluateFileChange("src/unplanned.go", plannedTask("docs/x.md"), dc.OpWrite, false)
	if d.Clean() {
		t.Fatal("a decision reached without the repo map must not read as clean")
	}
	if len(d.Degraded) == 0 || !strings.Contains(strings.Join(d.Degraded, ","), "repo_map") {
		t.Fatalf("degraded = %v, want the missing repo map named", d.Degraded)
	}
}
