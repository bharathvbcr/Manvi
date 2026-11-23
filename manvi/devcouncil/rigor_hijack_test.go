package devcouncil

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestGapsFromMapsRunnerHijackAsBlockingSecurityAction(t *testing.T) {
	gaps, actions := gapsFrom("TASK-HIJACK", &rigorResult{
		OK:    true,
		Files: 1,
		Findings: []Finding{{
			Gate:     "runner_hijack",
			Severity: "blocking",
			Path:     "tests/conftest.py",
			Line:     2,
			Evidence: "os._exit(0)",
			Message:  "`os._exit` inside test/bootstrap module bypasses test teardown and exits immediately",
		}},
	})
	if len(gaps) != 1 || !gaps[0].Blocking || gaps[0].Type != "runner_hijack" || gaps[0].Severity != "high" {
		t.Fatalf("expected 1 blocking high runner_hijack gap, got %+v", gaps)
	}
	if len(actions) != 1 || !actions[0].Blocking || !strings.Contains(actions[0].Action, "test-runner hijack") {
		t.Fatalf("expected blocking runner-hijack NextAction, got %+v", actions)
	}
}

func TestGapsFromMapsLeanTCBAsBlockingSecurityAction(t *testing.T) {
	gaps, actions := gapsFrom("TASK-LEAN", &rigorResult{
		OK:    true,
		Files: 1,
		Findings: []Finding{{
			Gate:     "lean_tcb",
			Severity: "blocking",
			Path:     "specs/Proof.lean",
			Line:     4,
			Evidence: "  sorry",
			Message:  "Lean 4 proof hole `sorry` in `Proof.thm` bypasses kernel verification via sorry axiom",
		}},
	})
	if len(gaps) != 1 || !gaps[0].Blocking || gaps[0].Type != "lean_tcb" || gaps[0].Severity != "high" {
		t.Fatalf("expected 1 blocking high lean_tcb gap, got %+v", gaps)
	}
	if len(actions) != 1 || !actions[0].Blocking || !strings.Contains(actions[0].Action, "Lean 4 TCB") {
		t.Fatalf("expected blocking Lean TCB NextAction, got %+v", actions)
	}
}

// leanDiff is a unified diff naming n distinct .lean files.
func leanDiff(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		name := "specs/S" + strconv.Itoa(i) + ".lean"
		b.WriteString("diff --git a/" + name + " b/" + name + "\n--- a/" + name + "\n+++ b/" + name + "\n@@ -0,0 +1 @@\n+theorem t : True := trivial\n")
	}
	return b.String()
}

func deadlineOf(diff string) time.Duration {
	d, _ := (rigorClient{}).deadline(diff)
	return d
}

// wantLeanDeadline is the deadline model written out independently of
// deadline(): files x (probe + (3 + imports) x step) + 60s, floored at 240s and
// capped.
func wantLeanDeadline(step time.Duration, files, imports int) time.Duration {
	per := min(step, 10*time.Second) + time.Duration(3+imports)*step
	return min(max(time.Duration(files)*per+60*time.Second, 240*time.Second), leanRigorCap)
}

func TestRigorDefaultTimeoutScalesWhenLeanKernelModeEnabled(t *testing.T) {
	t.Setenv("DC_VERIFY_LEAN_KERNEL", "")
	t.Setenv("DC_LEAN_KERNEL_TIMEOUT_MS", "")
	if got := deadlineOf(leanDiff(1)); got != 30*time.Second {
		t.Fatalf("default deadline = %v, want 30s", got)
	}
	t.Setenv("DC_VERIFY_LEAN_KERNEL", "auto")
	if got := deadlineOf(leanDiff(1)); got < 210*time.Second {
		t.Fatalf("deadline under DC_VERIFY_LEAN_KERNEL=auto = %v, want >= 210s for lean4checker --fresh replay", got)
	}
}

