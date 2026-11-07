package devcouncil

import (
	"strings"
	"testing"
)

// oldPorcelainHasUserChanges is the pre-w10-debt loop (a3710a1), kept verbatim
// so the rewrite is compared against it rather than against a restatement.
// The only edit is dropping a dead `i++` before a `return true`.
func oldPorcelainHasUserChanges(porcelain string) bool {
	entries := strings.Split(porcelain, "\x00")
	for i := 0; i < len(entries); i++ {
		rec := entries[i]
		if rec == "" {
			continue
		}
		if len(rec) < 4 || rec[2] != ' ' {
			return true
		}
		xy := rec[:2]
		path := rec[3:]
		if xy[0] == 'R' || xy[0] == 'C' || xy[1] == 'R' || xy[1] == 'C' {
			return true
		}
		if xy == "??" && isHarnessStateRelPath(path) {
			continue
		}
		return true
	}
	return false
}

func TestAdvW10PorcelainRewriteMatchesTheLoopItReplaced(t *testing.T) {
	recs := []string{
		"", "?? .devcouncil/x", "?? .devmap/y", "?? src/a.go", " M a.go", "R  new.go", "R  new.go\x00old.go",
		"C  c.go", " R r.go", " C r.go", "RM x", "R", "ab", "?? ", "??x", ".devcouncil/old",
	}
	var gen func(prefix string, depth int)
	gen = func(prefix string, depth int) {
		if got, want := porcelainHasUserChanges(prefix), oldPorcelainHasUserChanges(prefix); got != want {
			t.Fatalf("porcelain %q: got %v, old %v", prefix, got, want)
		}
		if depth == 0 {
			return
		}
		for _, r := range recs {
			gen(prefix+r+"\x00", depth-1)
			gen(prefix+r, depth-1)
		}
	}
	gen("", 3)
	// A rename as the last record, with and without its origin, and with only
	// harness-state records before it.
	for _, in := range []string{"R  a", "R  a\x00", "?? .devcouncil/x\x00R  a", "?? .devcouncil/x\x00R  a\x00.devcouncil/o\x00"} {
		if !porcelainHasUserChanges(in) {
			t.Errorf("%q: a rename is a user change", in)
		}
	}
}
