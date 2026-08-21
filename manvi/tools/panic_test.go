package tools

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"manvi/core/bus"
	"manvi/llm"
)

func panicRegistry(t *testing.T, ran *atomic.Int64) *Registry {
	t.Helper()
	r := NewRegistry(bus.New())
	if err := r.Register(Tool{
		Schema: llm.ToolSchema{Name: "write_file", Description: "d"},
		Handler: func(context.Context, Call) Result {
			ran.Add(1)
			return Result{Text: "wrote the file"}
		},
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

// A policy listener that panics must DENY, never fall through.
//
// This is the whole reason the recovery is per-stage rather than one wrapper.
// The obvious implementation — catch the panic and carry on — treats a gate
// that crashed as a gate that was not registered, and the tool body then runs
// unexamined. A gate that could not run must never produce the same outcome as
// a gate that ran and allowed.
func TestAPanickingPreExecuteListenerDeniesRatherThanFallingThrough(t *testing.T) {
	var ran atomic.Int64
	r := panicRegistry(t, &ran)

	if _, err := bus.OnWaterfall(r.bus, func(e PreExecute, next bus.Next[PreExecute]) PreExecute {
		panic("the gate blew up")
	}); err != nil {
		t.Fatal(err)
	}

	result := r.Run(context.Background(), Call{ID: "1", Name: "write_file"})

	if ran.Load() != 0 {
		t.Fatalf("the tool body ran %d time(s) after the gate panicked; the write went through ungated", ran.Load())
	}
	if !result.IsError {
		t.Fatalf("a panicking gate produced a success: %+v", result)
	}
	if strings.Contains(result.Text, "wrote the file") {
		t.Fatal("the tool's own output came back from a call the gate never approved")
	}
	if !result.Qualified() {
		t.Error("the result is not marked qualified, so the session log cannot tell this from a clean pass")
	}
	if len(result.Degraded) == 0 {
		t.Error("nothing recorded which stage could not run")
	}
}

// A panicking tool body must not report success, and must not take the process
// with it.
func TestAPanickingToolBodyIsAnErrorNotACrash(t *testing.T) {
	r := NewRegistry(bus.New())
	if err := r.Register(Tool{
		Schema:  llm.ToolSchema{Name: "boom", Description: "d"},
		Handler: func(context.Context, Call) Result { panic("handler exploded") },
	}); err != nil {
		t.Fatal(err)
	}

	result := r.Run(context.Background(), Call{ID: "1", Name: "boom"})
	if !result.IsError {
		t.Fatalf("a panicking tool body reported success: %+v", result)
	}
	if !result.Qualified() {
		t.Error("a panicking body produced an unqualified result")
	}

	// And the registry still works afterwards.
	if err := r.Register(Tool{
		Schema:  llm.ToolSchema{Name: "fine", Description: "d"},
		Handler: func(context.Context, Call) Result { return Result{Text: "ok"} },
	}); err != nil {
		t.Fatal(err)
	}
	if out := r.Run(context.Background(), Call{ID: "2", Name: "fine"}); out.IsError {
		t.Fatalf("the registry stopped working after a panic: %+v", out)
	}
}

// A panicking post-execute listener must not hand back the raw result.
//
// Post-execute is redaction. Returning the result because "the work already
// happened" returns text the redactor never finished passing over, which is
// exactly when it must not be returned.
func TestAPanickingPostExecuteListenerDoesNotLeakTheRawResult(t *testing.T) {
	var ran atomic.Int64
	r := NewRegistry(bus.New())
	if err := r.Register(Tool{
		Schema: llm.ToolSchema{Name: "read_secret", Description: "d"},
		Handler: func(context.Context, Call) Result {
			ran.Add(1)
			return Result{Text: "AKIA_SUPER_SECRET_TOKEN"}
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.OnWaterfall(r.bus, func(e PostExecute, next bus.Next[PostExecute]) PostExecute {
		panic("the redactor blew up")
	}); err != nil {
		t.Fatal(err)
	}

	result := r.Run(context.Background(), Call{ID: "1", Name: "read_secret"})
	if strings.Contains(result.Text, "AKIA_SUPER_SECRET_TOKEN") {
		t.Fatal("unredacted output was returned after the redaction stage panicked")
	}
	if !result.IsError {
		t.Fatalf("a panicking redactor produced a success: %+v", result)
	}
	if !result.Qualified() {
		t.Error("the result is not qualified, so nothing records that redaction did not run")
	}
}

// A listener that panics must not stop the next call from being judged. A gate
// that is down for one call and silently absent for every call afterwards is
// worse than one that refuses consistently.
func TestThePipelineKeepsGatingAfterAPanic(t *testing.T) {
	var ran atomic.Int64
	r := panicRegistry(t, &ran)

	var calls atomic.Int64
	if _, err := bus.OnWaterfall(r.bus, func(e PreExecute, next bus.Next[PreExecute]) PreExecute {
		if calls.Add(1) == 1 {
			panic("first call only")
		}
		denied := Errorf("denied by policy")
		e.Decided = &denied
		return e
	}); err != nil {
		t.Fatal(err)
	}

	first := r.Run(context.Background(), Call{ID: "1", Name: "write_file"})
	if !first.IsError {
		t.Fatalf("the panicking call was not refused: %+v", first)
	}
	second := r.Run(context.Background(), Call{ID: "2", Name: "write_file"})
	if !second.IsError || !strings.Contains(second.Text, "denied by policy") {
		t.Fatalf("the gate did not run on the call after a panic: %+v", second)
	}
	if ran.Load() != 0 {
		t.Fatalf("the tool body ran %d time(s); no call was ever approved", ran.Load())
	}
}