// dcverify accepts only auto and require for DC_VERIFY_LEAN_KERNEL and fails
// closed on any other value, so a value it would reject must not widen the
// deadline either.
func TestRigorLeanDeadlineIgnoresValuesDcverifyRejects(t *testing.T) {
	t.Setenv("DC_LEAN_KERNEL_TIMEOUT_MS", "")
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", "0", "off", "false", "lexical", ""} {
		t.Setenv("DC_VERIFY_LEAN_KERNEL", v)
		if got := deadlineOf(leanDiff(1)); got != 30*time.Second {
			t.Errorf("DC_VERIFY_LEAN_KERNEL=%q: deadline = %v, want the 30s default", v, got)
		}
	}
	for _, v := range []string{"auto", "AUTO", " require "} {
		t.Setenv("DC_VERIFY_LEAN_KERNEL", v)
		if got, want := deadlineOf(leanDiff(1)), wantLeanDeadline(180*time.Second, 1, 64); got != want {
			t.Errorf("DC_VERIFY_LEAN_KERNEL=%q: deadline = %v, want %v", v, got, want)
		}
	}
}

// The deadline follows the work: it is the sum over .lean files of probe +
// (3 + staged imports) x step, plus 60s, floored at 240s and capped. The
// staged-import count is dc-proc's own cap, 64.
func TestRigorLeanTimeoutFollowsStepAndFileCount(t *testing.T) {
	t.Setenv("DC_VERIFY_LEAN_KERNEL", "require")
	for _, tc := range []struct {
		raw  string
		step time.Duration
	}{
		{"", 180 * time.Second},
		{"0", 50 * time.Millisecond},
		{"1000", time.Second},
		{"180001", 180*time.Second + time.Millisecond},
		{"300000", 300 * time.Second},
		{"600001", 600 * time.Second},
		{"18446744073709551615", 600 * time.Second},
		{"18446744073709551616", 180 * time.Second},
		{"+300000", 300 * time.Second},
		{"-300000", 180 * time.Second},
		{"5m", 180 * time.Second},
		{" 300000 ", 300 * time.Second},
	} {
		t.Setenv("DC_LEAN_KERNEL_TIMEOUT_MS", tc.raw)
		if got := leanKernelStep(); got != tc.step {
			t.Errorf("DC_LEAN_KERNEL_TIMEOUT_MS=%q: step = %v, want %v", tc.raw, got, tc.step)
		}
		for _, files := range []int{0, 1, 2, 5, 1000} {
			if got, want := deadlineOf(leanDiff(files)), wantLeanDeadline(tc.step, files, 64); got != want {
				t.Errorf("DC_LEAN_KERNEL_TIMEOUT_MS=%q files=%d: deadline = %v, want %v", tc.raw, files, got, want)
			}
		}
	}
	t.Setenv("DC_LEAN_KERNEL_TIMEOUT_MS", "600000")
	if got := (rigorClient{Timeout: 5 * time.Second}); func() time.Duration { d, _ := got.deadline(leanDiff(3)); return d }() != 5*time.Second {
		t.Errorf("an explicit Timeout must win")
	}
}

// dcverifyLeanTable is DevCouncil's dcverify.Client.effectiveTimeout
// (backend/go_orchestrator/dc/dcverify/dcverify.go:191 at the pinned archive)
// evaluated by hand for DC_VERIFY_LEAN_KERNEL=require: the step clamped to
// 50ms..600s (180s when unset or not an unsigned decimal), plus 60s, never
// under 240s. That method is unexported, so it cannot be called from here; this
// table pins the step clamp Manvi shares with it and with dc-proc's
// configured_lean_kernel_timeout.
var dcverifyLeanTable = []struct {
	raw  string
	want time.Duration
}{
	{"", 240 * time.Second},
	{"0", 240 * time.Second},
	{"49", 240 * time.Second},
	{"180000", 240 * time.Second},
	{"180001", 240*time.Second + time.Millisecond},
	{"300000", 360 * time.Second},
	{"600000", 660 * time.Second},
	{"600001", 660 * time.Second},
	{"18446744073709551615", 660 * time.Second},
	{"18446744073709551616", 240 * time.Second},
	{"+300000", 240 * time.Second},
	{"-300000", 240 * time.Second},
	{"5m", 240 * time.Second},
	{" 300000 ", 360 * time.Second},
}

// rustParseDiffers lists the table rows where DevCouncil's Go client and
// dc-proc's Rust step parse disagree. The binary that runs each step is the
// Rust one, and Rust's u64 FromStr accepts one leading '+', so "+300000" is
// 240s for the Go client's table and 360s (a 300s step plus 60s) for the step
// Manvi must budget.
var rustParseDiffers = map[string]time.Duration{"+300000": 360 * time.Second}

