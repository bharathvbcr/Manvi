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

// transientLinkFixture puts a writer around every sub-search of a tree with a
// withheld .env: it links .env to src/alias.txt (the link command is lnCmd)
// just before the engine runs and removes the link just after, so the name
// exists only while the engine walks. The listing before the search and the
// listing after it both lack the name, so nothing but the matches can tell the
// search read it. The link is not made for the speculative whole-tree search,
// which is discarded unread and runs concurrently with the listing.
func transientLinkFixture(t *testing.T, lnCmd string) *fixture {
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
  LINK=0
  case "$BODY" in
    *'"path":".",'*|*'"path":"."}'*) ;;
    *'"path":"'*) LINK=1 ;;
  esac
  [ $LINK = 1 ] && %s 2>/dev/null
  printf '%%s' "$BODY" | "$REAL" "$@"
  RC=$?
  [ $LINK = 1 ] && rm -f "$ROOT/src/alias.txt"
  exit $RC
fi
exec "$REAL" "$@"
`, real, f.root, lnCmd)
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil { //nolint:gosec // an executable wrapper script in a test temp dir
		t.Fatal(err)
	}
	f.reg.deps.Grep = dcgrep.New(wrapper, f.root)
	return f
}

// A name that exists only while the engine walks is invisible to both listings,
// so what the search matched in it is the only thing that can tell it was
// there. A reply that differed on that (an error for a pattern the secret
// matches, a normal answer for one it does not) is one bit per pattern about a
// withheld file. Both patterns must get the same reply, and the secret must not
// be in it.
func TestGrepTransientLinkToAWithheldFileIsPatternIndependent(t *testing.T) {
	for name, lnCmd := range map[string]string{
		"hard link": `ln "$ROOT/.env" "$ROOT/src/alias.txt"`,
		"symlink":   `ln -s "$ROOT/.env" "$ROOT/src/alias.txt"`,
	} {
		t.Run(name, func(t *testing.T) {
			extra := map[string]any{"include_ignored": true}
			a := advGrepRaw(t, transientLinkFixture(t, lnCmd), "needle LATE", extra)
			b := advGrepRaw(t, transientLinkFixture(t, lnCmd), "zzz-none", extra)
			if a != b {
				t.Fatalf("the reply depends on whether the withheld file matched:\nmatching: %s\nnot matching: %s", a, b)
			}
			if strings.Contains(a, "LATE") || strings.Contains(a, "alias.txt") {
				t.Errorf("the reply carries the withheld file's content or the transient name: %s", a)
			}
		})
	}
}
