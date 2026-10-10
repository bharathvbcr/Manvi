package agent

import (
	"context"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// TestRuleOnAnAllowedResultStillMutates pins the end-of-turn checkpoint's input.
//
// devcouncil.annotate puts a Rule on every decision that is not Clean(),
// including allowed ones (demotion, grant, protected-file warn). Treating
// "Rule != empty" as "refused" made the turn look unchanged, so the sensor
// skipped verification for exactly the writes policy said must be verified.
// Only Blocked means refused.
func TestRuleOnAnAllowedResultStillMutates(t *testing.T) {
	cases := []struct {
		name        string
		result      tools.Result
		mutated     bool
		unenumerate bool
	}{
		{"demoted allow", tools.Result{
			Text: "wrote a.go", Wrote: []string{"a.go"}, Rule: "task.absent", Severity: "soft",
			Demoted: "policy.file.mode=off (harness.posture=yolo/override)",
		}, true, false},
		{"grant with rule", tools.Result{
			Text: "wrote a.go", Wrote: []string{"a.go"}, Rule: "task.absent", Severity: "soft",
			GrantID: "g1", GrantedBy: "owner",
		}, true, false},
		{"protected-file warn", tools.Result{
			Text: "wrote package.json", Wrote: []string{"package.json"}, Rule: "file.protected_write", Severity: "soft",
		}, true, false},
		{"exec allowed under demotion names no path", tools.Result{
			Text: "ran", Rule: "command.no_lease", Severity: "soft", Demoted: "posture",
		}, true, true},
		{"blocked is not a mutation", tools.Result{
			Text: "refused", IsError: true, Blocked: true, Rule: "file.secret", Severity: "hard",
		}, false, false},
		{"error is not a mutation", tools.Result{
			Text: "boom", IsError: true, Rule: "task.absent", Severity: "soft",
		}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := build(t, []replay.Turn{
				toolTurn("call-1", "write", `{"path":"a.go"}`),
				textTurn("done"),
			}, nil)
			result := tc.result
			if err := h.tools.Register(tools.Tool{
				Schema:  llm.ToolSchema{Name: "write"},
				Handler: func(context.Context, tools.Call) tools.Result { return result },
			}); err != nil {
				t.Fatal(err)
			}
			var stopping *TurnStopping
			if _, err := bus.OnSerial(h.bus, func(_ context.Context, e *TurnStopping) error {
				stopping = e
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			out, err := h.loop.Run(context.Background(), userMsg("write a.go"))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if out.Mutated != tc.mutated || out.UnenumeratedEffects != tc.unenumerate {
				t.Fatalf("Outcome Mutated=%v UnenumeratedEffects=%v, want %v/%v",
					out.Mutated, out.UnenumeratedEffects, tc.mutated, tc.unenumerate)
			}
			if stopping == nil {
				t.Fatal("TurnStopping never fired")
			}
			if stopping.Mutated != tc.mutated || stopping.UnenumeratedEffects != tc.unenumerate {
				t.Fatalf("TurnStopping Mutated=%v UnenumeratedEffects=%v, want %v/%v",
					stopping.Mutated, stopping.UnenumeratedEffects, tc.mutated, tc.unenumerate)
			}
		})
	}
}
