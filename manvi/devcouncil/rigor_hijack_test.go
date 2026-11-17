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
	if len(actions) != 1 || !actions[0].Blocking || !strings.Contains(actions[0].Action, "anti-reward-hack") {
		t.Fatalf("expected blocking anti-reward-hack NextAction, got %+v", actions)
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
	if len(actions) != 1 || !actions[0].Blocking || !strings.Contains(actions[0].Action, "Formal verification TCB violation") {
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
		if got := (rigorClient{}).effectiveTimeout(); got != 240*time.Second {
			t.Errorf("DC_VERIFY_LEAN_KERNEL=%q: effectiveTimeout() = %v, want 240s", v, got)
		}
	}
}

// The Lean step is DC_LEAN_KERNEL_TIMEOUT_MS clamped to 50ms..600s, 180s when
// it is unset or not an unsigned decimal; the deadline is that plus 60s and
// never under 240s.
func TestRigorLeanTimeoutClampsTheKernelStep(t *testing.T) {
	t.Setenv("DC_VERIFY_LEAN_KERNEL", "require")
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"", 240 * time.Second},
		{"0", 240 * time.Second},
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
