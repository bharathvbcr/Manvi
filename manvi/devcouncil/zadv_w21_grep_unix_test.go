//go:build unix

package devcouncil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/dcgrep"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
)

// Adversarial regression tests for card ft-d206865e0745d54a684041442b35d557
// (commit 54263e9, "the reply no longer depends on whether a withheld file
// matched under a concurrent link writer").
//
// Every test drives the real dcgrep through a wrapper script that plays the
// concurrent writer at a deterministic point: around each partitioned
// sub-search (searchPre/searchPost) or around the second `list` call, the
// relist (relistPre/relistPost). The speculative whole-tree search (path ".")
// is left alone, as in grep_transient_link_unix_test.go. No sleeps.

// w21Markers maps a fixture to the file the wrapper creates when the writer's
// relist hook ran, so a relist test can prove the writer fired (the relist only
// happens when the snapshot does not vouch for the tree).
var w21Markers sync.Map

func w21RelistRan(t *testing.T, f *fixture) {
	t.Helper()
	m, _ := w21Markers.Load(f)
	mark, isPath := m.(string)
	if !isPath {
		t.Fatal("no relist marker is registered for this fixture")
	}
	if _, err := os.Stat(mark); err != nil {
		t.Fatalf("the relist hook never ran, so this test proved nothing: %v", err)
	}
}

type w21Writer struct {
	searchPre, searchPost string // shell, run around each sub-search of a non-"." path
	relistPre, relistPost string // shell, run around the 2nd `list` call only
}

// w21Fixture builds a repository with the given files, then a wrapper around
// the engine that runs the writer's commands. $ROOT is the repository root.
func w21Fixture(t *testing.T, files map[string]string, w w21Writer) *fixture {
	t.Helper()
	f := newFixture(t)
	// Every stamp reads as possibly changed (within the slack of the search's
	// start), so the post-search check always takes the full relist path. Left to
	// the wall clock, a loaded machine ages the files past the slack and the
	// relist silently does not happen, which makes the relist tests vacuous.
	prevNow := grepNow
	grepNow = func() time.Time { return time.Now().Add(-time.Hour) }
	t.Cleanup(func() { grepNow = prevNow })
	real := testsupport.DCGrep(t)
	for name, content := range files {
		writeRepoFile(t, f.root, name, content)
	}
	dir := t.TempDir()
	mark := filepath.Join(dir, "relist.ran")
	w.relistPre = `touch "` + mark + `"; ` + w.relistPre
	for _, c := range []*string{&w.searchPre, &w.searchPost, &w.relistPre, &w.relistPost} {
		*c = ": ; " + *c
	}
	counter := filepath.Join(dir, "lists")
	wrapper := filepath.Join(dir, "w")
	script := fmt.Sprintf(`#!/bin/sh
REAL=%q
ROOT=%q
COUNT=%q
if [ "$1" = search ]; then
  BODY=$(cat)
  LINK=0
  case "$BODY" in
    *'"path":".",'*|*'"path":"."}'*) ;;
    *'"path":"'*) LINK=1 ;;
  esac
  if [ $LINK = 1 ]; then
%s
  fi
  printf '%%s' "$BODY" | "$REAL" "$@"
  RC=$?
  if [ $LINK = 1 ]; then
%s
  fi
  exit $RC
fi
if [ "$1" = files ]; then
  echo x >> "$COUNT"
  N=$(wc -l < "$COUNT" | tr -d ' ')
  if [ "$N" = 2 ]; then
%s
    BODY=$(cat)
    printf '%%s' "$BODY" | "$REAL" "$@"
    RC=$?
%s
    exit $RC
  fi
fi
exec "$REAL" "$@"
`, real, f.root, counter, w.searchPre, w.searchPost, w.relistPre, w.relistPost)
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil { //nolint:gosec // an executable wrapper script in a test temp dir
		t.Fatal(err)
	}
	f.reg.deps.Grep = dcgrep.New(wrapper, f.root)
	w21Markers.Store(f, mark)
	return f
}

