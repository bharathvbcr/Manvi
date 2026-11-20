package devcouncil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A diff that could not be read in full is named in the answer of the two tools
// an agent polls between edits, with a verdict that is not a pass. Before, only
// devcouncil_verify_task carried either field, so an agent reading the gap list
// alone saw an empty list and no reason to think the check was partial.
func TestGapAndNextActionToolsCarryTheVerdictAndTheDiffNote(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})
	if err := os.MkdirAll(filepath.Join(f.root, "src"), 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, f.root, "src/calc.go", "package calc\n")
	// The untracked-file listing fails, so the diff is the tracked one alone.
	shimGit(t, `for a in "$@"; do
  if [ "$a" = "ls-files" ]; then echo 'fatal: cannot list' >&2; exit 128; fi
done`)

	for _, name := range []string{"devcouncil_get_gaps", "devcouncil_get_next_actions"} {
		body := f.payload(name, map[string]string{})
		verdict, _ := body["verdict"].(string)
		if verdict == "" || verdict == VerdictPassed {
			t.Errorf("%s: verdict = %q, want a non-passed one: %v", name, body["verdict"], body)
		}
		degraded, ok := body["degraded"].([]any)
		if !ok || len(degraded) == 0 {
			t.Errorf("%s: degraded does not carry the diff note: %v", name, body["degraded"])
			continue
		}
		if first, _ := degraded[0].(string); !strings.Contains(first, "untracked_diff") {
			t.Errorf("%s: degraded does not name the diff note: %v", name, degraded)
		}
	}
}

// The leased report and VerifyPaths settle one rule: no verifier is degraded in
// both, a coverage-unmeasured note alone is not.
func TestReportAndVerifyPathsSettleANoVerifierRunTheSameWay(t *testing.T) {
	if got := settleVerdict(false, true); got != VerdictDegraded {
		t.Fatalf("owed check = %q", got)
	}
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})
	write(t, f.root, "main.go", "package main\n")
	f.reg.deps.VerifierBinary = ""
	f.reg.deps.CoverageFile = ""

	report, err := f.reg.report(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || report.Verdict == VerdictPassed {
		t.Fatalf("report with no verifier = %q passed=%v: %+v", report.Verdict, report.Passed, report.Degraded)
	}
	paths := f.reg.VerifyPaths(t.Context(), []string{"main.go"}, "", Baseline{})
	if paths.Verdict == VerdictPassed {
		t.Fatalf("VerifyPaths with no verifier passed: %+v", paths)
	}
}
