package devcouncil

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pinnedDevCouncil is the archive go.mod's replace directive points at, seen
// from this package: manvi/devcouncil -> worktree root -> its sibling.
const advPinnedDevCouncil = "../../../DevCouncil"

func advRead(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(advPinnedDevCouncil, rel))
	if err != nil {
		t.Fatalf("the pinned DevCouncil archive is required (never skipped): %v", err)
	}
	return string(raw)
}

// leanRunFn returns the body of run_lean_kernel_check.
func advLeanRunBody(t *testing.T) string {
	src := advRead(t, "rust/dc-proc/src/lean_kernel.rs")
	i := strings.Index(src, "pub fn run_lean_kernel_check(")
	if i < 0 {
		t.Fatal("run_lean_kernel_check not found at the pin")
	}
	return src[i:]
}

// advSequentialBudget derives, from the Rust source, how long one .lean file's
// check can legitimately run: a libdir probe (min(step,10s)), one bounded
// compile per staged local import, then the three fixed steps.
func advSequentialBudget(t *testing.T, step time.Duration, localImports int) time.Duration {
	t.Helper()
	body := advLeanRunBody(t)
	fixed := len(regexp.MustCompile(`(?m)^\s*// Step [0-9]+:`).FindAllString(body, -1))
	if fixed != 3 {
		t.Fatalf("pinned run_lean_kernel_check has %d fixed steps, the change assumes 3", fixed)
	}
	probe := min(step, 10*time.Second)
	return probe + time.Duration(fixed+localImports)*step
}

func advDefaultLeanDeadline(t *testing.T) time.Duration {
	t.Setenv("DC_VERIFY_LEAN_KERNEL", "require")
	t.Setenv("DC_LEAN_KERNEL_TIMEOUT_MS", "")
	return (rigorClient{}).effectiveTimeout()
}

// Attack 1a: a spec with ONE in-tree local import. dc-proc compiles each staged
// dependency under its own full step before the 3 fixed steps.
func TestAdvB29ResidueDeadlineCoversOneLocalImport(t *testing.T) {
	src := advLeanRunBody(t)
	if !strings.Contains(src, "for (mod_name, lean_rel, olean_rel, dep_src) in ordered") ||
		!strings.Contains(src, "run_bounded(&mut dep_cmd, bounds)") {
		t.Fatal("pinned dc-proc no longer compiles local imports under run_bounded per dependency")
	}
	got := advDefaultLeanDeadline(t)
	need := advSequentialBudget(t, defaultLeanKernelStep, 1)
	if got < need {
		t.Errorf("deadline %v < worst case for a single .lean file with one local import %v (probe + (3+1) x %v)", got, need, defaultLeanKernelStep)
	}
}

// Attack 1b: the maximum number of staged local imports at the pin.
func TestAdvB29ResidueDeadlineCoversMaxLocalImports(t *testing.T) {
	m := regexp.MustCompile(`const MAX_STAGED_LOCAL_MODULES: usize = (\d+);`).FindStringSubmatch(advRead(t, "rust/dc-proc/src/lean_kernel.rs"))
	if m == nil {
		t.Fatal("MAX_STAGED_LOCAL_MODULES not found at the pin")
	}
	n, _ := strconv.Atoi(m[1])
	got := advDefaultLeanDeadline(t)
	need := advSequentialBudget(t, defaultLeanKernelStep, n)
	if got < need {
		t.Errorf("deadline %v < worst case %v for one file with %d local imports (documented cap), i.e. the \"3 x step\" model is not the worst case", got, need, n)
	}
}

// Attack 1c: a diff with N .lean files. rigor.rs runs the whole sequence once
// per file; nothing shares or caps the budget.
func TestAdvB29ResidueDeadlineCoversTwoLeanFiles(t *testing.T) {
	rs := advRead(t, "rust/dc-verify/src/rigor.rs")
	i := strings.Index(rs, "pub fn detect_lean_tcb_report_with_root(")
	j := strings.Index(rs[i:], "LeanTcbCheckReport {\n        findings,")
	if i < 0 || j < 0 {
		t.Fatal("detect_lean_tcb_report_with_root not found at the pin")
	}
	fn := rs[i : i+j]
	if !strings.Contains(fn, "for file in files {") || !strings.Contains(fn, "run_lean_kernel_check(") {
		t.Fatal("pinned rigor.rs no longer runs the kernel check inside the per-file loop")
	}
	got := advDefaultLeanDeadline(t)
	need := 2 * advSequentialBudget(t, defaultLeanKernelStep, 0)
	if got < need {
		t.Errorf("deadline %v < worst case for a diff touching two .lean files %v (2 x (probe + 3 x step))", got, need)
	}
}

