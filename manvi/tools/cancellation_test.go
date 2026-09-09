package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
)

func TestCancellationBeforeAndAfterApprovalNeverDispatches(t *testing.T) {
	for _, stage := range []string{"entry", "approval", "dispatch"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			r, b := registryWith(t, func(context.Context, Call) Result { calls++; return Result{Text: "changed"} })
			switch stage {
			case "entry":
				cancel()
			case "approval":
				_, err := bus.OnWaterfall(b, func(e PreExecute, next bus.Next[PreExecute]) PreExecute { cancel(); return next(e) })
				if err != nil {
					t.Fatal(err)
				}
			case "dispatch":
				_, err := bus.OnWaterfall(b, func(e Execute, next bus.Next[Execute]) Execute { cancel(); e.Result = e.Dispatch(); return next(e) })
				if err != nil {
					t.Fatal(err)
				}
			}
			result := r.Run(ctx, Call{Name: "probe"})
			if calls != 0 || !result.IsError || result.Blocked {
				t.Fatalf("calls=%d result=%+v", calls, result)
			}
		})
	}
}

func TestApprovalUsesOwnedCallArguments(t *testing.T) {
	raw := json.RawMessage(`{"x":1}`)
	var executed string
	r, b := registryWith(t, func(_ context.Context, call Call) Result {
		executed = string(call.Arguments)
		return Result{Text: "ok"}
	})
	_, err := bus.OnWaterfall(b, func(e PreExecute, next bus.Next[PreExecute]) PreExecute { raw[5] = '9'; return next(e) })
	if err != nil {
		t.Fatal(err)
	}
	got := r.Run(context.Background(), Call{Name: "probe", Arguments: raw})
	if got.IsError || executed != `{"x":1}` {
		t.Fatalf("caller changed approved arguments: executed=%s", executed)
	}
}
