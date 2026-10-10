package devcouncil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The untracked fan-out now runs its renders in parallel, and the claim it
// makes is that nobody reading the diff can tell. These tests hold it to that
// against the old sequential code, kept verbatim in
// untracked_reference_test.go: same diff bytes, same notes in the same order,
// same errors, on trees chosen for the places a renderer goes wrong — names
// git has to escape, binaries, modes, links, a nested repository that cannot
// be rendered at all, and budgets that run out mid-file.

// trickyUntrackedTree fills root with untracked files that each exercise a
// different corner of `git diff --no-index`, plus one tracked modification so
// the tracked half of the diff is not empty.
func trickyUntrackedTree(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"plain.go":                "package main\n",
		"empty.txt":               "",
		"noeol.txt":               "no trailing newline",
		"crlf.txt":                "one\r\ntwo\r\n",
		"bin.dat":                 "\x00\x01\x02 binary\n",
		"opaque.nodiff":           "text the attributes call binary\n",
		".gitattributes":          "*.nodiff -diff\n",
		".gitignore":              "*.log\n",
		"ignored.log":             "never listed\n",
		"my notes.go":             "package main // space\n",
		"dir with space/inner.go": "package inner\n",
		"héllo.go":                "package main // unicode\n",
		"q\"uote.go":              "package main // quote\n",
		`back\slash.go`:           "package main // backslash\n",
		"tab\tname.go":            "package main // tab\n",
		"new\nline.go":            "package main // newline\n",
		"-leading-dash.go":        "package main // dash\n",
		"[brackets].go":           "package main // glob\n",
		"*star.go":                "package main // star\n",
		"a/b/c/deep.txt":          "deep\n",
	}
	for name, body := range files {
		write(t, root, name, body)
	}
	var big strings.Builder
	for i := 0; i < 3000; i++ {
		big.WriteString("a line long enough to make this file worth cutting ")
		big.WriteString(itoa(i))
		big.WriteString("\n")
	}
	write(t, root, "big.txt", big.String())

	write(t, root, "run.sh", "#!/bin/sh\necho hi\n")
	if err := os.Chmod(filepath.Join(root, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		"link-to-plain": "plain.go",
		"dangling":      "nowhere",
		"link-to-dir":   "dir with space",
		"escaping":      "/etc/passwd",
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}

	// A nested repository lists as "nested/", which --no-index cannot render:
	// the error path, which gitDiff skips and scopedDiff names.
	nested := filepath.Join(root, "nested")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = nested
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init nested: %v\n%s", err, out)
	}
	write(t, nested, "inside.txt", "inside\n")

	write(t, root, "seed.txt", "seed\nchanged\n")
}

func untrackedPaths(t *testing.T, root string) []string {
	t.Helper()
	out, err := runGit(context.Background(), root, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// withSpeculativeCap shrinks the per-render capture so the re-render path is
// taken as often as the cut-down one.
func withSpeculativeCap(t *testing.T, limit int) {
	t.Helper()
	saved := untrackedSpeculativeCap
	untrackedSpeculativeCap = limit
	t.Cleanup(func() { untrackedSpeculativeCap = saved })
}

func sameRenders(t *testing.T, label string, got, want []untrackedRender, gotLeft, wantLeft int) {
	t.Helper()
	if gotLeft != wantLeft {
		t.Errorf("%s: unrendered = %d, want %d", label, gotLeft, wantLeft)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: %d renders, want %d", label, len(got), len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.path != w.path || g.added != w.added || g.note != w.note {
			t.Errorf("%s: render %d (%q) differs:\n got  note=%q added=%q\n want note=%q added=%q",
				label, i, w.path, g.note, g.added, w.note, w.added)
		}
		if (g.err == nil) != (w.err == nil) || (g.err != nil && g.err.Error() != w.err.Error()) {
			t.Errorf("%s: render %d (%q) error = %v, want %v", label, i, w.path, g.err, w.err)
		}
	}
}

// TestRenderUntrackedAnswersAsTheSequentialLoopDid drives the renderer and the
// old loop over the same files under budgets from one byte to more than the
// whole tree, with the speculative capture shrunk far enough that every
// settling path — whole, cut down, re-rendered, failed — is taken.
func TestRenderUntrackedAnswersAsTheSequentialLoopDid(t *testing.T) {
	root := newGitRepo(t)
	trickyUntrackedTree(t, root)
	paths := untrackedPaths(t, root)
	ctx := context.Background()

	full, left := renderUntrackedSequential(ctx, root, paths, 1<<30)
	if left != 0 {
		t.Fatalf("an unbounded budget left %d files unrendered", left)
	}
	total, failed := 0, 0
	for _, u := range full {
		total += len(u.added)
		if u.err != nil {
			failed++
		}
	}
	if failed == 0 {
		t.Fatal("no render failed, so the error path is not being compared; the nested repository should fail")
	}

	budgets := []int{0, 1, 97, 600, 4096, total / 2, total - 1, total, 1 << 30}
	// Budgets that run out exactly at a file boundary, which is the only way
	// the loop reports files as unrendered rather than cut.
	spent := 0
	for i, u := range full {
		spent += len(u.added)
		if i == 0 || i == 5 || i == len(full)/2 {
			budgets = append(budgets, spent)
		}
	}
	sawUnrendered := false
	for _, limit := range []int{1, 97, 1 << 20} {
		withSpeculativeCap(t, limit)
		for _, budget := range budgets {
			want, wantLeft := renderUntrackedSequential(ctx, root, paths, budget)
			got, gotLeft := renderUntracked(ctx, root, paths, budget)
			sameRenders(t, "cap "+itoa(limit)+" budget "+itoa(budget), got, want, gotLeft, wantLeft)
			if wantLeft > 0 && budget > 0 {
				sawUnrendered = true
			}
		}
	}
	if !sawUnrendered {
		t.Fatal("no budget left a file unrendered, so that path was not compared")
	}
}

// TestGitDiffAnswersAsTheSequentialFanOutDid compares the whole of gitDiff's
// answer — diff and notes — on the tricky tree, and on one with more
// untracked files than the render cap.
func TestGitDiffAnswersAsTheSequentialFanOutDid(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, root string) {
		t.Helper()
		wantDiff, wantNotes, wantErr := gitDiffSequential(ctx, root)
		gotDiff, gotNotes, gotErr := gitDiff(ctx, root)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("error = %v, want %v", gotErr, wantErr)
		}
		if gotDiff != wantDiff {
			t.Errorf("diff differs from the sequential fan-out:\n got %d bytes\nwant %d bytes", len(gotDiff), len(wantDiff))
		}
		if !reflect.DeepEqual(gotNotes, wantNotes) {
			t.Errorf("notes differ:\n got %q\nwant %q", gotNotes, wantNotes)
		}
	}

	t.Run("tricky", func(t *testing.T) {
		root := newGitRepo(t)
		trickyUntrackedTree(t, root)
		check(t, root)
		withSpeculativeCap(t, 97)
		check(t, root)
	})
	t.Run("past the render cap", func(t *testing.T) {
		root := newGitRepo(t)
		for i := 0; i < maxUntrackedRendered+40; i++ {
			write(t, root, filepath.Join("many", "f"+itoa(i)+".go"), "package many\n")
		}
		check(t, root)
	})
}

