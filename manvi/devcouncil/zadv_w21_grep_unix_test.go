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

// w21Marks are the files a fixture's wrapper writes so a test can prove its
// writer ran: relist when the relist hook ran (the relist only happens when the
// snapshot does not vouch for the tree), fired when a search hook ran, failed
// when any hook command exited non-zero (a writer whose link command failed
// exercised nothing).
type w21Marks struct{ relist, fired, failed string }

var w21Markers sync.Map

func w21MarksOf(t *testing.T, f *fixture) w21Marks {
	t.Helper()
	m, ok := w21Markers.Load(f)
	marks, isMarks := m.(w21Marks)
	if !ok || !isMarks {
		t.Fatal("no writer markers are registered for this fixture")
	}
	return marks
}

func w21RelistRan(t *testing.T, f *fixture) {
	t.Helper()
	marks := w21MarksOf(t, f)
	if _, err := os.Stat(marks.relist); err != nil {
		t.Fatalf("the relist hook never ran, so this test proved nothing: %v", err)
	}
	w21WriterCommandsSucceeded(t, marks)
}

// w21WriterRan fails the test unless the fixture's search hook ran at least once
// and no hook command failed.
func w21WriterRan(t *testing.T, f *fixture) {
	t.Helper()
	marks := w21MarksOf(t, f)
	if _, err := os.Stat(marks.fired); err != nil {
		t.Fatalf("the writer never ran around a search, so this test proved nothing: %v", err)
	}
	w21WriterCommandsSucceeded(t, marks)
}