// Attack 1d: the deadline for a single file with no imports (what the change
// claims to cover) really is covered, at every step the env can produce.
func TestAdvB29ResidueDeadlineCoversOneFileNoImportsAtEveryStep(t *testing.T) {
	t.Setenv("DC_VERIFY_LEAN_KERNEL", "auto")
	for _, raw := range []string{"", "0", "49", "50", "51", "1000", "9999", "10000", "10001", "180000", "599999", "600000", "600001", "18446744073709551615", "-1", "nope"} {
		t.Setenv("DC_LEAN_KERNEL_TIMEOUT_MS", raw)
		got := (rigorClient{}).effectiveTimeout()
		need := advSequentialBudget(t, leanKernelStep(), 0)
		if got < need {
			t.Errorf("DC_LEAN_KERNEL_TIMEOUT_MS=%q: deadline %v < single-file sequence %v", raw, got, need)
		}
	}
}

// Attack 1e: there is an upper bound on the deadline whatever the env says.
func TestAdvB29ResidueDeadlineHasAnUpperClamp(t *testing.T) {
	t.Setenv("DC_VERIFY_LEAN_KERNEL", "require")
	const ceiling = 3*600*time.Second + 10*time.Second + 60*time.Second
	for _, raw := range []string{"600000", "600001", "99999999999", "18446744073709551615", "18446744073709551616", "-1", "0", "nope", "+5", "1e9", "\u00a0999999999\u0085"} {
		t.Setenv("DC_LEAN_KERNEL_TIMEOUT_MS", raw)
		got := (rigorClient{}).effectiveTimeout()
		if got > ceiling || got < 240*time.Second {
			t.Errorf("DC_LEAN_KERNEL_TIMEOUT_MS=%q: deadline %v outside [240s, %v]", raw, got, ceiling)
		}
	}
}

// Attack 1f / 3: the step parse must be the Rust one. dc-proc uses
// raw.trim().parse::<u64>(); Rust's integer FromStr accepts a leading '+'
// (checked with rustc: "+300000" -> Ok(300000)), Go's ParseUint does not.
// Manvi's table copies DevCouncil's *Go* behaviour ("+300000" -> default), so
// for the real binary the deadline is computed from the wrong step.
func TestAdvB29ResidueStepParseMatchesRustPlusSign(t *testing.T) {
	src := advRead(t, "rust/dc-proc/src/lean_kernel.rs")
	if !strings.Contains(src, "raw.trim().parse::<u64>()") {
		t.Fatal("dc-proc no longer parses the step with trim().parse::<u64>(); re-derive this test")
	}
	t.Setenv("DC_LEAN_KERNEL_TIMEOUT_MS", "+300000")
	if got, want := leanKernelStep(), 300*time.Second; got != want {
		t.Errorf("leanKernelStep() for DC_LEAN_KERNEL_TIMEOUT_MS=+300000 = %v, but dc-proc runs each step for %v, so a real 3-step run needs %v while the deadline is %v",
			got, want, 3*want+10*time.Second, func() time.Duration {
				t.Setenv("DC_VERIFY_LEAN_KERNEL", "require")
				return (rigorClient{}).effectiveTimeout()
			}())
	}
}

