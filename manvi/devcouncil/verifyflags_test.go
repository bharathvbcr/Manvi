package devcouncil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file used to be about two settings. verify.rigor.enabled and
// verify.diff_coverage.enforce were read on the verify path, and the tests
// here asserted that each one moved the line between "this run was checked"
// and "this run was not" — one turning a stubbed diff green, the other
// promoting an unmeasured diff to blocking.
//
// DevCouncil retired both (f7f427b, "retire the two verification switches that
// nothing read"): the gates they named run in dcverify, which runRigor reaches
// by finding the binary, so neither key ever decided whether a gate ran.
// Reading them on this side is what made the belief look founded, and the
// reads are gone with them.
//
// What is asserted below is the behaviour that is left, stated as the
// invariants the switches used to be able to break. They are worth more than
// the switch tests were: each one now says "there is no way to get the other
// answer", which is exactly what an operator was previously promised and did
// not have.
//
// stubbedSource is a diff the stub gate blocks. The `unimplemented!()` marker
// is one of crates/dc-verify's empty-body shapes, which it matches anywhere on
// an added line, and it is the only thing in this file any gate objects to —
// there is no credential here and the fixture's coverage profile covers it. So
// `passed` answers exactly one question: did the stub gate run.
const stubbedSource = "package calc\n\n" +
	"// Add is not written yet: unimplemented!()\n" +
	"func Add(a, b int) int { return 0 }\n"

// verifyAfterWriting checks out the seeded task, writes content to the one
// planned file, and returns the verifier's report.
func verifyAfterWriting(t *testing.T, f *fixture, content string) map[string]any {
	t.Helper()
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})
	if res := f.call("devcouncil_write_file", map[string]string{
		"path": "src/calc.go", "content": content,
	}); res.IsError {
		t.Fatalf("planned write refused: %s", res.Text)
	}
	return f.payload("devcouncil_verify_task", map[string]any{})
}

// reportGaps indexes a report's gaps by gap_type.
func reportGaps(t *testing.T, report map[string]any) map[string][]map[string]any {
	t.Helper()
	raw, _ := report["gaps"].([]any)
	byKind := map[string][]map[string]any{}
	for _, item := range raw {
		gap, _ := item.(map[string]any)
		kind, _ := gap["gap_type"].(string)
		byKind[kind] = append(byKind[kind], gap)
	}
	return byKind
}

// reportDegraded joins a report's degraded entries for substring assertions.
func reportDegraded(report map[string]any) string {
	raw, _ := report["degraded"].([]any)
	return fmt.Sprint(raw...)
}

// TestTheStubGateAlwaysRunsAndRefuses replaces the test that asserted
// verify.rigor.enabled could turn this run green.
//
// The stub gate is reached by finding the dcverify binary and by nothing else,
// so a stubbed diff is refused on every run that reaches it. There is no
// longer a supported way to get `passed: true` out of this diff, which is the
// property the old setting undermined: it dropped the gate's findings on the
// host side, after dcverify had already produced them, and reported a run that
// checked less as one that checked and was satisfied.
func TestTheStubGateAlwaysRunsAndRefuses(t *testing.T) {
	f := newFixture(t)
	report := verifyAfterWriting(t, f, stubbedSource)

	if report["passed"] != false {
		t.Fatalf("a stubbed diff passed verification: %v", report)
	}
	gaps := reportGaps(t, report)["stub_detection"]
	if len(gaps) == 0 {
		t.Fatalf("no stub_detection gap for a diff containing an empty body: %v", report)
	}

	// Nothing on this path suppresses a gate any more. The degradations that
	// remain legitimate are about inputs and reachability — an absent coverage
	// profile, a verifier that could not be run — so a degradation claiming a
	// gate was switched off would mean the host had started dropping findings
	// again.
	degraded := reportDegraded(report)
	for _, marker := range []string{"suppressed by", "verify.rigor.enabled"} {
		if strings.Contains(degraded, marker) {
			t.Errorf("a gate was reported as suppressed, which no setting can now do: %q", degraded)
		}
	}
}