// W21 (a), writers other than a new hard link or symlink in an existing
// directory: a directory symlink to the withheld directory, a link in a
// directory made only for the search, and a link to a case-variant withheld
// name. Matching and non-matching patterns must produce the same reply.
func TestW21GrepOtherTransientShapesArePatternIndependent(t *testing.T) {
	shapes := map[string]w21Writer{
		"directory symlink": {
			searchPre:  `ln -s "$ROOT/sec" "$ROOT/src/d" 2>/dev/null`,
			searchPost: `rm -f "$ROOT/src/d"`,
		},
		"link in a transient directory": {
			searchPre:  `mkdir "$ROOT/src/tmpd" && ln "$ROOT/.env" "$ROOT/src/tmpd/x.txt"`,
			searchPost: `rm -rf "$ROOT/src/tmpd"`,
		},
		"case-variant withheld name": {
			searchPre:  `ln "$ROOT/.ENV.local" "$ROOT/src/alias.txt" 2>/dev/null`,
			searchPost: `rm -f "$ROOT/src/alias.txt"`,
		},
		"both secrets linked": {
			searchPre:  `ln "$ROOT/.env" "$ROOT/src/alias.txt" && ln "$ROOT/.ENV.local" "$ROOT/src/alias2.txt"`,
			searchPost: `rm -f "$ROOT/src/alias.txt" "$ROOT/src/alias2.txt"`,
		},
	}
	files := map[string]string{
		".env":        "API=needle ONE\n",
		".ENV.local":  "API=needle TWO\n",
		"sec/.env":    "API=needle THREE\n",
		"src/a.go":    "needle visible\n",
		"src/zlast.x": "needle also visible\n",
	}
	for name, w := range shapes {
		t.Run(name, func(t *testing.T) {
			var replies []string
			for _, p := range []string{"needle (ONE|TWO|THREE|visible)", "needle (zzz|visible)", "needle (ONE|visible)", "needle (TWO|visible)", "needle (THREE|visible)"} {
				replies = append(replies, advGrepRaw(t, w21Fixture(t, files, w), p, map[string]any{"include_ignored": true}))
			}
			for i, r := range replies[1:] {
				if r != replies[0] {
					t.Errorf("pattern #%d reply differs from #0:\n#0: %s\n#%d: %s", i+1, replies[0], i+1, r)
				}
			}
			for _, r := range replies {
				if strings.Contains(r, "ONE") || strings.Contains(r, "TWO") || strings.Contains(r, "THREE") {
					// the pattern is normalised to <PATTERN>, so any hit is file content
					t.Errorf("withheld content in the reply: %s", r)
				}
			}
		})
	}
}

// W21 (a)/(c). A name that exists only while the engine lists the tree the
// second time (the relist) changes the listing, so the whole pass is retried;
// the reply after the retry (or the error, if it keeps changing) must not
// depend on the pattern. Also: the link exists only during the relist, so no
// match can be in it.
func TestW21GrepLinkOnlyDuringRelistIsPatternIndependent(t *testing.T) {
	for name, lnCmd := range map[string]string{
		"hard link": `ln "$ROOT/.env" "$ROOT/src/alias.txt"`,
		"symlink":   `ln -s "$ROOT/.env" "$ROOT/src/alias.txt"`,
	} {
		t.Run(name, func(t *testing.T) {
			build := func() *fixture {
				return w21Fixture(t, map[string]string{
					".env":     "API=needle LATE\n",
					"src/a.go": "needle visible\n",
				}, w21Writer{relistPre: lnCmd, relistPost: `rm -f "$ROOT/src/alias.txt"`})
			}
			fa, fb := build(), build()
			a := advGrepRaw(t, fa, "needle (LATE|visible)", map[string]any{"include_ignored": true})
			b := advGrepRaw(t, fb, "needle (zzz|visible)", map[string]any{"include_ignored": true})
			w21RelistRan(t, fa)
			w21RelistRan(t, fb)
			if a != b {
				t.Fatalf("the reply depends on the pattern:\nmatching: %s\nnot matching: %s", a, b)
			}
			if strings.Contains(a, "LATE") {
				t.Errorf("withheld content in the reply: %s", a)
			}
		})
	}
}