// Attack 3: clamp edges asserted on leanKernelStep itself. The existing table
// goes through max(step+60s, 240s), which hides every step below 180s, so a
// wrong lower clamp (50ms) cannot fail it.
func TestAdvB29ResidueStepClampEdges(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"0", 50 * time.Millisecond},
		{"49", 50 * time.Millisecond},
		{"50", 50 * time.Millisecond},
		{"51", 51 * time.Millisecond},
		{"179999", 179999 * time.Millisecond},
		{"180000", 180 * time.Second},
		{"599999", 599999 * time.Millisecond},
		{"600000", 600 * time.Second},
		{"600001", 600 * time.Second},
		{"", 180 * time.Second},
		{"nope", 180 * time.Second},
		{"-1", 180 * time.Second},
		{"18446744073709551616", 180 * time.Second},
		{"00300000", 300 * time.Second},
	} {
		t.Setenv("DC_LEAN_KERNEL_TIMEOUT_MS", tc.raw)
		if got := leanKernelStep(); got != tc.want {
			t.Errorf("leanKernelStep() for %q = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// Attack 3b: the 14-row table's expected values re-derived independently by
// evaluating dcverify.go:191-216 at the pin (step+60s, floor 240s; parse error
// or overflow handled as the Go code does).
func TestAdvB29ResidueDevCouncilTableRowsAreArithmeticallyRight(t *testing.T) {
	src := advRead(t, "backend/go_orchestrator/dc/dcverify/dcverify.go")
	for _, frag := range []string{"step := 180 * time.Second", "ms <= 600_000", "ms < 50", "expanded := step + 60*time.Second", "expanded < 240*time.Second"} {
		if !strings.Contains(src, frag) {
			t.Fatalf("pinned dcverify.go no longer contains %q; table derivation is stale", frag)
		}
	}
	derive := func(raw string) time.Duration {
		step := 180 * time.Second
		if raw = strings.TrimSpace(raw); raw != "" {
			if ms, err := strconv.ParseUint(raw, 10, 64); err == nil && ms <= 600000 {
				if ms < 50 {
					ms = 50
				}
				step = time.Duration(ms) * time.Millisecond
			} else if err == nil {
				step = 600 * time.Second
			}
		}
		return max(step+60*time.Second, 240*time.Second)
	}
	if len(dcverifyLeanTable) != 14 {
		t.Fatalf("table has %d rows, expected 14", len(dcverifyLeanTable))
	}
	for _, tc := range dcverifyLeanTable {
		if got := derive(tc.raw); got != tc.want {
			t.Errorf("row %q: independent derivation %v, table %v", tc.raw, got, tc.want)
		}
	}
}

// Attack 2: which code reaches DevCouncil's Client (step+60s) instead of
// rigorClient. Nothing under manvi/ may import dc/dcverify, or a caller could
// get the shorter deadline and be killed mid-sequence.
func TestAdvB29ResidueNoCallerReachesDevCouncilDcverifyClient(t *testing.T) {
	var hits []string
	err := filepath.WalkDir("../", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(raw), "go_orchestrator/dc/dcverify\"") {
			hits = append(hits, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("these files import DevCouncil's dcverify client and bypass rigorClient.effectiveTimeout: %v", hits)
	}
}

// Attack 5: gapsFrom wording against next_actions.go at the pin, for every gate
// dc-verify can emit. Expected text is built from actionText's literals, and the
// literals are checked to still be in the pinned source.
func TestAdvB29ResidueGapsFromWordingMatchesDevCouncilForEveryGate(t *testing.T) {
	na := advRead(t, "backend/go_orchestrator/devcouncil/verify/next_actions.go")
	rg := advRead(t, "backend/go_orchestrator/devcouncil/verify/rigor.go")
	for _, lit := range []string{
		"Remove the detected secret/finding from the diff and rotate any exposed credential.",
		"Replace the stub/placeholder at ",
		" with a real implementation, then re-verify. ",
		"Do not mark work complete while placeholders remain.",
		"Review the intentional stub declared at ",
		"; replace it before marking done.",
		"Remove the test-runner hijack in ",
		"Replace the Lean 4 TCB soundness hole",
	} {
		if !strings.Contains(na, lit) {
			t.Fatalf("pinned next_actions.go no longer contains %q", lit)
		}
	}
	for _, lit := range []string{
		"Make the added test assert on the behaviour it is named for, or call a helper that does.",
		"Remove the unconditional skip, or make the skip conditional on what the test needs.",
	} {
		if !strings.Contains(rg, lit) {
			t.Fatalf("pinned rigor.go no longer contains %q", lit)
		}
	}
	const path = "pkg/a.go"
	cases := []struct {
		gate, want string
	}{
		{"secret_scan", "Remove the detected secret/finding from the diff and rotate any exposed credential."},
		{"stub_detection", "Replace the stub/placeholder at pkg/a.go:7 with a real implementation, then re-verify. Do not mark work complete while placeholders remain."},
		{"stub_allowed", "Review the intentional stub declared at pkg/a.go:7; replace it before marking done."},
		{"assert_free_test", "Make the added test assert on the behaviour it is named for, or call a helper that does."},
		{"skipped_test", "Remove the unconditional skip, or make the skip conditional on what the test needs."},
		{"runner_hijack", "Remove the test-runner hijack in pkg/a.go so the real test harness executes and reports failures honestly."},
		{"lean_tcb", "Replace the Lean 4 TCB soundness hole, metaprogramming escape, or non-standard axiom/extern hook in pkg/a.go with a kernel- and lean4checker-verified proof, then re-verify."},
	}
	for _, tc := range cases {
		t.Run(tc.gate, func(t *testing.T) {
			_, actions := gapsFrom("T", &rigorResult{Findings: []Finding{
				{Gate: tc.gate, Severity: "blocking", Path: path, Line: 7, Message: "dc-verify says so", Evidence: "e"},
			}})
			if len(actions) != 1 || actions[0].Action != tc.want {
				t.Errorf("gate %s action = %q, want DevCouncil's %q", tc.gate, firstAction(actions), tc.want)
			}
		})
	}
}

// Attack 5b: DevCouncil names "the affected file" when a finding has no path.
func TestAdvB29ResidueGapsFromEmptyPathUsesAffectedFile(t *testing.T) {
	for _, gate := range []string{"runner_hijack", "lean_tcb"} {
		_, actions := gapsFrom("T", &rigorResult{Findings: []Finding{{Gate: gate, Severity: "blocking", Path: "", Line: 0}}})
		if len(actions) != 1 || !strings.Contains(actions[0].Action, "the affected file") {
			t.Errorf("gate %s with empty path: %q does not say \"the affected file\" as next_actions.go does", gate, firstAction(actions))
		}
	}
}

func firstAction(a []NextAction) string {
	if len(a) == 0 {
		return "<none>"
	}
	return a[0].Action
}