// TestACredentialAlwaysBlocks is the security boundary that outlived the
// setting.
//
// The old test reached it through verify.rigor.enabled=false, checking that
// quieting the stub gate did not take the credential scanner down with it.
// With no switch to set, the invariant is simpler and stronger: a credential in
// an added line blocks, and no configuration reachable from here changes that.
func TestACredentialAlwaysBlocks(t *testing.T) {
	f := newFixture(t)
	report := verifyAfterWriting(t, f, "package calc\n\n"+
		"const key = \"sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAA\"\n")

	secrets := reportGaps(t, report)["secret_scan"]
	if len(secrets) == 0 {
		t.Fatalf("the credential scanner produced no finding: %v", report)
	}
	if secrets[0]["blocking"] != true {
		t.Errorf("a credential finding must block: %v", secrets[0])
	}
	if report["passed"] == true {
		t.Fatalf("a diff containing a credential passed verification: %v", report)
	}
}

// TestAnUnmeasuredDiffIsReportedAndNeverBlocks replaces the enforce test.
//
// Unmeasured is a statement about the harness's inputs — no coverage data was
// supplied — rather than about the change, so it is always reported and never
// stops the task. The reporting half is the half that matters and the half the
// retired setting never touched: the distinction between "covered" and "never
// measured" survives either way, so an operator reading this gap is not being
// told the added lines ran.
func TestAnUnmeasuredDiffIsReportedAndNeverBlocks(t *testing.T) {
	const source = "package calc\n\nfunc Add(a, b int) int { return a + b }\n"

	f := newFixture(t)
	f.reg.deps.CoverageFile = ""
	report := verifyAfterWriting(t, f, source)

	gaps := reportGaps(t, report)["diff_coverage"]
	if len(gaps) == 0 {
		t.Fatalf("no diff_coverage gap without a coverage profile: %v", report)
	}
	if gaps[0]["blocking"] != false {
		t.Fatalf("an unmeasured diff blocked: %v", gaps[0])
	}
	if report["passed"] != true {
		t.Fatalf("an otherwise clean but unmeasured diff did not pass: %v", report)
	}

	// The next action must agree with the gap. An agent routes on the action's
	// own Blocking field, and the two disagreeing is a report that tells the
	// model the task is finished while the gate says it is not.
	assertCoverageActionsAgree(t, report, false)

	// The absence of a profile is named, so "no gap" and "nothing measured"
	// cannot be read as the same answer.
	if degraded := reportDegraded(report); !strings.Contains(degraded, "no coverage file was supplied") {
		t.Errorf("a run with no coverage profile did not say so: %q", degraded)
	}
}

// TestUnexercisedAddedLinesAreReportedAndNeverBlock is the same invariant
// against real measurements rather than their absence: the profile below
// measures the file and records that nothing in it ran.
func TestUnexercisedAddedLinesAreReportedAndNeverBlock(t *testing.T) {
	const source = "package calc\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"

	// A Go coverprofile whose only block for this file has an execution count
	// of zero: measured, and not executed. That is a statement about the code,
	// unlike the unmeasured case above, which is a statement about the inputs.
	f := newFixture(t)
	profile := filepath.Join(t.TempDir(), "cover.out")
	if err := os.WriteFile(profile,
		[]byte("mode: set\nmanvi/src/calc.go:1.1,10.2 4 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.reg.deps.CoverageFile = profile
	report := verifyAfterWriting(t, f, source)

	gaps := reportGaps(t, report)["diff_coverage"]
	if len(gaps) == 0 {
		t.Fatalf("an entirely unexercised file produced no coverage gap: %v", report)
	}
	if gaps[0]["blocking"] != false {
		t.Fatalf("uncovered added lines blocked: %v", gaps[0])
	}
	assertCoverageActionsAgree(t, report, false)
}

// assertCoverageActionsAgree checks every diff_coverage next action carries the
// same blocking answer as the gap it belongs to.
func assertCoverageActionsAgree(t *testing.T, report map[string]any, want bool) {
	t.Helper()
	actions, _ := report["next_actions"].([]any)
	var seen int
	for _, item := range actions {
		action, _ := item.(map[string]any)
		if action["category"] != "diff_coverage" {
			continue
		}
		seen++
		if action["blocking"] != want {
			t.Errorf("a diff_coverage next action reports blocking=%v, want %v: %v",
				action["blocking"], want, action)
		}
	}
	if seen == 0 {
		t.Errorf("no diff_coverage next action accompanied the coverage gap: %v", actions)
	}
}
