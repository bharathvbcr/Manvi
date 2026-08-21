package devcouncil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"manvi/flags"
)

// The two verification settings are the highest-stakes knobs in this package,
// because both of them move the line between "this run was checked" and "this
// run was not". The tests below are written around one property: turning a
// check off must never be indistinguishable from the check running and passing.
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

// TestRigorDisabledTurnsAFailingRunGreenAndSaysSo is the whole argument for
// this flag being wired the way it is.
//
// The same diff is verified twice. With verify.rigor.enabled on it is refused;
// with it off it passes. That inversion is the setting doing its job — and it
// is also precisely why the report must name the gate that did not run. An
// operator who reads `passed: true` from the second run and cannot tell it from
// the first has been handed a check that was never performed, dressed as one
// that succeeded.
func TestRigorDisabledTurnsAFailingRunGreenAndSaysSo(t *testing.T) {
	on := newFixture(t)
	onReport := verifyAfterWriting(t, on, stubbedSource)
	if onReport["passed"] != false {
		t.Fatalf("the stub gate did not refuse a stubbed diff; the control proves nothing: %v", onReport)
	}
	if len(reportGaps(t, onReport)["stub_detection"]) == 0 {
		t.Fatalf("no stub_detection gap in the control run: %v", onReport)
	}

	off := newFixtureWith(t, map[string]string{
		flags.HarnessPosture:     flags.PostureStrict,
		flags.VerifyRigorEnabled: "false",
	})
	offReport := verifyAfterWriting(t, off, stubbedSource)
	if got := reportGaps(t, offReport)["stub_detection"]; len(got) > 0 {
		t.Fatalf("%s=false still produced stub_detection findings: %v", flags.VerifyRigorEnabled, got)
	}

	degraded := reportDegraded(offReport)
	if !strings.Contains(degraded, flags.VerifyRigorEnabled) {
		t.Errorf("the degradation does not name the setting responsible: %q", degraded)
	}
	if !strings.Contains(degraded, "stub_detection") {
		t.Errorf("the degradation does not name the gate that did not run: %q", degraded)
	}

	// And the tool result carries it too, not only the JSON body. The session
	// log is assembled from Result, so a degradation that lives only in the
	// payload is one the run report cannot see.
	res := off.call("devcouncil_verify_task", map[string]any{})
	if len(res.Degraded) == 0 {
		t.Fatal("the verify result carried no Degraded entries; the run report cannot tell this from a clean pass")
	}
	if !res.Qualified() {
		t.Fatal("a verification with a gate switched off reported itself as an ordinary pass")
	}
}

// TestRigorDisabledLeavesSecretScanningOn is the security boundary on that
// setting.
//
// verify.rigor.enabled is described as stub, effort and acceptance-proof
// detection. It reaches the same dcverify process as the credential scanner,
// and wiring it as "skip the verifier" would have silently turned that scanner
// off too — an operator quieting a noisy TODO check would have stopped
// credential detection without being told.
func TestRigorDisabledLeavesSecretScanningOn(t *testing.T) {
	f := newFixtureWith(t, map[string]string{
		flags.HarnessPosture:     flags.PostureStrict,
		flags.VerifyRigorEnabled: "false",
	})
	report := verifyAfterWriting(t, f, "package calc\n\n"+
		"const key = \"sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAA\"\n")

	secrets := reportGaps(t, report)["secret_scan"]
	if len(secrets) == 0 {
		t.Fatalf("%s=false took the credential scanner down with it: %v",
			flags.VerifyRigorEnabled, report)
	}
	if secrets[0]["blocking"] != true {
		t.Errorf("a credential finding must still block: %v", secrets[0])
	}
	if report["passed"] == true {
		t.Fatalf("a diff containing a credential passed verification: %v", report)
	}
}

