//go:build unix

package devcouncil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/dcgrep"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
)

// agedClock moves the stamp clock an hour on, so files written a moment ago do
// not read as fresh: the stand-in for a checkout written long before the search.
func agedClock(t testing.TB) time.Time {
	t.Helper()
	prev := grepNow
	grepNow = func() time.Time { return time.Now().Add(time.Hour) }
	t.Cleanup(func() { grepNow = prev })
	return grepNow()
}

// countFullRechecks counts the calls that took the full relist-and-judge path.
func countFullRechecks(t testing.TB) *int {
	t.Helper()
	n := 0
	prev := grepFullRecheck
	grepFullRecheck = func() { n++ }
	t.Cleanup(func() { grepFullRecheck = prev })
	return &n
}

// Every kind of change the judgement depends on makes the recorded stamps read
// back different; a change to nothing does not. Each case is one change to an
// otherwise settled tree.
func TestTreeStampsSeeEveryChangeTheJudgementDependsOn(t *testing.T) {
	started := agedClock(t)
	outside := t.TempDir()
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, root string)
		want   bool // whether the stamps still hold
	}{
		{"nothing", func(*testing.T, string) {}, true},
		{"new name in a listed directory", func(t *testing.T, root string) {
			writeRepoFile(t, root, "src/new.txt", "x\n")
		}, false},
		{"rename within a directory", func(t *testing.T, root string) {
			mustRename(t, filepath.Join(root, "src/a.go"), filepath.Join(root, "src/b2.go"))
		}, false},
		{"rename across directories", func(t *testing.T, root string) {
			mustRename(t, filepath.Join(root, "src/a.go"), filepath.Join(root, "lib/a.go"))
		}, false},
		{"hard link from outside the root", func(t *testing.T, root string) {
			if err := os.Link(filepath.Join(root, "src/a.go"), filepath.Join(outside, "alias")); err != nil {
				t.Fatalf("no hard links across these directories: %v", err)
			}
		}, false},
		{"hard link inside the root", func(t *testing.T, root string) {
			if err := os.Link(filepath.Join(root, "src/a.go"), filepath.Join(root, "lib/alias.go")); err != nil {
				t.Fatalf("no hard links: %v", err)
			}
		}, false},
		{"symlink swapped in for a file", func(t *testing.T, root string) {
			mustRemove(t, filepath.Join(root, "src/a.go"))
			if err := os.Symlink("../lib/c.go", filepath.Join(root, "src/a.go")); err != nil {
				t.Fatalf("no symlinks: %v", err)
			}
		}, false},
		{"symlink swapped in for a directory", func(t *testing.T, root string) {
			mustRename(t, filepath.Join(root, "lib"), filepath.Join(root, "lib.moved"))
			if err := os.Symlink("lib.moved", filepath.Join(root, "lib")); err != nil {
				t.Fatalf("no symlinks: %v", err)
			}
		}, false},
		{"chmod of a listed file", func(t *testing.T, root string) {
			if err := os.Chmod(filepath.Join(root, "src/a.go"), 0o400); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"listed file removed", func(t *testing.T, root string) {
			mustRemove(t, filepath.Join(root, "lib/c.go"))
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			writeRepoFile(t, f.root, "src/a.go", "a\n")
			writeRepoFile(t, f.root, "lib/c.go", "c\n")
			listed := []string{"lib/c.go", "src/a.go"}
			snap := f.reg.snapshotTree(listed, nil, started)
			if snap == nil || snap.fresh {
				t.Fatalf("setup: no settled snapshot (nil=%v)", snap == nil)
			}
			tc.change(t, f.root)
			if got := snap.holds(f.reg, listed); got != tc.want {
				t.Errorf("stamps hold = %v, want %v", got, tc.want)
			}
		})
	}
}