func TestRigorLeanStepClampMatchesDevCouncilTable(t *testing.T) {
	for _, tc := range dcverifyLeanTable {
		t.Setenv("DC_LEAN_KERNEL_TIMEOUT_MS", tc.raw)
		want := tc.want
		if rust, ok := rustParseDiffers[tc.raw]; ok {
			want = rust
		}
		if got := max(leanKernelStep()+60*time.Second, 240*time.Second); got != want {
			t.Errorf("DC_LEAN_KERNEL_TIMEOUT_MS=%q: step clamp implies %v, want %v (DevCouncil's table row is %v)", tc.raw, got, want, tc.want)
		}
	}
}

// The Rust timeout is per child process and one check runs the Lean compile,
// the axioms driver and lean4checker one after the other, behind a libdir probe
// of up to 10s. A deadline of one step plus a margin kills a slow but honest
// run during its second step.
func TestRigorLeanTimeoutCoversTheSequentialStepWorstCase(t *testing.T) {
	for _, mode := range []string{"auto", "require"} {
		t.Setenv("DC_VERIFY_LEAN_KERNEL", mode)
		for _, tc := range dcverifyLeanTable {
			t.Setenv("DC_LEAN_KERNEL_TIMEOUT_MS", tc.raw)
			step := leanKernelStep()
			for _, files := range []int{1, 2} {
				worst := time.Duration(files) * (min(step, 10*time.Second) + 3*step)
				got := deadlineOf(leanDiff(files))
				if got < min(worst+60*time.Second, leanRigorCap) {
					t.Errorf("mode=%s DC_LEAN_KERNEL_TIMEOUT_MS=%q files=%d: deadline %v is under the no-import worst case %v plus slack", mode, tc.raw, files, got, worst)
				}
			}
		}
	}
}

func TestRunRigorDegradationNamesEveryOwnedGate(t *testing.T) {
	r := &Registry{}
	_, _, degraded := r.runRigor(t.Context(), "", nil, "TASK-OWNED")
	if len(degraded) != 1 {
		t.Fatalf("want one degradation, got %v", degraded)
	}
	for _, gate := range []string{"secret_scan", "stub_detection", "diff_coverage", "runner_hijack", "lean_tcb"} {
		if !strings.Contains(degraded[0], gate) {
			t.Errorf("degradation %q omits gate %s", degraded[0], gate)
		}
	}
}

func TestGapsFromUsesDevCouncilWording(t *testing.T) {
	for _, tc := range []struct {
		gate, path string
		line       int
		want       string
	}{
		{"secret_scan", "a/k.go", 3, "Remove the detected secret/finding from the diff and rotate any exposed credential."},
		{"stub_detection", "a/s.go", 7, "Replace the stub/placeholder at a/s.go:7 with a real implementation, then re-verify. Do not mark work complete while placeholders remain."},
		{"stub_detection", "", 0, "Replace the stub/placeholder at the affected file:0 with a real implementation, then re-verify. Do not mark work complete while placeholders remain."},
		{"stub_allowed", "a/s.go", 9, "Review the intentional stub declared at a/s.go:9; replace it before marking done."},
		{"assert_free_test", "a/t_test.go", 4, "Make the added test assert on the behaviour it is named for, or call a helper that does."},
		{"skipped_test", "a/t_test.go", 5, "Remove the unconditional skip, or make the skip conditional on what the test needs."},
		{"runner_hijack", "a/conftest.py", 2, "Remove the test-runner hijack in a/conftest.py so the real test harness executes and reports failures honestly."},
		{"runner_hijack", "", 0, "Remove the test-runner hijack in the affected file so the real test harness executes and reports failures honestly."},
		{"lean_tcb", "s/P.lean", 4, "Replace the Lean 4 TCB soundness hole, metaprogramming escape, or non-standard axiom/extern hook in s/P.lean with a kernel- and lean4checker-verified proof, then re-verify."},
		{"lean_tcb", "", 0, "Replace the Lean 4 TCB soundness hole, metaprogramming escape, or non-standard axiom/extern hook in the affected file with a kernel- and lean4checker-verified proof, then re-verify."},
		{"some_future_gate", "a/x.go", 1, "the verifier's own message"},
	} {
		_, actions := gapsFrom("T", &rigorResult{Findings: []Finding{
			{Gate: tc.gate, Severity: "blocking", Path: tc.path, Line: tc.line, Message: "the verifier's own message"},
		}})
		if len(actions) != 1 || actions[0].Action != tc.want {
			t.Errorf("gate %s path %q: action = %+v, want %q", tc.gate, tc.path, actions, tc.want)
		}
	}
}

