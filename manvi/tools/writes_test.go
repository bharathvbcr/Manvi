package tools

import (
	"fmt"
	"slices"
	"strings"
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
	// A later call that brings nothing new is the case a non-sticky flag gets
	// wrong: a new path re-trips the cap whether or not the flag was kept.
	have, truncated = MergeWrites(have, truncated, []string{"f0.go", " ", ""})
	if !truncated {
		t.Fatal("a later call cleared the truncation flag, so an incomplete set now claims to " +
			"be complete")
	}
	have, truncated = MergeWrites(have, truncated, nil)
	if !truncated {
		t.Fatal("an empty call cleared the truncation flag")
	}
	_ = have
}

// FuzzMergeWrites holds MergeWrites to an exact oracle over any sequence of
// calls: the set is the first-seen order of every distinct non-blank trimmed
// path, cut at MaxTrackedWrites, and it reports truncation exactly when a new
// path arrived after the cut. Every property the checkpoint relies on — no
// blanks, no duplicates, append-only order, a bound that says when it bit — is
// a consequence, so a merge that drifts from any of them fails here.
//
// The input is calls separated by 0x01, paths within a call by 0x00.
func FuzzMergeWrites(f *testing.F) {
	f.Add([]byte("a.go\x00b.go\x01b.go\x00 c.go \x00\x00\t\x01a.go"))
	f.Add([]byte(" \x00\n\x00\r\n\x01"))
	var many []byte
	for i := range MaxTrackedWrites + 3 {
		many = append(many, fmt.Sprintf("f%d.go\x00", i)...)
		if i%50 == 0 {
			many = append(many, 0x01)
		}
	}
	f.Add(many)
	f.Add(append(append([]byte(nil), many...), "\x01f0.go\x00 \x01"...))
	f.Fuzz(func(t *testing.T, data []byte) {
		var have []string
		truncated := false
		var oracle []string
		seen := map[string]bool{}
		for _, call := range strings.Split(string(data), "\x01") {
			add := strings.Split(call, "\x00")
			snapshot := append([]string(nil), add...)
			before := append([]string(nil), have...)
			have, truncated = MergeWrites(have, truncated, add)
			if !slices.Equal(add, snapshot) {
				t.Fatalf("MergeWrites changed the caller's input: %q -> %q", snapshot, add)
			}
			if len(have) < len(before) || !slices.Equal(have[:len(before)], before) {
				t.Fatalf("an earlier path was moved or dropped: %q -> %q", before, have)
			}
			for _, p := range add {
				if p = strings.TrimSpace(p); p != "" && !seen[p] {
					seen[p] = true
					oracle = append(oracle, p)
				}
			}
			// Checked after every call, not only at the end: once cut, the
			// set stays reported as cut whatever later calls bring.
			if wantCut := len(oracle) > MaxTrackedWrites; truncated != wantCut {
				t.Fatalf("after call %q: truncated = %v with %d distinct paths against a cap of %d",
					call, truncated, len(oracle), MaxTrackedWrites)
			}
		}
		want := oracle
		if len(want) > MaxTrackedWrites {
			want = want[:MaxTrackedWrites]
		}
		if !slices.Equal(have, want) {
			t.Fatalf("set = %q\nwant  %q", have, want)
		}
		if wantCut := len(oracle) > MaxTrackedWrites; truncated != wantCut {
			t.Fatalf("truncated = %v with %d distinct paths against a cap of %d",
				truncated, len(oracle), MaxTrackedWrites)
		}
	})
}