// TestDiffCoverageEnforcePromotesUnmeasuredToBlocking covers the case an
// operator is most likely to reach the setting for, and the one where getting
// the direction wrong is worst.
//
// With no coverage file every changed file is unmeasured. Under enforce that
// has to block: if it did not, the way to satisfy an enforced coverage gate
// would be to stop supplying coverage, which is the "a check that could not run
// looks like one that passed" failure with extra steps.
func TestDiffCoverageEnforcePromotesUnmeasuredToBlocking(t *testing.T) {
	const source = "package calc\n\nfunc Add(a, b int) int { return a + b }\n"

	lenient := newFixture(t)
	lenient.reg.deps.CoverageFile = ""
	lenientReport := verifyAfterWriting(t, lenient, source)
	lenientGaps := reportGaps(t, lenientReport)["diff_coverage"]
	if len(lenientGaps) == 0 {
		t.Fatalf("no diff_coverage gap without a profile; the control proves nothing: %v", lenientReport)
	}
	if lenientGaps[0]["blocking"] != false {
		t.Fatalf("diff coverage blocked with enforce off: %v", lenientGaps[0])
	}
	if lenientReport["passed"] != true {
		t.Fatalf("the unenforced run did not pass, so the comparison below is not about enforce: %v", lenientReport)
	}

	strict := newFixtureWith(t, map[string]string{
		flags.HarnessPosture:            flags.PostureStrict,
		flags.VerifyDiffCoverageEnforce: "true",
	})
	strict.reg.deps.CoverageFile = ""
	strictReport := verifyAfterWriting(t, strict, source)
	strictGaps := reportGaps(t, strictReport)["diff_coverage"]
	if len(strictGaps) == 0 {
		t.Fatalf("no diff_coverage gap under enforce: %v", strictReport)
	}
	if strictGaps[0]["blocking"] != true {
		t.Fatalf("%s=true left an unmeasured diff non-blocking: %v",
			flags.VerifyDiffCoverageEnforce, strictGaps[0])
	}
	if strictReport["passed"] != false {
		t.Fatalf("an unmeasured diff passed under %s=true: %v",
			flags.VerifyDiffCoverageEnforce, strictReport)
	}

	// The next action must agree with the gap. An agent routes on the action's
	// own Blocking field, and the two disagreeing is a report that tells the
	// model the task is finished while the gate says it is not.
	actions, _ := strictReport["next_actions"].([]any)
	var sawBlocking bool
	for _, item := range actions {
		action, _ := item.(map[string]any)
		if action["category"] == "diff_coverage" && action["blocking"] == true {
			sawBlocking = true
		}
	}
	if !sawBlocking {
		t.Fatalf("the diff_coverage next action is not marked blocking under enforce: %v", actions)
	}
}

// TestDiffCoverageEnforceBlocksUnexercisedAddedLines is the same setting
// against real measurements rather than their absence: the profile below
// measures the file and records that nothing in it ran.
func TestDiffCoverageEnforceBlocksUnexercisedAddedLines(t *testing.T) {
	const source = "package calc\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"

	// A Go coverprofile whose only block for this file has an execution count
	// of zero: measured, and not executed. That is a statement about the code,
	// unlike the unmeasured case above, which is a statement about the inputs.
	withProfile := func(t *testing.T, settings map[string]string) map[string]any {
		t.Helper()
		f := newFixtureWith(t, settings)
		profile := filepath.Join(t.TempDir(), "cover.out")
		if err := os.WriteFile(profile,
			[]byte("mode: set\nmanvi/src/calc.go:1.1,10.2 4 0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		f.reg.deps.CoverageFile = profile
		return verifyAfterWriting(t, f, source)
	}

	lenient := withProfile(t, map[string]string{flags.HarnessPosture: flags.PostureStrict})
	lenientGaps := reportGaps(t, lenient)["diff_coverage"]
	if len(lenientGaps) == 0 {
		t.Fatalf("an entirely unexercised file produced no coverage gap: %v", lenient)
	}
	if lenientGaps[0]["blocking"] != false {
		t.Fatalf("coverage blocked with enforce off: %v", lenientGaps[0])
	}

	strict := withProfile(t, map[string]string{
		flags.HarnessPosture:            flags.PostureStrict,
		flags.VerifyDiffCoverageEnforce: "true",
	})
	strictGaps := reportGaps(t, strict)["diff_coverage"]
	if len(strictGaps) == 0 {
		t.Fatalf("an entirely unexercised file produced no coverage gap under enforce: %v", strict)
	}
	if strictGaps[0]["blocking"] != true {
		t.Fatalf("%s=true left uncovered added lines non-blocking: %v",
			flags.VerifyDiffCoverageEnforce, strictGaps[0])
	}
	if strict["passed"] != false {
		t.Fatalf("a diff no test executed passed under %s=true: %v",
			flags.VerifyDiffCoverageEnforce, strict)
	}
}