// A recorded time within the slack of the search start, or after it, makes the
// snapshot fresh, whatever it reads back as.
func TestTreeStampsFreshWithinTheSlackOfTheSearchStart(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, "src/a.go", "a\n")
	listed := []string{"src/a.go"}
	if snap := f.reg.snapshotTree(listed, nil, time.Now()); snap == nil || !snap.fresh {
		t.Fatalf("files written just now were not marked fresh against a search starting now: %+v", snap)
	}
	if snap := f.reg.snapshotTree(listed, nil, time.Now().Add(time.Hour)); snap == nil || snap.fresh {
		t.Fatalf("files an hour old were marked fresh: %+v", snap)
	}
	snap := f.reg.snapshotTree(listed, nil, time.Now().Add(time.Hour))
	if !snap.holds(f.reg, listed) {
		t.Error("an unchanged settled tree does not read back the same")
	}
	fresh := f.reg.snapshotTree(listed, nil, time.Now())
	if fresh.holds(f.reg, listed) {
		t.Error("a fresh snapshot vouched for the tree")
	}
}

// When nothing changes in an aged tree the fast path stands and the relist is
// never taken; the counter proves which path ran.
func TestGrepTakesTheStatPassWhenNothingChanges(t *testing.T) {
	agedClock(t)
	n := countFullRechecks(t)
	f := newFixture(t)
	writeRepoFile(t, f.root, "src/a.go", "needle\n")
	writeRepoFile(t, f.root, "lib/c.go", "needle\n")
	res := f.call("devcouncil_grep", map[string]any{"pattern": "needle", "include_ignored": true})
	if res.IsError || !strings.Contains(res.Text, "src/a.go") {
		t.Fatalf("grep failed: %s", res.Text)
	}
	if *n != 0 {
		t.Errorf("the full relist ran %d time(s) over a tree nothing touched", *n)
	}
}

// The only change is a hard link made OUTSIDE the root to a listed file, during
// the search. No directory in the root moves, so only the file's link count and
// ctime show it. It must take the full path (counter), and the reply must be the
// one a file refused at listing time gives, the same for every pattern.
func TestGrepHardLinkFromOutsideTheRootTakesTheFullPath(t *testing.T) {
	run := func(pattern string) (string, int) {
		agedClock(t)
		n := countFullRechecks(t)
		f := newFixture(t)
		real := testsupport.DCGrep(t)
		// A refused decoy forces the partitioned path, whose sub-searches run
		// strictly after the listing was judged; the link is made in the first.
		writeRepoFile(t, f.root, ".env", "decoy=1\n")
		writeRepoFile(t, f.root, "src/a.go", "needle visible\n")
		writeRepoFile(t, f.root, "src/token.txt", "needle LATE\n")
		outside := t.TempDir()
		wrapper := filepath.Join(t.TempDir(), "w")
		script := fmt.Sprintf(`#!/bin/sh
REAL=%q
if [ "$1" = search ]; then
  BODY=$(cat)
  case "$BODY" in
    *'"path":".",'*|*'"path":"."}'*) ;;
    *'"path":"'*) ln "%s/src/token.txt" "%s/elsewhere" 2>/dev/null ;;
  esac
  printf '%%s' "$BODY" | "$REAL" "$@"
  exit $?
fi
exec "$REAL" "$@"
`, real, f.root, outside)
		if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil { //nolint:gosec // an executable wrapper script in a test temp dir
			t.Fatal(err)
		}
		f.reg.deps.Grep = dcgrep.New(wrapper, f.root)
		out := advGrepRaw(t, f, pattern, map[string]any{"include_ignored": true})
		if _, err := os.Stat(filepath.Join(outside, "elsewhere")); err != nil {
			t.Fatalf("the link was not made (cross-device?): %v", err)
		}
		return out, *n
	}
	a, na := run("needle LATE")
	b, nb := run("zzz-none")
	if na != 1 || nb != 1 {
		t.Errorf("the full path ran %d and %d times, want once each: only the link count and ctime moved", na, nb)
	}
	if strings.HasPrefix(a, "isError=true") || strings.HasPrefix(b, "isError=true") {
		t.Fatalf("a link made outside the root made the call fail:\n%s\n%s", a, b)
	}
	if !strings.Contains(a, "src/token.txt") {
		t.Errorf("the linked file is not in the withheld report: %s", a)
	}
	if strings.Contains(a, "LATE") {
		t.Errorf("content of the linked file reached the reply: %s", a)
	}
}

func mustRename(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
}

func mustRemove(t *testing.T, name string) {
	t.Helper()
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
}