func w21WriterCommandsSucceeded(t *testing.T, marks w21Marks) {
	t.Helper()
	if _, err := os.Stat(marks.failed); err == nil {
		t.Fatal("a writer command failed, so the tree was not changed the way the test describes")
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
	fired := filepath.Join(dir, "search.fired")
	failed := filepath.Join(dir, "writer.failed")
	w.relistPre = `touch "` + mark + `"; ` + w.relistPre
	if w.searchPre != "" {
		w.searchPre = `touch "` + fired + `"; ` + w.searchPre
	}
	// Each hook runs in its own subshell and a non-zero exit leaves a marker the
	// test reads, so a failed ln is a failed test and not a quiet no-op.
	for _, c := range []*string{&w.searchPre, &w.searchPost, &w.relistPre, &w.relistPost} {
		*c = `( : ; ` + *c + ` ) || touch "` + failed + `"`
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
	w21Markers.Store(f, w21Marks{relist: mark, fired: fired, failed: failed})
	return f
}

// W21 (a), the result cap, ported from w21-grepbit-adv @ c7fa4f9 onto w21Fixture.
// A match the reply drops (in a name that exists only while the engine walks)
// must not occupy a cap slot, so a capped reply does not depend on whether the
// withheld file matched. The alias sorts before the visible file.
func TestW21GrepCapSlotTakenByDroppedMatchLeaksWithheldMatch(t *testing.T) {
	type shape struct {
		extra  map[string]any
		secret string
	}
	cases := map[string]shape{
		"max_results=1":           {map[string]any{"include_ignored": true, "max_results": 1}, "API=needle K\n"},             //nolint:gosec // test fixture text, not a credential
		"default cap":             {map[string]any{"include_ignored": true}, strings.Repeat("needle K\n", 60)},               //nolint:gosec // test fixture text, not a credential
		"past the engine maximum": {map[string]any{"include_ignored": true}, strings.Repeat("needle K\n", grepMaxLimit+100)}, //nolint:gosec // test fixture text, not a credential
	}
	// The alias sorts before the visible match, and after it: the engine sets
	// truncated for a match that is displaced as well as for one that is kept.
	for name, c := range cases {
		for _, alias := range []string{"0alias.txt", "zalias.txt"} {
			t.Run(name+"/"+alias, func(t *testing.T) {
				build := func() *fixture {
					return w21Fixture(t, map[string]string{
						".env":      c.secret,
						"src/a.go":  "needle visible\n",
						"src/b.txt": "plain\n",
					}, w21Writer{
						searchPre:  `ln "$ROOT/.env" "$ROOT/src/` + alias + `"`,
						searchPost: `rm -f "$ROOT/src/` + alias + `"`,
					})
				}
				fa, fb := build(), build()
				a := advGrepRaw(t, fa, "needle (K|visible)", c.extra)
				b := advGrepRaw(t, fb, "needle (zzz|visible)", c.extra)
				w21WriterRan(t, fa)
				w21WriterRan(t, fb)
				t.Logf("secret matches:     %s", a)
				t.Logf("secret not matches: %s", b)
				if a != b {
					t.Fatalf("the capped reply depends on whether the withheld file matched:\nmatching: %s\nnot matching: %s", a, b)
				}
			})
		}
	}
}

// W21 (a), skipped counts and files_searched. The engine classifies a file as
// binary, too large or unreadable from its content or mode, not from the
// pattern, and reports aggregates over every file it opened. Same pattern
// throughout; only the withheld file a transient alias links to differs.
func TestW21GrepTransientLinkSkippedCountersIndependentOfWithheldContent(t *testing.T) {
	variants := map[string]string{
		"plain":  "TOKEN=abc123\n",
		"binary": "TOKEN=abc\x00\x00\n",
		"unread": "TOKEN=abc123\n",
	}
	var ref string
	for _, name := range []string{"plain", "binary", "unread"} {
		f := w21Fixture(t, map[string]string{
			".env":     variants[name],
			"src/a.go": "needle visible\n",
		}, w21Writer{
			searchPre:  `ln "$ROOT/.env" "$ROOT/src/alias.txt"`,
			searchPost: `rm -f "$ROOT/src/alias.txt"`,
		})
		if name == "unread" {
			if err := os.Chmod(filepath.Join(f.root, ".env"), 0); err != nil {
				t.Fatal(err)
			}
		}
		got := advGrepRaw(t, f, "needle", map[string]any{"include_ignored": true})
		if name == "unread" {
			_ = os.Chmod(filepath.Join(f.root, ".env"), 0o600) //nolint:errcheck // restore for cleanup
		}
		w21WriterRan(t, f)
		t.Logf("%-8s %s", name, got)
		if ref == "" {
			ref = got
			continue
		}
		if got != ref {
			t.Errorf("the reply depends on the withheld file's content (%s vs plain):\nplain: %s\n%s: %s", name, ref, name, got)
		}
	}
}

// W21 (a), writers other than a new hard link in an existing directory: a
// directory symlink to the withheld directory (a behaviour test, not a
// regression test: the engine does not follow symlinks), a link in a directory
// made only for the search, and a link to a case-variant withheld name. Matching and non-matching patterns must produce the same reply.
func TestW21GrepOtherTransientShapesArePatternIndependent(t *testing.T) {
	shapes := map[string]w21Writer{
		"directory symlink": {
			searchPre:  `ln -s "$ROOT/sec" "$ROOT/src/d"`,
			searchPost: `rm -f "$ROOT/src/d"`,
		},
		"link in a transient directory": {
			searchPre:  `mkdir "$ROOT/src/tmpd" && ln "$ROOT/.env" "$ROOT/src/tmpd/x.txt"`,
			searchPost: `rm -rf "$ROOT/src/tmpd"`,
		},
		"case-variant withheld name": {
			searchPre:  `ln "$ROOT/.ENV.local" "$ROOT/src/alias.txt"`,
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
				f := w21Fixture(t, files, w)
				replies = append(replies, advGrepRaw(t, f, p, map[string]any{"include_ignored": true}))
				w21WriterRan(t, f)
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

// W21 (a)/(b). A listed file becomes a hard link to the withheld file right
// after the engine's second listing returns and stays one. This is the window
// between the engine's relist and Go judging it (candidateRefusals reads the
// link count then), not the later one between the relist and revalidateMatches:
// the hook runs before Go looks at the relist, so reaching revalidateMatches
// with a changed file would need a seam between the two and there is none. The
// changed link count makes the relist's verdicts differ from the listing's for
// both patterns, so both are retried and both replies must be equal and free of
// the withheld content.
func TestW21GrepLinkMadeAfterTheEnginesRelistIsRejudgedPatternIndependent(t *testing.T) {
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
	for _, r := range []string{a, b} {
		if strings.Contains(r, "SECRETVALUE") {
			t.Errorf("withheld content in the reply: %s", r)
		}
	}
	if a != b {
		t.Fatalf("the reply depends on the pattern:\nmatching b.txt: %s\nnot matching: %s", a, b)
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
