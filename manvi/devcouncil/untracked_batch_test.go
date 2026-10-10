package devcouncil

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The batch renderer is an optimisation of renderUntracked and must be the old
// answer exactly or step aside. These tests hold it to the per-file loop kept
// verbatim in untracked_reference_test.go.

// batchTree is trickyUntrackedTree without the two things the batch declines
// on purpose: the nested repository, and names git escapes in a header.
func batchTree(t *testing.T) (string, []string) {
	t.Helper()
	root := newGitRepo(t)
	trickyUntrackedTree(t, root)
	if err := os.RemoveAll(filepath.Join(root, "nested")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"link-to-dir", "q\"uote.go", `back\slash.go`, "tab\tname.go", "new\nline.go"} {
		if err := os.Remove(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	return root, untrackedPaths(t, root)
}

func TestBatchRenderIsTheOldAnswerUnderEveryBudget(t *testing.T) {
	root, paths := batchTree(t)
	ctx := context.Background()
	full, _ := renderUntrackedSequential(ctx, root, paths, 1<<30)
	total := 0
	for _, u := range full {
		total += len(u.added)
	}
	budgets := []int{1, 97, 600, 4096, total / 2, total - 1, total, 1 << 30}
	spent := 0
	for i, u := range full {
		spent += len(u.added)
		if i == 0 || i == 5 || i == len(full)/2 {
			budgets = append(budgets, spent)
		}
	}
	for _, budget := range budgets {
		want, wantLeft := renderUntrackedSequential(ctx, root, paths, budget)
		got, gotLeft, ok := renderUntrackedBatch(ctx, root, paths, budget)
		if !ok {
			t.Fatalf("budget %d: the batch declined a tree it should render", budget)
		}
		sameRenders(t, "budget "+itoa(budget), got, want, gotLeft, wantLeft)
	}
}

// What the batch cannot attribute to a file, it hands back to the per-file
// loop rather than guess.
func TestBatchRenderDeclinesWhatItCannotAttribute(t *testing.T) {
	ctx := context.Background()

	nested := newGitRepo(t)
	trickyUntrackedTree(t, nested)
	if _, _, ok := renderUntrackedBatch(ctx, nested, untrackedPaths(t, nested), 1<<30); ok {
		t.Error("a nested repository was rendered by the batch")
	}

	dirLink := newGitRepo(t)
	write(t, dirLink, "sub/in.go", "package p\n")
	if err := os.Symlink("sub", filepath.Join(dirLink, "to-sub")); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := renderUntrackedBatch(ctx, dirLink, untrackedPaths(t, dirLink), 1<<30); ok {
		t.Error("a symlink to a directory was rendered by the batch")
	}

	escaped := newGitRepo(t)
	write(t, escaped, "plain.go", "package p\n")
	write(t, escaped, "q\"uote.go", "package p\n")
	if _, _, ok := renderUntrackedBatch(ctx, escaped, untrackedPaths(t, escaped), 1<<30); ok {
		t.Error("a path git escapes in its header was rendered by the batch")
	}
	// And the public entry point still answers for both, as the loop did.
	for _, root := range []string{nested, escaped, dirLink} {
		paths := untrackedPaths(t, root)
		want, wantLeft := renderUntrackedSequential(ctx, root, paths, 1<<30)
		got, gotLeft := renderUntracked(ctx, root, paths, 1<<30)
		sameRenders(t, "fallback", got, want, gotLeft, wantLeft)
	}
}

// Content that looks like a header must not split a file in two.
func TestBatchRenderIsNotFooledByHeaderLookalikeContent(t *testing.T) {
	root := newGitRepo(t)
	write(t, root, "a.txt", "diff --git a/b.txt b/b.txt\n+diff --git a/c.txt b/c.txt\n")
	write(t, root, "b.txt", "real\n")
	paths := untrackedPaths(t, root)
	ctx := context.Background()
	want, wantLeft := renderUntrackedSequential(ctx, root, paths, 1<<30)
	got, gotLeft, ok := renderUntrackedBatch(ctx, root, paths, 1<<30)
	if !ok {
		t.Fatal("the batch declined")
	}
	sameRenders(t, "lookalike", got, want, gotLeft, wantLeft)
}

// Attribute and config combinations that change what git prints for a file.
func TestBatchRenderMatchesUnderTextAttributesAndAutocrlf(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, root string){
		"autocrlf": func(t *testing.T, root string) { gitIn(t, root, "config", "core.autocrlf", "true") },
		"eol-attr": func(t *testing.T, root string) {
			write(t, root, ".gitattributes", "*.txt text eol=crlf\n*.bin binary\n")
		},
		"textconv-off": func(t *testing.T, root string) {
			write(t, root, ".gitattributes", "*.txt diff=nope\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := newGitRepo(t)
			setup(t, root)
			write(t, root, "unix.txt", "one\ntwo\n")
			write(t, root, "dos.txt", "one\r\ntwo\r\n")
			write(t, root, "mixed.txt", "one\r\ntwo\n")
			write(t, root, "x.bin", "\x00\x01bin\n")
			write(t, root, "z.go", "package z")
			paths := untrackedPaths(t, root)
			ctx := context.Background()
			want, wantLeft := renderUntrackedSequential(ctx, root, paths, 1<<30)
			got, gotLeft, ok := renderUntrackedBatch(ctx, root, paths, 1<<30)
			if !ok {
				t.Fatalf("the batch declined under %s", name)
			}
			sameRenders(t, name, got, want, gotLeft, wantLeft)
		})
	}
}

// A config that changes how git prints the header is one the batch's header
// check cannot vouch for; it declines and the per-file path answers.
func TestBatchRenderDeclinesWhenTheHeaderFormatIsReconfigured(t *testing.T) {
	root := newGitRepo(t)
	gitIn(t, root, "config", "diff.noprefix", "true")
	write(t, root, "a.go", "package a\n")
	ctx := context.Background()
	paths := untrackedPaths(t, root)
	if _, _, ok := renderUntrackedBatch(ctx, root, paths, 1<<30); ok {
		t.Fatal("the batch rendered under diff.noprefix")
	}
	want, wantLeft := renderUntrackedSequential(ctx, root, paths, 1<<30)
	got, gotLeft := renderUntracked(ctx, root, paths, 1<<30)
	sameRenders(t, "noprefix", got, want, gotLeft, wantLeft)
}

// 256 untracked files were 258 git processes. The batch is a fixed handful,
// whatever the count.
func TestBatchRenderSpawnBound(t *testing.T) {
	root := benchRepo(t, 20, 256, 0)
	before := procSpawns.Load()
	if _, _, err := gitDiff(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if got := procSpawns.Load() - before; got > 10 {
		t.Fatalf("gitDiff over 256 untracked files started %d git processes, bound is 10", got)
	}
}
