package agent

import (
	"context"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// What the ref guard returns for a ref that moved between calls (refguard.go
// withViolation, v.between): an error with the secret-path rule at hard
// severity and Blocked true. The loop must leave a durable policy record of it:
// a leaked commit parked between calls is exactly what the evidence trail
// exists to show. recordPolicy reads Blocked, Qualified (grant, demotion,
// widening, degraded) and PipelinePanic and nothing else, so the report is
// recorded only because it is Blocked; clearing Blocked (e42b36d2, reverted)
// made it match none of recordPolicy's cases.
func TestAdvB31BetweenCallsRefReportLeavesAPolicyRecord(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("call-1", "exec", `{"command":"echo two"}`),
		textTurn("done"),
	}, nil)
	if err := h.tools.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "exec"},
		Handler: func(context.Context, tools.Call) tools.Result {
			return tools.Result{
				Text:     `{"operation":"ref_changed_between_calls","rule":"path.secret","severity":"hard"}` + "\ntwo\n",
				IsError:  true,
				Rule:     "path.secret",
				Severity: "hard",
				Blocked:  true,
			}
		},
	}); err != nil {
		t.Fatal(err)
	}
	out, err := h.loop.Run(context.Background(), userMsg("run it"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var denied, qualified int
	for _, e := range h.log.Events() {
		switch e.Type {
		case session.PolicyDenied:
			denied++
		case session.PolicyQualified:
			qualified++
		}
	}
	t.Logf("Outcome.Denied=%d Qualified=%d; PolicyDenied events=%d PolicyQualified events=%d", out.Denied, out.Qualified, denied, qualified)
	if out.Denied+out.Qualified == 0 && denied+qualified == 0 {
		t.Fatalf("a hard path.secret report left no policy record at all: not counted in the outcome, not in the session log")
	}
}