// W21 (a). A listed file replaced (rename-over) by a link to the withheld file
// after the search and before the relist, and left that way. The relist shows
// a refused name, so the next attempt withholds it; the replies for a pattern
// that matched the withheld file and one that did not must be equal.
func TestW21GrepRenameOverBeforeRelistIsPatternIndependent(t *testing.T) {
	build := func() *fixture {
		return w21Fixture(t, map[string]string{
			".env":      "API=needle LATE\n",
			"src/a.go":  "needle visible\n",
			"src/b.txt": "needle plain\n",
		}, w21Writer{
			relistPre: `ln "$ROOT/.env" "$ROOT/src/swap.tmp" && mv "$ROOT/src/swap.tmp" "$ROOT/src/b.txt"`,
		})
	}
	fa, fb := build(), build()
	a := advGrepRaw(t, fa, "needle (LATE|visible|plain)", map[string]any{"include_ignored": true})
	b := advGrepRaw(t, fb, "needle (zzz|visible|plain)", map[string]any{"include_ignored": true})
	w21RelistRan(t, fa)
	w21RelistRan(t, fb)
	if a != b {
		t.Fatalf("the reply depends on the pattern:\nmatching: %s\nnot matching: %s", a, b)
	}
}

// W21 (a)/(b), the window between the relist and revalidateMatches that the
// fix's comment calls safe: a listed file becomes a hard link to the withheld
// file right after the relist and stays one. Neither reply may carry the
// withheld file's content. Observed on 54263e9: the two replies are identical,
// because the full recheck re-judges every relisted name and the new link count
// trips the refusal comparison for both patterns, so both are retried. The
// replies are logged for the record.
func TestW21GrepSwapAfterRelistNeverReturnsWithheldContent(t *testing.T) {
	build := func() *fixture {
		return w21Fixture(t, map[string]string{
			".env":      "API=needle SECRETVALUE\n",
			"src/a.go":  "needle visible\n",
			"src/b.txt": "needle plain\n",
		}, w21Writer{relistPost: `rm -f "$ROOT/src/b.txt"; ln "$ROOT/.env" "$ROOT/src/b.txt"`})
	}
	fa, fb := build(), build()
	a := advGrepRaw(t, fa, "needle (plain|visible)", map[string]any{"include_ignored": true})
	b := advGrepRaw(t, fb, "needle (zzz|visible)", map[string]any{"include_ignored": true})
	w21RelistRan(t, fa)
	w21RelistRan(t, fb)
	t.Logf("pattern matches the readable b.txt: %s", a)
	t.Logf("pattern does not match b.txt:       %s", b)
	for _, r := range []string{a, b} {
		if strings.Contains(r, "SECRETVALUE") {
			t.Errorf("withheld content in the reply: %s", r)
		}
	}
}

// W21 (a). A link that exists at listing time and is gone before the search:
// the listing refuses both names (link count), the partition leaves them out,
// the relist differs, and the retry sees neither. Both patterns must agree.
func TestW21GrepLinkPresentAtListingRemovedBeforeSearchIsPatternIndependent(t *testing.T) {
	build := func() *fixture {
		f := w21Fixture(t, map[string]string{
			".env":     "API=needle LATE\n",
			"src/a.go": "needle visible\n",
		}, w21Writer{searchPre: `rm -f "$ROOT/src/alias.txt"`})
		if err := os.Link(filepath.Join(f.root, ".env"), filepath.Join(f.root, "src", "alias.txt")); err != nil {
			t.Fatal(err)
		}
		return f
	}
	a := advGrepRaw(t, build(), "needle (LATE|visible)", map[string]any{"include_ignored": true})
	b := advGrepRaw(t, build(), "needle (zzz|visible)", map[string]any{"include_ignored": true})
	if a != b {
		t.Fatalf("the reply depends on the pattern:\nmatching: %s\nnot matching: %s", a, b)
	}
	if strings.Contains(a, "LATE") {
		t.Errorf("withheld content in the reply: %s", a)
	}
}
