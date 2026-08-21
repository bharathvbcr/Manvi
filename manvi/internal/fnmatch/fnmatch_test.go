package fnmatch

import (
	"os"
	"strings"
	"testing"
)

// fixturePath is the shared CPython-generated parity table. The Rust matcher
// in crates/dc-glob reads the same file, so the two implementations cannot
// drift apart without one of them failing. Regenerate with
// scripts/gen-fnmatch-parity.py.
const fixturePath = "../../../testdata/fnmatch-parity.tsv"

// TestMatchesPythonFnmatch is the cross-language parity gate.
func TestMatchesPythonFnmatch(t *testing.T) {
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read parity fixture: %v", err)
	}
	checked := 0
	for i, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) != 3 {
			t.Fatalf("line %d is malformed: %q", i+1, line)
		}
		pattern, name, want := cols[0], cols[1], cols[2] == "true"
		if got := Match(pattern, name); got != want {
			t.Errorf("Match(%q, %q) = %v, want %v (CPython, line %d)", pattern, name, got, want, i+1)
		}
		checked++
	}
	// A fixture that failed to load would make this test pass by checking nothing.
	if checked < 500 {
		t.Fatalf("only %d cases loaded from %s", checked, fixturePath)
	}
}

// TestStarCrossesSeparator pins the single behaviour that separates this
// package from path.Match. If this ever flips, every secret and restricted
// path rule silently stops matching nested paths.
func TestStarCrossesSeparator(t *testing.T) {
	if !Match("*.py", "src/deep/foo.py") {
		t.Fatal(`Match("*.py", "src/deep/foo.py") = false; Python says true`)
	}
	if !Match(".claude/*", ".claude/agents/x.md") {
		t.Fatal(`".claude/*" must match nested paths, or the restricted-path rule leaks`)
	}
}

// TestDoubleStarRequiresASeparator is the other half: "**/.env" deliberately
// does not match a bare ".env", which is why DevCouncil lists both patterns.
func TestDoubleStarRequiresASeparator(t *testing.T) {
	if Match("**/.env", ".env") {
		t.Fatal(`Match("**/.env", ".env") = true; Python says false`)
	}
	if !Match("**/.env", "a/b/.env") {
		t.Fatal(`Match("**/.env", "a/b/.env") = false; Python says true`)
	}
}

func TestUnterminatedClassNeverMatchesEverything(t *testing.T) {
	// A malformed pattern must not become a wildcard.
	if Match("a[bc", "anything at all") {
		t.Fatal("unterminated character class matched an unrelated string")
	}
	if !Match("a[bc", "a[bc") {
		t.Fatal("unterminated '[' should be treated as a literal")
	}
}

func TestMatchAny(t *testing.T) {
	pats := []string{"*.md", "**/*.pem"}
	if !MatchAny(pats, "deep/key.pem") {
		t.Fatal("MatchAny missed a matching pattern")
	}
	if MatchAny(pats, "src/main.go") {
		t.Fatal("MatchAny matched an unrelated path")
	}
}