// slowVerifier is a dcverify stand-in that never answers within any bound.
func slowVerifier(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "dcverify")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil { // #nosec G306 -- an executable test double in a temp dir
		t.Fatal(err)
	}
	return bin
}

func shrinkLeanCap(t *testing.T, d time.Duration) {
	t.Helper()
	old := leanRigorCap
	leanRigorCap = d
	t.Cleanup(func() { leanRigorCap = old })
}

// A Lean check cut off by its bound is an error naming the bound and the work,
// not a result: run returns no findings with it.
func TestRigorLeanBoundTimeoutIsACouldNotRunError(t *testing.T) {
	t.Setenv("DC_VERIFY_LEAN_KERNEL", "require")
	t.Setenv("DC_LEAN_KERNEL_TIMEOUT_MS", "50")
	shrinkLeanCap(t, 300*time.Millisecond)
	res, err := rigorClient{Binary: slowVerifier(t)}.run(t.Context(), leanDiff(2), nil, "")
	var bound *leanBoundError
	if !errors.As(err, &bound) || res != nil {
		t.Fatalf("run = (%v, %v), want a *leanBoundError and no result", res, err)
	}
	const want = "could not run: the Lean check exceeded its 300ms bound (2 files, up to 64 local imports each)"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

// runRigor maps that error onto the did-not-run path: a blocking gap and a
// degradation, so a timed-out Lean check can be neither a pass nor an ordinary
// finding.
func TestRunRigorMapsLeanBoundTimeoutToDidNotRun(t *testing.T) {
	t.Setenv("DC_VERIFY_LEAN_KERNEL", "auto")
	t.Setenv("DC_LEAN_KERNEL_TIMEOUT_MS", "50")
	shrinkLeanCap(t, 300*time.Millisecond)
	r := &Registry{}
	r.deps.VerifierBinary = slowVerifier(t)
	gaps, actions, degraded := r.runRigor(t.Context(), leanDiff(3), nil, "TASK-BOUND")
	const msg = "could not run: the Lean check exceeded its 300ms bound (3 files, up to 64 local imports each)"
	if len(gaps) != 1 || gaps[0].Type != gapRigorDidNotRun || !gaps[0].Blocking || !strings.Contains(gaps[0].Detail, msg) {
		t.Fatalf("gaps = %+v, want one blocking %s gap carrying %q", gaps, gapRigorDidNotRun, msg)
	}
	if len(actions) != 1 || !actions[0].Blocking {
		t.Fatalf("actions = %+v, want one blocking action", actions)
	}
	if len(degraded) != 1 || !strings.Contains(degraded[0], msg) {
		t.Fatalf("degraded = %v, want one entry carrying %q", degraded, msg)
	}
}

// With no .lean file in the diff, a timeout is the ordinary verifier timeout:
// the Lean bound says nothing about work that was never asked for.
func TestRigorTimeoutWithoutLeanFilesIsNotALeanBoundError(t *testing.T) {
	t.Setenv("DC_VERIFY_LEAN_KERNEL", "auto")
	shrinkLeanCap(t, 300*time.Millisecond)
	_, err := rigorClient{Binary: slowVerifier(t), Timeout: 200 * time.Millisecond}.run(t.Context(), leanDiff(0)+"diff --git a/a.go b/a.go\n", nil, "")
	var bound *leanBoundError
	if err == nil || errors.As(err, &bound) {
		t.Fatalf("err = %v, want an ordinary timeout", err)
	}
}

func TestLeanFilesInDiffCountsDistinctPaths(t *testing.T) {
	diff := leanDiff(2) + "diff --git a/specs/S0.lean b/specs/S0.lean\n+++ b/specs/S0.lean\ndiff --git a/x.go b/x.go\n+++ b/x.go\n+++ /dev/null\n"
	if got := leanFilesInDiff(diff); got != 2 {
		t.Fatalf("leanFilesInDiff = %d, want 2", got)
	}
}
