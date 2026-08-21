package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"manvi/core/bus"
	"manvi/llm"
)

func registryWith(t *testing.T, handler Handler) (*Registry, *bus.Bus) {
	t.Helper()
	b := bus.New()
	r := NewRegistry(b)
	if err := r.Register(Tool{
		Schema:  llm.ToolSchema{Name: "probe", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Handler: handler,
	}); err != nil {
		t.Fatal(err)
	}
	return r, b
}

// TestAToolBodyRunsExactlyOncePerCall is the side-effect invariant. A tool that
// writes a file must write it once, whether or not a listener wraps dispatch
// and whatever the body returns.
func TestAToolBodyRunsExactlyOncePerCall(t *testing.T) {
	cases := []struct {
		name    string
		result  Result
		wrapped bool
	}{
		{"empty result, unwrapped", Result{}, false},
		{"empty result, wrapped", Result{}, true},
		{"normal result, unwrapped", Result{Text: "ok"}, false},
		{"normal result, wrapped", Result{Text: "ok"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			r, b := registryWith(t, func(ctx context.Context, c Call) Result {
				calls.Add(1)
				return tc.result
			})
			if tc.wrapped {
				if _, err := bus.OnWaterfall(b, func(e Execute, next bus.Next[Execute]) Execute {
					e.Result = e.Dispatch()
					return next(e)
				}); err != nil {
					t.Fatal(err)
				}
			}
			r.Run(context.Background(), Call{ID: "1", Name: "probe"})
			if got := calls.Load(); got != 1 {
				t.Fatalf("the tool body ran %d times, want exactly 1", got)
			}
		})
	}
}

// TestPreExecuteShortCircuitSkipsTheBody is the gate's mechanism: a listener
// that decides must stop the body from running at all.
func TestPreExecuteShortCircuitSkipsTheBody(t *testing.T) {
	var calls atomic.Int32
	r, b := registryWith(t, func(ctx context.Context, c Call) Result {
		calls.Add(1)
		return Result{Text: "should not happen"}
	})
	if _, err := bus.OnWaterfall(b, func(e PreExecute, next bus.Next[PreExecute]) PreExecute {
		e.Decided = &Result{Text: "denied", IsError: true, Rule: "scope.unplanned"}
		return next(e)
	}); err != nil {
		t.Fatal(err)
	}
	res := r.Run(context.Background(), Call{ID: "1", Name: "probe"})
	if calls.Load() != 0 {
		t.Fatal("a short-circuited call must not reach the tool body")
	}
	if !res.IsError || res.Rule != "scope.unplanned" {
		t.Fatalf("result = %+v, want the deciding listener's outcome", res)
	}
}

// TestUnknownToolIsAnErrorResult: the model asked for something that does not
// exist, and the turn continues rather than the process failing.
func TestUnknownToolIsAnErrorResult(t *testing.T) {
	r, _ := registryWith(t, func(ctx context.Context, c Call) Result { return Result{} })
	res := r.Run(context.Background(), Call{ID: "1", Name: "nope"})
	if !res.IsError || !strings.Contains(res.Text, "nope") {
		t.Fatalf("result = %+v, want an error naming the unknown tool", res)
	}
}

// TestQualifiedDistinguishesAnOrdinaryPass is what the run report reads.
func TestQualifiedDistinguishesAnOrdinaryPass(t *testing.T) {
	if (Result{Text: "wrote x"}).Qualified() {
		t.Error("a plain success must not read as qualified")
	}
	for _, r := range []Result{
		{Text: "wrote x", GrantID: "GRANT-0001"},
		{Text: "wrote x", Demoted: "harness.posture=dev"},
		{Text: "wrote x", Degraded: []string{"repo_map.unavailable"}},
	} {
		if !r.Qualified() {
			t.Errorf("%+v must read as qualified", r)
		}
	}
}

// TestDuplicateRegistrationIsRefused: two bodies behind one name would make
// which one runs depend on registration order.
func TestDuplicateRegistrationIsRefused(t *testing.T) {
	r, _ := registryWith(t, func(ctx context.Context, c Call) Result { return Result{} })
	err := r.Register(Tool{
		Schema:  llm.ToolSchema{Name: "probe"},
		Handler: func(ctx context.Context, c Call) Result { return Result{} },
	})
	if err == nil {
		t.Fatal("a duplicate tool name was accepted")
	}
	if err := r.Register(Tool{Schema: llm.ToolSchema{Name: "nameless"}}); err == nil {
		t.Fatal("a tool with no handler was accepted")
	}
}

func TestDynamicToolActivationAndSearch(t *testing.T) {
	r := NewRegistry(bus.New())
	mustReg := func(name, group string, extended, readOnly bool, requires ...string) {
		t.Helper()
		if err := r.Register(Tool{
			Schema: llm.ToolSchema{
				Name:        name,
				Description: "Tool " + name + " in group " + group,
				InputSchema: []byte(`{"type":"object"}`),
			},
			Group:    group,
			Extended: extended,
			ReadOnly: readOnly,
			Requires: requires,
			Handler: func(context.Context, Call) Result {
				return Result{Text: name + " ok"}
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	mustReg("read_file", GroupCore, false, true)
	mustReg("write_file", GroupCore, false, false)
	mustReg("devmap_find", GroupNav, true, true)
	mustReg("verify_task", GroupTask, true, true, "checkout_task")
	mustReg("checkout_task", GroupTask, true, false)

	// Enable dynamic mode (default initial: core / non-extended)
	r.EnableDynamic()
	if !r.IsDynamic() {
		t.Fatal("expected dynamic mode to be enabled")
	}

	active := r.ActiveSchemas()
	if len(active) != 2 {
		t.Fatalf("expected 2 initial active schemas, got %d", len(active))
	}
	if active[0].Name != "read_file" || active[1].Name != "write_file" {
		t.Fatalf("unexpected active schemas: %v", active)
	}

	// Search
	results := r.Search("devmap")
	if len(results) != 1 || results[0].Name != "devmap_find" {
		t.Fatalf("search for 'devmap' returned %v", results)
	}

	// Search all
	allSummaries := r.ListSummaries()
	if len(allSummaries) != 5 {
		t.Fatalf("expected 5 summaries, got %d", len(allSummaries))
	}

	// Activate a group ("nav")
	activated, err := r.Activate(GroupNav)
	if err != nil {
		t.Fatalf("activate group nav failed: %v", err)
	}
	if len(activated) != 1 || activated[0] != "devmap_find" {
		t.Fatalf("expected ['devmap_find'] activated, got %v", activated)
	}

	// Activate with dependency auto-resolution ("verify_task" requires "checkout_task")
	activated, err = r.Activate("verify_task")
	if err != nil {
		t.Fatalf("activate verify_task failed: %v", err)
	}
	// verify_task should activate both verify_task and checkout_task
	if len(activated) != 1 || activated[0] != "verify_task" {
		t.Fatalf("unexpected activation return: %v", activated)
	}
	newActive := r.ActiveSchemas()
	var names []string
	for _, s := range newActive {
		names = append(names, s.Name)
	}
	// Should now have read_file, write_file, devmap_find, verify_task, checkout_task (5 tools)
	if len(newActive) != 5 {
		t.Fatalf("expected 5 active tools after dependency resolution, got %d (%v)", len(newActive), names)
	}

	// Deactivate
	deactivated := r.Deactivate(GroupNav)
	if len(deactivated) != 1 || deactivated[0] != "devmap_find" {
		t.Fatalf("unexpected deactivation: %v", deactivated)
	}
	if len(r.ActiveSchemas()) != 4 {
		t.Fatalf("expected 4 active schemas after deactivating nav, got %d", len(r.ActiveSchemas()))
	}
}
