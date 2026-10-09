package tools

import (
	"fmt"
	"testing"
)

// The same path written repeatedly is one file to verify, and must not consume
// the cap.
func TestWrittenPathsAreDeduplicated(t *testing.T) {
	have, truncated := []string(nil), false
	for range 1000 {
		have, truncated = MergeWrites(have, truncated, []string{"a.go", "b.go"})
	}
	if len(have) != 2 {
		t.Fatalf("tracked %d paths for two distinct files", len(have))
	}
	if truncated {
		t.Fatal("de-duplication reported a truncation that did not happen")
	}
}

// Junk from a handler must not enter the set. A path that is empty or is
// nothing but whitespace names no file, and carrying it would produce a check
// against "".
func TestWrittenPathsRejectEmptyEntries(t *testing.T) {
	have, truncated := MergeWrites(nil, false, []string{"", "   ", "\t\n", "real.go"})
	if len(have) != 1 || have[0] != "real.go" {
		t.Fatalf("tracked %v, want only the real path", have)
	}
	if truncated {
		t.Fatal("dropping blanks was reported as a truncation")
	}
}

// Once truncated, always truncated: a later call that happens to fit must not
// clear the flag and make the set look complete again.
func TestTruncationIsSticky(t *testing.T) {
	var have []string
	truncated := false
	for i := range MaxTrackedWrites + 5 {
		have, truncated = MergeWrites(have, truncated, []string{fmt.Sprintf("f%d.go", i)})
	}
	if !truncated {
		t.Fatal("the cap was hit without being recorded")
	}
	have, truncated = MergeWrites(have, truncated, []string{"a.go"})
	if !truncated {
		t.Fatal("a later call cleared the truncation flag, so an incomplete set now claims to " +
			"be complete")
	}
	_ = have
}
