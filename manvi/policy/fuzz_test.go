package policy

import (
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"manvi/dc"
)

// FuzzWriteGateNeverAllowsASecret is the property the whole ladder exists for,
// stated as an invariant rather than as a list of cases.
//
// The task plans everything, so nothing in the plan can be the reason a write
// is refused — if a path resolves to a credential file and the gate allows it,
// the failure is in normalization or matching, which is precisely where the
// case-variant and NUL-byte bypasses were found by hand.
func FuzzWriteGateNeverAllowsASecret(f *testing.F) {
	for _, seed := range []string{
		".env", ".ENV", "src/../.env", "./.env", "a/b/../../.env",
		".env.production", "sub/.env", "keys/id_rsa", ".npmrc",
		"src/a.go", "docs/x.md", "", ".", "..", "/etc/passwd",
		".claude/settings.json", ".git/config", "x\x00.env",
	} {
		f.Add(seed)
	}

	root := f.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		f.Fatal(err)
	}
	gate := FileGate{Root: resolved, HardRules: true, AllowNeighbors: true}
	task := &dc.Task{ID: "T", PlannedFiles: []dc.PlannedFile{{Path: "**", AllowedChange: dc.ChangeModify}}}

	f.Fuzz(func(t *testing.T, path string) {
		if !utf8.ValidString(path) {
			return
		}
		decision := gate.EvaluateFileChange(path, task, dc.OpWrite, false)

		// Whatever the gate decided, the normalized target it reports must
		// never be a credential path that was allowed. Comparing against the
		// *decision's own* target is the strong form: it catches a normalizer
		// that resolves to a secret while the matcher looks at something else.
		if decision.Action == Allow {
			target := decision.Target
			base := strings.ToLower(filepath.Base(target))
			for _, forbidden := range []string{".env", "id_rsa", "id_ed25519", ".npmrc", ".netrc", ".pypirc"} {
				if base == forbidden || strings.HasPrefix(base, ".env.") {
					t.Fatalf("input %q was allowed and resolved to %q, a credential path", path, target)
				}
			}
			if strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") {
				t.Fatalf("input %q was allowed and resolved to key material %q", path, target)
			}
			// An allowed write must never point outside the repository.
			if filepath.IsAbs(target) {
				t.Fatalf("input %q was allowed with an absolute target %q", path, target)
			}
			if strings.HasPrefix(target, "../") || target == ".." {
				t.Fatalf("input %q was allowed with an escaping target %q", path, target)
			}
			// And a NUL must never survive into something a syscall would truncate.
			if strings.ContainsRune(target, 0) {
				t.Fatalf("input %q was allowed with a NUL-bearing target %q", path, target)
			}
		}

		// A decision must always be internally consistent: a denial names a
		// rule, and a rule's severity matches the classification.
		if decision.Action == Deny {
			if decision.Rule == RuleNone {
				t.Fatalf("input %q was denied without naming a rule", path)
			}
			if decision.Severity != SeverityOf(decision.Rule) {
				t.Fatalf("input %q: severity %q does not match rule %q", path, decision.Severity, decision.Rule)
			}
		}
	})
}

// FuzzNormalizeRepoPathDoesNotAliasBetweenContainedPaths states the invariant
// the normalizer actually has to hold: renormalizing its own output must never
// land on a *different* contained path. If it could, two inputs the gate treats
// as one file would be two files to the filesystem, or the reverse — which is
// the shape of every bypass found in this package.
//
// The stronger property — exact idempotence — does not hold, and the reason is
// worth recording rather than papering over. The normalizer unwraps a matched
// pair of surrounding quotes, which is input cleaning for a path typed at a CLI.
// That step cannot distinguish a file literally named `""` from a quoted empty
// string, so feeding `""` back in yields an empty path. Landing on "outside the
// root" is the fail-closed direction and is permitted here; landing on a
// different contained file is not.
func FuzzNormalizeRepoPathDoesNotAliasBetweenContainedPaths(f *testing.F) {
	for _, seed := range []string{"src/a.go", "./src/a.go", "src/../src/a.go", ".env", "", "a//b"} {
		f.Add(seed)
	}
	root := f.TempDir()
	resolved, _ := filepath.EvalSymlinks(root)

	f.Fuzz(func(t *testing.T, path string) {
		if !utf8.ValidString(path) {
			return
		}
		once, outside := NormalizeRepoPath(resolved, path)
		if outside {
			return
		}
		twice, outsideAgain := NormalizeRepoPath(resolved, once)
		if outsideAgain {
			// Fail-closed: the second pass refused what the first accepted.
			// Nothing is aliased, so this is safe.
			return
		}
		if once != twice {
			t.Fatalf("%q normalized to the contained path %q, which renormalized to a "+
				"different contained path %q — two inputs the gate treats as one file", path, once, twice)
		}
	})
}
