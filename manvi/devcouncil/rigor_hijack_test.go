package devcouncil

import (
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

func TestRigorDefaultTimeoutScalesWhenLeanKernelModeEnabled(t *testing.T) {
	t.Setenv("DC_VERIFY_LEAN_KERNEL", "")
	t.Setenv("DC_LEAN_KERNEL_TIMEOUT_MS", "")
	var c rigorClient
	if got := c.effectiveTimeout(); got < 30*time.Second || got > 60*time.Second {
		t.Fatalf("default effectiveTimeout() = %v, want 30s", got)
	}
	t.Setenv("DC_VERIFY_LEAN_KERNEL", "auto")
	if got := c.effectiveTimeout(); got < 210*time.Second {
		t.Fatalf("effectiveTimeout() under DC_VERIFY_LEAN_KERNEL=auto = %v, want >= 210s for lean4checker --fresh replay", got)
	}
}

// dcverify accepts only auto and require for DC_VERIFY_LEAN_KERNEL and fails
// closed on any other value, so a value it would reject must not widen the
// deadline either.
func TestRigorLeanDeadlineIgnoresValuesDcverifyRejects(t *testing.T) {
	t.Setenv("DC_LEAN_KERNEL_TIMEOUT_MS", "")
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", "0", "off", "false", "lexical", ""} {
		t.Setenv("DC_VERIFY_LEAN_KERNEL", v)
		if got := (rigorClient{}).effectiveTimeout(); got != 30*time.Second {
			t.Errorf("DC_VERIFY_LEAN_KERNEL=%q: effectiveTimeout() = %v, want the 30s default", v, got)
		}
	}
	for _, v := range []string{"auto", "AUTO", " require "} {
		t.Setenv("DC_VERIFY_LEAN_KERNEL", v)
		if got := (rigorClient{}).effectiveTimeout(); got != 610*time.Second {
			t.Errorf("DC_VERIFY_LEAN_KERNEL=%q: effectiveTimeout() = %v, want 610s (3 x 180s + 10s + 60s)", v, got)
		}
	}
}

// The Lean step is DC_LEAN_KERNEL_TIMEOUT_MS clamped to 50ms..600s, 180s when
// it is unset or not an unsigned decimal; the deadline is three steps plus 10s
// plus 60s and never under 240s.
func TestRigorLeanTimeoutClampsTheKernelStep(t *testing.T) {
	t.Setenv("DC_VERIFY_LEAN_KERNEL", "require")
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"", 610 * time.Second},
		{"0", 240 * time.Second},
		{"180000", 610 * time.Second},
		{"180001", 610*time.Second + 3*time.Millisecond},
		{"300000", 970 * time.Second},
		{"600000", 1870 * time.Second},
		{"600001", 1870 * time.Second},
		{"18446744073709551615", 1870 * time.Second},
		{"18446744073709551616", 610 * time.Second},
		{"+300000", 610 * time.Second},
		{"-300000", 610 * time.Second},
		{"5m", 610 * time.Second},
		{" 300000 ", 970 * time.Second},
	} {
		t.Setenv("DC_LEAN_KERNEL_TIMEOUT_MS", tc.raw)
		if got := (rigorClient{}).effectiveTimeout(); got != tc.want {
			t.Errorf("DC_LEAN_KERNEL_TIMEOUT_MS=%q: effectiveTimeout() = %v, want %v", tc.raw, got, tc.want)
		}
	}
	t.Setenv("DC_LEAN_KERNEL_TIMEOUT_MS", "600000")
	if got := (rigorClient{Timeout: 5 * time.Second}).effectiveTimeout(); got != 5*time.Second {
		t.Errorf("an explicit Timeout must win: got %v", got)
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

func TestRigorLeanStepClampMatchesDevCouncilTable(t *testing.T) {
	for _, tc := range dcverifyLeanTable {
		t.Setenv("DC_LEAN_KERNEL_TIMEOUT_MS", tc.raw)
		if got := max(leanKernelStep()+60*time.Second, 240*time.Second); got != tc.want {
			t.Errorf("DC_LEAN_KERNEL_TIMEOUT_MS=%q: step clamp implies %v, DevCouncil's table says %v", tc.raw, got, tc.want)
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
			worst := 3*step + 10*time.Second
			if got := (rigorClient{}).effectiveTimeout(); got < worst+60*time.Second {
				t.Errorf("mode=%s DC_LEAN_KERNEL_TIMEOUT_MS=%q: timeout %v leaves under 60s over the worst case %v", mode, tc.raw, got, worst)
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
	_, actions := gapsFrom("T", &rigorResult{Findings: []Finding{
		{Gate: "runner_hijack", Severity: "blocking", Path: "a/conftest.py", Line: 2},
		{Gate: "lean_tcb", Severity: "blocking", Path: "s/P.lean", Line: 4},
	}})
	want := []string{
		"Remove the test-runner hijack in a/conftest.py so the real test harness executes and reports failures honestly.",
		"Replace the Lean 4 TCB soundness hole, metaprogramming escape, or non-standard axiom/extern hook in s/P.lean with a kernel- and lean4checker-verified proof, then re-verify.",
	}
	if len(actions) != 2 || actions[0].Action != want[0] || actions[1].Action != want[1] {
		t.Fatalf("actions = %+v, want DevCouncil's next_actions.go wording %q", actions, want)
	}
}
