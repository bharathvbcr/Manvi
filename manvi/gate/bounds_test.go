package gate

import (
	"strings"
	"testing"
	"time"

	"manvi/dc"
	"manvi/policy"
)

// The gate is asked about every command an agent runs, and the command is a
// string a model composed. Deciding must therefore cost a bounded amount, and
// the bound must not depend on the model's restraint.
//
// This is the check that was missing. The ladder is a stack of scans over the
// line and several of them recurse, so cost is a function of length — and
// nothing checked length. Measured before the bound existed, a 1.8 MB line of
// chained substitutions took seven seconds to reach a refusal, and the
// redirect rung re-judged one repeated filename twenty thousand times to
// produce the same verdict twenty thousand times.

// boundsTask allows every command word so the cost being measured is the
// ladder's own, not an allowlist miss short-circuiting it.
func boundsTask() *dc.Task {
	return &dc.Task{
		ID:              "TASK-BOUNDS",
		PlannedFiles:    []dc.PlannedFile{{Path: "src/calc.go", AllowedChange: dc.ChangeModify}},
		AllowedCommands: []string{"*"},
	}
}

// adversarialShapes are the inputs whose cost grows fastest with length: each
// one multiplies a per-clause or per-span cost by a count the input controls.
var adversarialShapes = []struct {
	name string
	make func(n int) string
}{
	{"nested-substitutions", func(n int) string {
		return strings.Repeat("$(", n) + "echo hi" + strings.Repeat(")", n)
	}},
	{"repeated-substitutions", func(n int) string {
		return "echo " + strings.Repeat("$(echo a > b.txt) ", n)
	}},
	{"long-chain-one-target", func(n int) string {
		return strings.TrimSuffix(strings.Repeat("echo a > b.txt && ", n), "&& ")
	}},
	{"long-chain-distinct-targets", func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(" && ")
			}
			b.WriteString("echo a > out")
			b.WriteString(strings.Repeat("x", i%16))
			b.WriteString(".txt")
		}
		return b.String()
	}},
	{"nested-parens", func(n int) string {
		return "echo " + strings.Repeat("(", n) + strings.Repeat(")", n)
	}},
	{"repeated-backticks", func(n int) string {
		return "echo " + strings.Repeat("`echo a`", n)
	}},
	{"long-single-word", func(n int) string {
		return "echo " + strings.Repeat("a", n) + " > b.txt"
	}},
}

// TestEveryCommandDecidesInBoundedTime is the cost invariant.
//
// The budget is deliberately loose — this is a backstop against unbounded work,
// not a benchmark, and a loose budget is one that does not go red on a busy
// machine. What it catches is the shape that grows without limit, which is the
// only thing worth failing over.
func TestEveryCommandDecidesInBoundedTime(t *testing.T) {
	const budget = 2 * time.Second
	for _, shape := range adversarialShapes {
		for _, n := range []int{1000, 20000, 100000} {
			command := shape.make(n)
			g := newGate(t, nil)
			start := time.Now()
			d, err := g.EvaluateCommand(command, boundsTask())
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("%s n=%d: %v", shape.name, n, err)
			}
			if elapsed > budget {
				t.Errorf("%s n=%d (%d bytes) took %v to decide, over the %v budget; "+
					"cost is growing with an input the caller controls",
					shape.name, n, len(command), elapsed.Round(time.Millisecond), budget)
			}
			t.Logf("%-28s n=%-7d %8d bytes  %-22s %v",
				shape.name, n, len(command), verdictOf(d), elapsed.Round(time.Millisecond))
		}
	}
}

// TestOversizedCommandsAreRefusedWithoutBeingRead pins the bound itself.
func TestOversizedCommandsAreRefusedWithoutBeingRead(t *testing.T) {
	// One byte over is refused; the same shape under the limit is not refused
	// *for length*. Both halves matter: a bound that refuses everything would
	// pass the first check alone.
	under := "echo " + strings.Repeat("a", 1000) + " > src/calc.go"
	g := newGate(t, nil)
	d, err := g.EvaluateCommand(under, boundsTask())
	if err != nil {
		t.Fatal(err)
	}
	if d.Rule == policy.RuleCommandTooLong {
		t.Fatalf("a %d-byte command was refused for length; the bound is too tight", len(under))
	}

	over := "echo " + strings.Repeat("a", 200<<10) + " > src/calc.go"
	g = newGate(t, nil)
	d, err = g.EvaluateCommand(over, boundsTask())
	if err != nil {
		t.Fatal(err)
	}
	if d.Rule != policy.RuleCommandTooLong {
		t.Fatalf("a %d-byte command got rule %q, want %q", len(over), d.Rule, policy.RuleCommandTooLong)
	}
	if d.Severity != policy.Hard {
		t.Errorf("severity %q, want hard: a length bound no posture can relax is the point of it", d.Severity)
	}
	// Target travels into logs, transcripts and the TUI. Echoing back the
	// megabytes that triggered the rule would move the cost rather than bound
	// it.
	if len(d.Target) > 128 {
		t.Errorf("the refusal carried %d bytes of the offending command back in Target", len(d.Target))
	}
	if strings.Contains(d.Target, strings.Repeat("a", 64)) {
		t.Error("the refusal quoted the command it refused for being too long")
	}
}

func verdictOf(d policy.Decision) string {
	if d.Blocked() {
		return "deny:" + string(d.Rule)
	}
	return string(d.Action)
}
