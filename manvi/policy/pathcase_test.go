package policy

import (
	"testing"

	"manvi/dc"
)

// The rungs that are statements about *files* have to read a path the way the
// filesystem does. Three of them did — the secret rung, the restricted rung and
// forbidden_changes all fold — and the protected-write rung did not, so on the
// case-insensitive filesystems this harness ships on the flag simply vanished
// for the spelling an agent would reach for to make it vanish.
func TestTheProtectedWriteRungFoldsCase(t *testing.T) {
	for _, spelling := range []string{
		"package.json", "PACKAGE.JSON", "Package.JSON",
		"Dockerfile", "dockerfile", "DOCKERFILE",
		"docker-compose.yml", "DOCKER-COMPOSE.YML",
		"pnpm-lock.yaml", "PNPM-LOCK.YAML",
	} {
		g := FileGate{Root: t.TempDir(), HardRules: true}
		d := g.EvaluateFileChange(spelling, planTask(modify(spelling)), dc.OpWrite, false)
		if d.Rule != RuleProtectedWrite {
			t.Errorf("%q was judged %s/%q, want the protected-write flag: on APFS this is the same "+
				"file as its lower-cased twin", spelling, d.Action, d.Rule)
		}
	}
	// The rung still means something: an ordinary file is not flagged.
	g := FileGate{Root: t.TempDir(), HardRules: true}
	if d := g.EvaluateFileChange("src/calc.go", planTask(modify("src/calc.go")), dc.OpWrite, false); d.Rule != RuleNone {
		t.Errorf("an ordinary write was flagged %q: %+v", d.Rule, d)
	}
}

// The restricted rung's prefix half tested characters, and a path is made of
// components. ".git" is a character prefix of ".gitignore" and ".github/...",
// which the rung denied Hard — and because it runs before the protected-write
// rung, the two ".github/workflows/*" entries in ProtectedWritePatterns could
// never fire, so the "allowed, but flagged" behaviour they document did not
// exist. In the other direction the character test needed a trailing "/" to
// reach a subtree, which meant it never covered the directory itself, and only
// ".git" and ".devcouncil" had bare entries added by hand to cover for that.
func TestTheRestrictedRungMatchesWholeComponents(t *testing.T) {
	g := FileGate{Root: t.TempDir(), HardRules: true}

	for _, path := range []string{
		".git", ".git/config", ".GIT", ".Git/config",
		".devcouncil", ".devcouncil/state.json",
		".claude", ".claude/settings.json", ".CLAUDE",
		".codex", ".cursor", ".gemini", ".grok", ".opencode", ".agents",
		".codex/config.toml", ".agents/x.md",
		"opencode.json",
	} {
		d := g.EvaluateFileChange(path, planTask(modify(path)), dc.OpWrite, false)
		if d.Rule != RuleRestrictedPath {
			t.Errorf("%q was judged %s/%q, want path.restricted: a bare agent-config name is the "+
				"file that pre-empts the directory this rung protects", path, d.Action, d.Rule)
		}
	}

	// Neighbours of a restricted name that are not under it. These are ordinary
	// repository files; denying them Hard was over-blocking, and it is what made
	// the protected-write entries unreachable.
	for _, path := range []string{
		".gitignore", ".gitattributes", ".gitmodules",
		".github/CODEOWNERS", ".devcouncilx", "opencode.json.bak", ".claudex",
	} {
		d := g.EvaluateFileChange(path, planTask(modify(path)), dc.OpWrite, false)
		if d.Rule == RuleRestrictedPath {
			t.Errorf("%q was denied as repository machinery; it is an ordinary tracked file", path)
		}
	}
}

// The two entries the restricted rung had made unreachable, exercised through
// the ladder: a workflow edit is allowed and flagged, which is what
// ProtectedWritePatterns says about it.
func TestWorkflowEditsAreFlaggedRatherThanRefused(t *testing.T) {
	g := FileGate{Root: t.TempDir(), HardRules: true}
	for _, path := range []string{
		".github/workflows/ci.yml",
		".github/workflows/release.yaml",
		".GITHUB/WORKFLOWS/CI.YML",
	} {
		d := g.EvaluateFileChange(path, planTask(modify(path)), dc.OpWrite, false)
		if d.Rule != RuleProtectedWrite {
			t.Errorf("%q was judged %s/%q, want path.protected_write — the entry in "+
				"ProtectedWritePatterns has to be reachable to mean anything", path, d.Action, d.Rule)
		}
	}
	// Everything genuinely inside .git stays hard-denied.
	if d := g.EvaluateFileChange(".git/hooks/pre-commit", planTask(modify(".git/hooks/pre-commit")),
		dc.OpWrite, false); d.Rule != RuleRestrictedPath {
		t.Errorf("a hook write was judged %q, want path.restricted", d.Rule)
	}
}
