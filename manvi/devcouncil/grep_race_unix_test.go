//go:build unix

package devcouncil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/dcgrep"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
)

// raceToWithheldFixture links a withheld .env under a new name inside the first
// sub-search, after the listing was judged.
func raceToWithheldFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	real := testsupport.DCGrep(t)
	writeRepoFile(t, f.root, ".env", "API=needle LATE\n")
	writeRepoFile(t, f.root, "src/a.go", "needle visible\n")
	wrapper := filepath.Join(t.TempDir(), "w")
	script := fmt.Sprintf(`#!/bin/sh
REAL=%q
ROOT=%q
if [ "$1" = search ]; then
  BODY=$(cat)
  case "$BODY" in
    *'"path":".",'*|*'"path":"."}'*) ;;
    *'"path":"'*) ln "$ROOT/.env" "$ROOT/src/alias.txt" 2>/dev/null ;;
  esac
  printf '%%s' "$BODY" | "$REAL" "$@"
  exit $?
fi
exec "$REAL" "$@"
`, real, f.root)
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil { //nolint:gosec // an executable wrapper script in a test temp dir
		t.Fatal(err)
	}
	f.reg.deps.Grep = dcgrep.New(wrapper, f.root)
	return f
}

// A new name for a file that is withheld already moves no listed verdict, yet the
// search may have read it, and a retry that names it only when the pattern
// matched would be a per-pattern bit. The second listing makes both patterns
// take the same path.
func TestGrepLinkToAWithheldFileMidSearchIsPatternIndependent(t *testing.T) {
	a := advGrepRaw(t, raceToWithheldFixture(t), "needle LATE", map[string]any{"include_ignored": true})
	b := advGrepRaw(t, raceToWithheldFixture(t), "zzz-none", map[string]any{"include_ignored": true})
	if a != b {
		t.Fatalf("the reply depends on the pattern:\nmatching: %s\nnot matching: %s", a, b)
	}
	if !strings.Contains(a, "src/alias.txt") {
		t.Errorf("the new name for the withheld file is not in the withheld report: %s", a)
	}
}

// A tree that changes during every attempt ends in one error, and that error is
// the same for a pattern that matches the linked file and one that does not: it
// depends on the refusals, never on the matches.
func TestGrepTreeChangingEveryAttemptFailsTheSameForEveryPattern(t *testing.T) {
	build := func() *fixture {
		f := newFixture(t)
		real := testsupport.DCGrep(t)
		writeRepoFile(t, f.root, "token.txt", "needle LATE\n")
		writeRepoFile(t, f.root, "src/a.go", "needle visible\n")
		wrapper := filepath.Join(t.TempDir(), "w")
		script := fmt.Sprintf(`#!/bin/sh
REAL=%q
ROOT=%q
if [ "$1" = search ]; then
  ln "$ROOT/token.txt" "$ROOT/src/alias-$$.txt" 2>/dev/null
fi
exec "$REAL" "$@"
`, real, f.root)
		if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil { //nolint:gosec // an executable wrapper script in a test temp dir
			t.Fatal(err)
		}
		f.reg.deps.Grep = dcgrep.New(wrapper, f.root)
		return f
	}
	a := advGrepRaw(t, build(), "needle LATE", map[string]any{"include_ignored": true})
	b := advGrepRaw(t, build(), "zzz-none", map[string]any{"include_ignored": true})
	if a != b {
		t.Fatalf("the outcome depends on the pattern:\nmatching: %s\nnot matching: %s", a, b)
	}
	if !strings.HasPrefix(a, "isError=true") || !strings.Contains(a, "changed under the search repeatedly") {
		t.Errorf("a tree that changes on every attempt did not fail with the repeated-change error: %s", a)
	}
	if strings.Contains(a, "alias") || strings.Contains(a, "token") || strings.Contains(a, "LATE") {
		t.Errorf("the error names withheld files or content: %s", a)
	}
}
