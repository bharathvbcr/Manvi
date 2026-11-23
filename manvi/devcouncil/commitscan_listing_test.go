package devcouncil

import (
	"fmt"
	"strings"
	"testing"
)

// emptyBlobSHA1 is the id of the empty blob in a SHA-1 repository.
const emptyBlobSHA1 = "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"

// trackManyLongPaths commits count tracked paths of pathLen bytes each without
// creating a file: the index is written directly, every entry names the empty
// blob, and skip-worktree keeps git from reporting the missing files as
// deletions. The tree's `ls-files` listing is then count*(pathLen+1) bytes at
// a cost of one index file and one tree object, not a million inodes.
func trackManyLongPaths(t *testing.T, f *fixture, count, pathLen int) {
	t.Helper()
	ctx := t.Context()
	if _, err := runGitStdin(ctx, f.root, "", "hash-object", "-w", "--stdin"); err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("n", pathLen-len("long/00000-"))
	var in, names strings.Builder
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("long/%05d-%s", i, pad)
		fmt.Fprintf(&in, "100644 %s\t%s\n", emptyBlobSHA1, name)
		names.WriteString(name)
		names.WriteByte(0)
	}
	if _, err := runGitStdin(ctx, f.root, in.String(), "update-index", "--add", "--index-info"); err != nil {
		t.Fatalf("writing the index: %v", err)
	}
	if _, err := runGitStdin(ctx, f.root, names.String(), "update-index", "-z", "--skip-worktree", "--stdin"); err != nil {
		t.Fatalf("marking the paths skip-worktree: %v", err)
	}
	run(t, f.root, "commit", "-qm", "many long paths")
}

// A tree whose file listing is longer than the 16 MiB capture cap is still
// committable. The listing is consumed as it arrives and only the paths under a
// secret pattern are kept, so its length is not a limit; it used to be read
// whole, and a cap reached by an honest monorepo refused every commit in it.
func TestCommitAllowsATreeWhoseListingExceedsTheCaptureCap(t *testing.T) {
	const count, pathLen = 80000, 226
	if count*(pathLen+1) <= maxGitCaptureBytes {
		t.Fatalf("the fixture lists %d bytes, not past the %d-byte cap", count*(pathLen+1), maxGitCaptureBytes)
	}
	f := scanFixture(t)
	trackManyLongPaths(t, f, count, pathLen)
	write(t, f.root, "small.txt", "an ordinary change\n")
	res := stageAndCommit(t, f, "small.txt")
	if res.IsError {
		t.Fatalf("a commit in a tree with a %d-byte file listing was refused: %s", count*(pathLen+1), res.Text)
	}
}
