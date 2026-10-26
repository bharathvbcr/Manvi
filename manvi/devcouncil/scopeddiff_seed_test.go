package devcouncil

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The end-of-turn diff starts from the baseline's kept scratch index and
// re-reads only the examined paths. The contract is that it answers exactly as
// the full route does, which is what a baseline without a kept index still
// takes. These tests hold the two to byte equality over every shape of change
// the gates have to read, with the one stated exception in
// TestScopedDiffSeesAFileFlaggedAssumeUnchangedDuringTheTurn.

// seededTurn is a repository, the baseline taken before a turn, and the turn.
type seededTurn struct {
	root     string
	r        *Registry
	baseline Baseline
}

// turnFixture leaves a repository where the operator had work in progress, takes
// the baseline, and then plays a turn that changes every kind of thing a diff
// has to carry.
func turnFixture(t *testing.T) seededTurn {
	t.Helper()
	root := benchRepo(t, 40, 0, 0)
	write(t, root, "mod.txt", "one\ntwo\nthree\n")
	write(t, root, "del.txt", "to be deleted\n")
	write(t, root, "old name.txt", "renamed content, long enough to be recognised as a rename\nsecond line\nthird line\n")
	write(t, root, "-dash.txt", "starts with a dash\n")
	write(t, root, "ünï/cödé.txt", "unicode path\n")
	write(t, root, "dir/a.txt", "a\n")
	write(t, root, "dir/b.txt", "b\n")
	write(t, root, "a1.go", "package a\n")
	write(t, root, "a[1].go", "package a\n")
	write(t, root, "outside.txt", "outside the examined set\n")
	write(t, root, "operator.txt", "operator's tracked file\n")
	if err := os.WriteFile(filepath.Join(root, "bin.dat"), []byte{0, 1, 2, 3, 0, 255}, 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-qm", "fixture")

	// Work the operator had in the tree before the turn began: a tracked edit
	// and an untracked file, neither of which is the turn's.
	write(t, root, "operator.txt", "operator's tracked file, edited before the turn\n")
	write(t, root, "operator_new.txt", "operator's untracked file\n")

	r := benchRegistry(root)
	baseline := r.CaptureBaseline(context.Background())
	if !baseline.Captured() || baseline.seed == nil {
		t.Fatalf("no baseline with a kept index: %+v", baseline)
	}

	// The turn.
	write(t, root, "mod.txt", "one\nTWO\nthree\nfour\n")
	if err := os.Remove(filepath.Join(root, "del.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "old name.txt"), filepath.Join(root, "new name.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, root, "-dash.txt", "starts with a dash, edited\n")
	write(t, root, "ünï/cödé.txt", "unicode path, edited\n")
	write(t, root, "ünï/new ünï.txt", "a new file with a space and accents\n")
	write(t, root, "-fresh dash.txt", "a new file that starts with a dash\n")
	write(t, root, "fresh.txt", "a new file\n")
	write(t, root, "dir/a.txt", "a, edited\n")
	write(t, root, "dir/c.txt", "c, new\n")
	if err := os.Remove(filepath.Join(root, "dir/b.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, root, "a1.go", "package a\n// edited\n")
	write(t, root, "a[1].go", "package a\n// edited too\n")
	write(t, root, "ignored/out.bin", "an ignored build product\n")
	if err := os.WriteFile(filepath.Join(root, "bin.dat"), []byte{0, 9, 9, 9, 0, 255}, 0o600); err != nil {
		t.Fatal(err)
	}
	// Changed after the baseline and never examined: it must not be in the diff.
	write(t, root, "outside.txt", "outside the examined set, changed by someone else\n")
	return seededTurn{root: root, r: r, baseline: baseline}
}

// withoutSeed is the same baseline with its kept index dropped, which is the
// full route the incremental one has to agree with.
func (f seededTurn) withoutSeed() Baseline { return Baseline{Tree: f.baseline.Tree} }

func TestScopedDiffFromTheKeptIndexMatchesTheFullRoute(t *testing.T) {
	f := turnFixture(t)
	ctx := context.Background()

	cases := []struct {
		name  string
		paths []string
		// incremental is false for the cases git refuses to add incrementally,
		// where the full route is the answer by construction.
		incremental bool
		mustShow    []string
	}{
		{"tracked and modified", []string{"mod.txt"}, true, []string{"mod.txt", "+TWO"}},
		{"untracked", []string{"fresh.txt"}, true, []string{"fresh.txt", "new file mode"}},
		{"deleted", []string{"del.txt"}, true, []string{"del.txt", "deleted file mode"}},
		{"renamed, both names examined", []string{"old name.txt", "new name.txt"}, true, []string{"rename from old name.txt", "rename to new name.txt"}},
		{"renamed, only the new name examined", []string{"new name.txt"}, true, []string{"new name.txt", "new file mode"}},
		{"binary", []string{"bin.dat"}, true, []string{"Binary files"}},
		{"leading dash", []string{"-dash.txt", "-fresh dash.txt"}, true, []string{"-dash.txt", "-fresh dash.txt"}},
		{"unicode and spaces", []string{"ünï/cödé.txt", "ünï/new ünï.txt"}, true, []string{"cödé.txt", "new ünï.txt"}},
		{"a directory", []string{"dir"}, true, []string{"dir/a.txt", "dir/b.txt", "dir/c.txt"}},
		{"a glob-looking name beside the file it would match", []string{"a[1].go"}, true, []string{"a[1].go"}},
		{"an ignored path named outright", []string{"ignored/out.bin", "mod.txt"}, false, []string{"mod.txt"}},
		{"an ignored path alone", []string{"ignored/out.bin"}, false, nil},
		{"a path that exists nowhere", []string{"nope.txt", "mod.txt"}, false, []string{"mod.txt"}},
		{"everything at once", []string{
			"mod.txt", "del.txt", "old name.txt", "new name.txt", "-dash.txt", "-fresh dash.txt", "fresh.txt",
			"ünï/cödé.txt", "ünï/new ünï.txt", "dir", "bin.dat", "a[1].go",
		}, true, []string{"mod.txt", "del.txt", "dir/c.txt", "Binary files"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantSpawns := procSpawns.Load()
			want, wantNotes, wantErr := f.r.scopedDiff(ctx, tc.paths, f.withoutSeed())
			wantSpawns = procSpawns.Load() - wantSpawns
			if wantErr != nil {
				t.Fatalf("full route: %v", wantErr)
			}
			gotSpawns := procSpawns.Load()
			got, gotNotes, gotErr := f.r.scopedDiff(ctx, tc.paths, f.baseline)
			gotSpawns = procSpawns.Load() - gotSpawns
			if gotErr != nil {
				t.Fatalf("incremental route: %v", gotErr)
			}

			if got != want {
				t.Fatalf("the scoped diff differs from the full route's.\nfull:\n%s\nincremental:\n%s", want, got)
			}
			if !reflect.DeepEqual(gotNotes, wantNotes) {
				t.Fatalf("notes differ: full %v, incremental %v", wantNotes, gotNotes)
			}
			for _, s := range tc.mustShow {
				if !strings.Contains(got, s) {
					t.Errorf("the diff does not show %q:\n%s", s, got)
				}
			}
			// Not part of this turn's change: the operator's earlier work, and a
			// file changed outside the examined set.
			for _, s := range []string{"operator.txt", "operator_new.txt", "outside.txt"} {
				if strings.Contains(got, s) {
					t.Errorf("the diff carries %q, which is not an examined path of this turn:\n%s", s, got)
				}
			}

			// The shortcut has to be the one that ran, or equality proves nothing.
			// Incremental is add, write-tree and the diff; the full route adds the
			// rev-parse that locates the index.
			if tc.incremental && gotSpawns >= wantSpawns {
				t.Errorf("the kept index was not used: %d git processes against the full route's %d",
					gotSpawns, wantSpawns)
			}
		})
	}
}

// The incremental route reads the examined paths and nothing else: the add it
// runs names them, and no spawn locates the repository's own index.
func TestScopedDiffFromTheKeptIndexIsThreeGitProcesses(t *testing.T) {
	f := turnFixture(t)
	before := procSpawns.Load()
	if _, _, err := f.r.scopedDiff(context.Background(), []string{"mod.txt", "fresh.txt"}, f.baseline); err != nil {
		t.Fatal(err)
	}
	if got := procSpawns.Load() - before; got != 3 {
		t.Fatalf("the scoped diff spawned %d git processes, want 3 (add, write-tree, diff)", got)
	}
}

// A baseline that has no kept index, or whose index belongs to another
// repository, or a call with no paths, takes the full route and answers as it
// always did.
func TestScopedDiffFallsBackToTheFullRoute(t *testing.T) {
	f := turnFixture(t)
	ctx := context.Background()
	paths := []string{"mod.txt", "fresh.txt"}
	want, _, err := f.r.scopedDiff(ctx, paths, f.withoutSeed())
	if err != nil {
		t.Fatal(err)
	}

	foreign := f.baseline
	foreign.seed = &baselineSeed{root: filepath.Join(f.root, "elsewhere"), index: f.baseline.seed.index}
	corrupt := f.baseline
	corrupt.seed = &baselineSeed{root: f.root, index: []byte("not an index")}

	for name, b := range map[string]Baseline{"another repository's index": foreign, "an unreadable index": corrupt} {
		before := procSpawns.Load()
		got, _, err := f.r.scopedDiff(ctx, paths, b)
		if err != nil || got != want {
			t.Errorf("%s: diff differs from the full route's (err %v)\n%s", name, err, got)
		}
		if spawned := procSpawns.Load() - before; spawned < 4 {
			t.Errorf("%s: spawned %d git processes; the full route is not what ran", name, spawned)
		}
	}

	// No paths is the whole diff, not an add of the whole tree.
	whole, _, err := f.r.scopedDiff(ctx, nil, f.baseline)
	if err != nil {
		t.Fatal(err)
	}
	wholeWant, _, err := f.r.scopedDiff(ctx, nil, f.withoutSeed())
	if err != nil || whole != wholeWant {
		t.Fatalf("an empty path list gave a different diff (err %v)", err)
	}
}

// The kept index is a copy, and using it leaves the real index, the working
// tree and the baseline's own copy exactly as they were: a second look at the
// same turn, a bounce's re-check, sees the same starting point.
func TestScopedDiffLeavesTheRealIndexAndTheKeptIndexAlone(t *testing.T) {
	f := turnFixture(t)
	// Status first: it refreshes the real index's stat data, and the index read
	// below must be the one the diff starts from.
	status := gitIn(t, f.root, "status", "--porcelain")
	realIndex := filepath.Join(f.root, ".git", "index")
	before, err := os.ReadFile(realIndex)
	if err != nil {
		t.Fatal(err)
	}
	kept := bytes.Clone(f.baseline.seed.index)

	ctx := context.Background()
	first, _, err := f.r.scopedDiff(ctx, []string{"mod.txt", "dir"}, f.baseline)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := f.r.scopedDiff(ctx, []string{"mod.txt", "dir"}, f.baseline)
	if err != nil || second != first {
		t.Fatalf("a second look at the same turn differs (err %v)", err)
	}

	after, err := os.ReadFile(realIndex)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the repository's own index changed (err %v)", err)
	}
	if !bytes.Equal(kept, f.baseline.seed.index) {
		t.Fatal("the baseline's kept index was modified by a diff")
	}
	if got := gitIn(t, f.root, "status", "--porcelain"); got != status {
		t.Fatalf("git status changed:\n%s\n--- was ---\n%s", got, status)
	}
}

// A baseline over a repository with no commits keeps its index too, and the
// incremental diff agrees with the full route there.
func TestScopedDiffFromTheKeptIndexInAnUnbornRepository(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-q")
	write(t, root, "keep.txt", "keep\n")
	r := benchRegistry(root)
	baseline := r.CaptureBaseline(context.Background())
	if !baseline.Captured() || baseline.seed == nil {
		t.Fatalf("no baseline with a kept index: %+v", baseline)
	}
	write(t, root, "keep.txt", "keep, edited\n")
	write(t, root, "made.txt", "made this turn\n")

	paths := []string{"keep.txt", "made.txt"}
	want, _, err := r.scopedDiff(context.Background(), paths, Baseline{Tree: baseline.Tree})
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := r.scopedDiff(context.Background(), paths, baseline)
	if err != nil || got != want || !strings.Contains(got, "made.txt") {
		t.Fatalf("unborn repository: diff differs from the full route's (err %v)\nwant:\n%s\ngot:\n%s", err, want, got)
	}
}

// An index too large to hold is not kept, and the baseline is still whole.
func TestBaselineDoesNotKeepAnOversizeIndex(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-q")
	write(t, root, "a.txt", "a\n")
	r := benchRegistry(root)
	ctx := context.Background()

	tree, kept, _, err := r.recordWorktree(ctx, true, true)
	if err != nil || tree == "" || kept == nil {
		t.Fatalf("a small index was not kept: tree %q, %d bytes, err %v", tree, len(kept), err)
	}
	index := filepath.Join(t.TempDir(), "index")
	if err := os.WriteFile(index, bytes.Repeat([]byte{'x'}, maxSeedIndexBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := readSeedIndex(index); got != nil {
		t.Fatalf("an index of %d bytes was kept; the bound is %d", len(got), maxSeedIndexBytes)
	}
}

// The kept index has to carry its modified time with it, for the reason
// TestSeededBaselineSeesASameSizedEditMadeInTheSecondTheIndexWasWritten gives:
// written to a new file stamped later, an entry that was racy when the baseline
// was recorded looks settled, and a same-sized edit made in that second is
// diffed as the old content.
//
// The file is stamped one second ahead, so its entry is as new as the index
// git wrote when the baseline was recorded; the test then waits until the clock
// has passed it, so a copy stamped "now" would be strictly newer than the
// entry. Stat checking is reduced to mtime and size, and the edit keeps both,
// so the racy rule is the only thing that can see it.
func TestScopedDiffFromTheKeptIndexSeesASameSizedEditInTheSecondOfTheCapture(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-q")
	gitIn(t, root, "config", "core.checkStat", "minimal")
	gitIn(t, root, "config", "core.trustctime", "false")
	ahead := time.Now().Add(time.Second)

	write(t, root, "f.dat", "aaaaaa")
	if err := os.Chtimes(filepath.Join(root, "f.dat"), ahead, ahead); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-qm", "seed")

	r := benchRegistry(root)
	baseline := r.CaptureBaseline(context.Background())
	if !baseline.Captured() || baseline.seed == nil {
		t.Fatalf("no baseline with a kept index: %+v", baseline)
	}
	time.Sleep(time.Until(ahead) + 1200*time.Millisecond)

	write(t, root, "f.dat", "bbbbbb")
	if err := os.Chtimes(filepath.Join(root, "f.dat"), ahead, ahead); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	want, _, err := r.scopedDiff(ctx, []string{"f.dat"}, Baseline{Tree: baseline.Tree})
	if err != nil || !strings.Contains(want, "+bbbbbb") {
		t.Fatalf("the full route does not see the edit (err %v):\n%s", err, want)
	}
	got, _, err := r.scopedDiff(ctx, []string{"f.dat"}, baseline)
	if err != nil || got != want {
		t.Fatalf("the diff from the kept index missed the edit (err %v)\nwant:\n%s\ngot:\n%s", err, want, got)
	}
}

// The one place the kept-index route and the full route part, and the kept
// index is the one that is right. A file flagged assume-unchanged or
// skip-worktree during the turn is skipped by `git add` when the index it is
// added to carries the flag. The full route seeds from the repository's index
// as it stands at the end of the turn, so it carries the flag and misses the
// edit; the kept index predates the flag and sees it. A cold read of the tree,
// which trusts no stat data and no flags, is the oracle.
//
// A flag set before the baseline was taken is in both indexes and both miss the
// edit; that is not asserted here because it is not changed here.
func TestScopedDiffSeesAFileFlaggedAssumeUnchangedDuringTheTurn(t *testing.T) {
	f := turnFixture(t)
	ctx := context.Background()
	gitIn(t, f.root, "update-index", "--assume-unchanged", "mod.txt")
	gitIn(t, f.root, "update-index", "--skip-worktree", "dir/a.txt")
	paths := []string{"mod.txt", "dir/a.txt"}

	cold, err := f.r.worktreeTree(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	oracle, err := runGit(ctx, f.root, "diff", f.baseline.Tree, cold, "--", "mod.txt", "dir/a.txt")
	if err != nil || !strings.Contains(oracle, "+TWO") || !strings.Contains(oracle, "+a, edited") {
		t.Fatalf("the cold oracle does not show the turn's edits (err %v):\n%s", err, oracle)
	}

	got, notes, err := f.r.scopedDiff(ctx, paths, f.baseline)
	if err != nil || len(notes) != 0 {
		t.Fatalf("kept-index route: %v %v", err, notes)
	}
	if got != oracle {
		t.Fatalf("the kept-index route differs from the cold oracle.\ncold:\n%s\nkept index:\n%s", oracle, got)
	}
	// Recorded rather than asserted: the full route is expected to miss it, but
	// that is git's behaviour to change, not this package's contract.
	if old, _, err := f.r.scopedDiff(ctx, paths, f.withoutSeed()); err == nil && old == oracle {
		t.Log("the full route now agrees with the cold oracle on flagged files too")
	}
}
