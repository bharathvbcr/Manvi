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