// TestScopedDiffFallbackAnswersAsTheSequentialFanOutDid is the same comparison
// for scopedDiff without a baseline, which is the path that renders untracked
// files — and the one that names a file it could not render.
func TestScopedDiffFallbackAnswersAsTheSequentialFanOutDid(t *testing.T) {
	root := newGitRepo(t)
	trickyUntrackedTree(t, root)
	r := &Registry{deps: Deps{Root: root}}
	paths := append([]string{"seed.txt"}, untrackedPaths(t, root)...)
	ctx := context.Background()

	for _, limit := range []int{97, 1 << 20} {
		withSpeculativeCap(t, limit)
		wantDiff, wantNotes, wantErr := r.scopedDiffSequential(ctx, paths, Baseline{})
		gotDiff, gotNotes, gotErr := r.scopedDiff(ctx, paths, Baseline{})
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("error = %v, want %v", gotErr, wantErr)
		}
		if gotDiff != wantDiff {
			t.Errorf("cap %d: diff differs:\n got %d bytes\nwant %d bytes", limit, len(gotDiff), len(wantDiff))
		}
		if !reflect.DeepEqual(gotNotes, wantNotes) {
			t.Errorf("cap %d: notes differ:\n got %q\nwant %q", limit, gotNotes, wantNotes)
		}
		if !strings.Contains(strings.Join(gotNotes, "\n"), "nested/ could not be rendered") {
			t.Errorf("the unrenderable nested repository was not named: %q", gotNotes)
		}
	}
}

// TestGitDiffBudgetNotesAnswerAsBefore runs the real 16 MiB budget out, and the
// cut notes — with their byte counts — must read exactly as the sequential
// loop wrote them. That includes the loop's own quirk: cutting a file back to
// a whole line returns the tail to the budget, so the files after the cut are
// each rendered into those few bytes and reported cut, rather than reported
// unrendered. That is the old answer, so it is the answer.
func TestGitDiffBudgetNotesAnswerAsBefore(t *testing.T) {
	if testing.Short() {
		t.Skip("writes about 20 MiB of untracked files")
	}
	root := newGitRepo(t)
	line := strings.Repeat("x", 120) + "\n"
	body := strings.Repeat(line, (6<<20)/len(line))
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		write(t, root, name, body)
	}
	write(t, root, "d.txt", "after the budget\n")
	write(t, root, "e.txt", "also after the budget\n")

	ctx := context.Background()
	wantDiff, wantNotes, _ := gitDiffSequential(ctx, root)
	gotDiff, gotNotes, _ := gitDiff(ctx, root)
	if gotDiff != wantDiff {
		t.Errorf("diff differs: got %d bytes, want %d", len(gotDiff), len(wantDiff))
	}
	if !reflect.DeepEqual(gotNotes, wantNotes) {
		t.Errorf("notes differ:\n got %q\nwant %q", gotNotes, wantNotes)
	}
	joined := strings.Join(gotNotes, "\n")
	if !strings.Contains(joined, "c.txt [output truncated:") || !strings.Contains(joined, "e.txt [output truncated:") {
		t.Fatalf("the budget did not run out where this test needs it to: %q", gotNotes)
	}
}

// BenchmarkGitDiffUntracked is the case the parallel fan-out exists for: a few
// hundred new files, each one git process.
func BenchmarkGitDiffUntracked(b *testing.B) {
	if _, err := exec.LookPath("git"); err != nil {
		b.Skip("git is not on PATH")
	}
	root := b.TempDir()
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "b@test"}, {"config", "user.name", "b"},
		{"commit", "-q", "--allow-empty", "-m", "seed"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			b.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	for i := 0; i < 300; i++ {
		if err := os.WriteFile(filepath.Join(root, "f"+itoa(i)+".go"), []byte("package f\n"), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	ctx := context.Background()
	b.Run("parallel", func(b *testing.B) {
		for b.Loop() {
			if _, _, err := gitDiff(ctx, root); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("sequential", func(b *testing.B) {
		for b.Loop() {
			if _, _, err := gitDiffSequential(ctx, root); err != nil {
				b.Fatal(err)
			}
		}
	})
}
