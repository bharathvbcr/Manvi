package devcouncil

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- E1: the verifier's git capture had no ceiling ---------------------------
//
// runGit used cmd.Output(), which accumulates a child's entire stdout in a
// bytes.Buffer that grows until the process runs out of memory. Every diff the
// verifier takes goes through it, and how big that diff is was decided by the
// content of the working tree — which on this path is content an agent has
// just written. The sibling paths already had ceilings: patchFile at
// maxPatchReadBytes, gatedGit at maxGitOutputBytes. This one had none.

// writeOversizedFile puts a file past maxGitCaptureBytes in the tree, made of
// distinct lines so a cut can be seen to land on a line boundary.
func writeOversizedFile(t *testing.T, root, name string) {
	t.Helper()
	var b strings.Builder
	// Each line is 64 bytes; enough of them to run well past the cap.
	line := strings.Repeat("a", 63) + "\n"
	b.Grow(maxGitCaptureBytes + (4 << 20))
	for b.Len() < maxGitCaptureBytes+(4<<20) {
		b.WriteString(line)
	}
	if err := os.WriteFile(filepath.Join(root, name), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runGit must refuse an output it cannot hold rather than hand back a partial
// one. A diff cut mid-hunk still parses, still names files, and reads to every
// gate downstream exactly like a complete diff — which is the failure this
// codebase names as "a capped sample presented as complete coverage".
func TestRunGitRefusesAnOutputPastTheCapRatherThanTruncatingIt(t *testing.T) {
	root := newGitRepo(t)
	writeOversizedFile(t, root, "huge.txt")

	out, err := runGit(context.Background(), root,
		"diff", "--no-index", "--", "/dev/null", "huge.txt")
	if err == nil {
		t.Fatalf("runGit returned %d bytes and no error for an output past the %d-byte cap; "+
			"an unbounded read reported success", len(out), maxGitCaptureBytes)
	}
	if out != "" {
		t.Errorf("runGit returned %d bytes alongside its refusal; a caller that ignores the "+
			"error still gets a partial diff", len(out))
	}
	if !strings.Contains(err.Error(), "discarded") {
		t.Errorf("the refusal does not say what was dropped: %v", err)
	}
}

// And the caller that CAN carry a degradation must carry it. gitDiff has a
// notes channel that reaches Report.Degraded, so a truncated capture becomes a
// named shortfall instead of a silent one — and the diff it does return is
// bounded and ends on a line boundary, so half a "+++ b/path" header cannot be
// read as a header naming a file nobody wrote.
func TestTheWorkingTreeDiffIsBoundedAndNamesWhatItCouldNotCover(t *testing.T) {
	root := newGitRepo(t)
	writeOversizedFile(t, root, "huge.txt")

	diff, notes, err := gitDiff(context.Background(), root)
	if err != nil {
		t.Fatalf("gitDiff: %v", err)
	}
	// Bounded: the tracked diff is empty here, so the whole of this is the
	// untracked render, which shares one capture budget.
	if len(diff) > maxGitCaptureBytes+(1<<20) {
		t.Errorf("gitDiff returned %d bytes against a %d-byte budget; the capture is unbounded",
			len(diff), maxGitCaptureBytes)
	}
	joined := strings.Join(notes, " | ")
	if !strings.Contains(joined, "untracked_diff") || !strings.Contains(joined, "discarded") {
		t.Fatalf("a truncated diff was returned with no degradation naming it: %q", joined)
	}
	if diff != "" && !strings.HasSuffix(diff, "\n") {
		t.Error("the diff was cut mid-line; a half-written header parses as a header naming " +
			"a path that does not exist")
	}
}

// The bound must not be a blanket refusal: an ordinary working tree still
// produces a complete diff and no degradation at all. Without this the test
// above is satisfied by a gitDiff that always reports itself truncated.
func TestAnOrdinaryWorkingTreeDiffIsStillCompleteAndUndegraded(t *testing.T) {
	root := newGitRepo(t)
	write(t, root, "seed.txt", "seed\nchanged\n")
	write(t, root, "fresh.txt", "brand new\n")

	diff, notes, err := gitDiff(context.Background(), root)
	if err != nil {
		t.Fatalf("gitDiff: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("a small working tree reported a degradation: %q", notes)
	}
	for _, want := range []string{"seed.txt", "fresh.txt", "brand new"} {
		if !strings.Contains(diff, want) {
			t.Errorf("the diff does not carry %q:\n%s", want, diff)
		}
	}
}
