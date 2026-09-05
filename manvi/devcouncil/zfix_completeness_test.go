package devcouncil

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// A required path the cap dropped is a check that was owed and did not run, and
// the verdict has to say so.
//
// The planted credential is what makes this concrete rather than procedural.
// The rigor gates block on it, so a report that examined the file would fail;
// the file sorts past the cap, so nothing opened it — and the whole turn came
// back "passed", with no findings and no degradation, because settle() read
// Findings and Degraded and the omission was recorded in neither.
func TestCappedPathsCannotProduceAPass(t *testing.T) {
	f := newFixture(t)
	var paths []string
	for i := 0; i < maxVerifiedPaths+72; i++ {
		rel := fmt.Sprintf("src/f%03d.go", i)
		writeRepoFile(t, f.root, rel, fmt.Sprintf("package src\n\nfunc F%03d() {}\n", i))
		paths = append(paths, rel)
	}
	last := fmt.Sprintf("src/f%03d.go", maxVerifiedPaths+71)
	writeRepoFile(t, f.root, last,
		"package src\n\nconst k = \"sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAA\"\n")

	got := f.reg.VerifyPaths(context.Background(), paths, "", Baseline{})

	if len(got.Examined) != maxVerifiedPaths {
		t.Fatalf("examined %d, want the cap of %d", len(got.Examined), maxVerifiedPaths)
	}
	if got.Verdict == VerdictPassed {
		t.Fatalf("%d paths were never examined and the verdict is %q: %+v",
			len(got.Omitted), got.Verdict, got)
	}
	if len(got.Omitted) == 0 {
		t.Fatal("the dropped paths were not recorded as omissions")
	}
	// The omission has to name a path and a reason, or a reader cannot act on
	// it — re-running against the rest is the remedy and it needs the list.
	if !strings.Contains(strings.Join(got.Omitted, "\n"), "examined the first") {
		t.Errorf("omissions should say why they were dropped: %v", got.Omitted)
	}
}

// The counterpart: the harness's own bookkeeping was never owed a check, so
// excluding it must not degrade anything. Without this the fix above would turn
// every ordinary turn degraded and the verdict would stop meaning anything.
func TestIntentionalExclusionsDoNotDegrade(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, "src/a.go", "package src\n\nfunc A() {}\n")
	writeRepoFile(t, f.root, ".devcouncil/notes.md", "# plan\n")

	got := f.reg.VerifyPaths(context.Background(), []string{"src/a.go", ".devcouncil/notes.md"}, "", Baseline{})
	if len(got.Skipped) == 0 {
		t.Fatal("harness bookkeeping should be recorded as a deliberate exclusion")
	}
	if len(got.Omitted) != 0 {
		t.Fatalf("bookkeeping was never owed a check and must not count as omitted: %v", got.Omitted)
	}
	if got.Verdict != VerdictPassed {
		t.Fatalf("verdict = %q, want passed: the only exclusion was deliberate (%+v)", got.Verdict, got)
	}
}
