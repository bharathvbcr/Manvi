package bus

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

type evt struct{ Value string }

func TestEmitObservesInRegistrationOrder(t *testing.T) {
	b := New()
	var order []string
	for _, name := range []string{"a", "b", "c"} {
		if _, err := On(b, func(evt) { order = append(order, name) }); err != nil {
			t.Fatal(err)
		}
	}
	if err := Emit(b, evt{}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, "") != "abc" {
		t.Fatalf("order = %v", order)
	}
}

func TestWaterfallWraps(t *testing.T) {
	b := New()
	if _, err := OnWaterfall(b, func(e evt, next Next[evt]) evt {
		e.Value += "outer("
		e = next(e)
		return evt{Value: e.Value + ")"}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := OnWaterfall(b, func(e evt, next Next[evt]) evt {
		e.Value += "inner"
		return next(e)
	}); err != nil {
		t.Fatal(err)
	}

	out, err := Waterfall(b, evt{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Value != "outer(inner)" {
		t.Fatalf("waterfall = %q, want around-middleware nesting", out.Value)
	}
}

// TestWaterfallShortCircuit is the property the tool pipeline depends on: a
// listener that returns without calling next owns the decision, and the
// listeners behind it never run.
func TestWaterfallShortCircuit(t *testing.T) {
	b := New()
	reached := false
	if _, err := OnWaterfall(b, func(e evt, next Next[evt]) evt {
		return evt{Value: "denied"} // no next()
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := OnWaterfall(b, func(e evt, next Next[evt]) evt {
		reached = true
		return next(e)
	}); err != nil {
		t.Fatal(err)
	}

	out, err := Waterfall(b, evt{Value: "start"})
	if err != nil {
		t.Fatal(err)
	}
	if reached {
		t.Fatal("a short-circuited waterfall must not reach later listeners")
	}
	if out.Value != "denied" {
		t.Fatalf("result = %q", out.Value)
	}
}

func TestPrependRunsFirst(t *testing.T) {
	b := New()
	var order []string
	if _, err := OnWaterfall(b, func(e evt, next Next[evt]) evt {
		order = append(order, "normal")
		return next(e)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := OnWaterfall(b, func(e evt, next Next[evt]) evt {
		order = append(order, "prepended")
		return next(e)
	}, Prepend()); err != nil {
		t.Fatal(err)
	}
	if _, err := Waterfall(b, evt{}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "prepended,normal" {
		t.Fatalf("order = %v", order)
	}
}

func TestSerialStopsAtFirstError(t *testing.T) {
	b := New()
	var ran int32
	if _, err := OnSerial(b, func(context.Context, evt) error {
		atomic.AddInt32(&ran, 1)
		return errors.New("stop")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := OnSerial(b, func(context.Context, evt) error {
		atomic.AddInt32(&ran, 1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := Serial(b, context.Background(), evt{}); err == nil {
		t.Fatal("expected the error to propagate")
	}
	if ran != 1 {
		t.Fatalf("%d listeners ran; serial must stop at the first error", ran)
	}
}

// TestParallelRunsEveryListenerDespiteFailures: one listener failing must not
// stop the others, which is what distinguishes parallel from serial.
func TestParallelRunsEveryListenerDespiteFailures(t *testing.T) {
	b := New()
	var ran int32
	for i := range 4 {
		if _, err := OnParallel(b, func(context.Context, evt) error {
			atomic.AddInt32(&ran, 1)
			if i%2 == 0 {
				return errors.New("boom")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	err := Parallel(b, context.Background(), evt{})
	if err == nil {
		t.Fatal("errors must be collected, not swallowed")
	}
	if ran != 4 {
		t.Fatalf("%d listeners ran, want all 4", ran)
	}
	if strings.Count(err.Error(), "boom") != 2 {
		t.Fatalf("expected both failures reported, got %v", err)
	}
}

// TestDispatchModeIsPartOfTheContract: emitting a waterfall event would drop
// every listener's return value silently.
func TestDispatchModeIsPartOfTheContract(t *testing.T) {
	b := New()
	if _, err := OnWaterfall(b, func(e evt, next Next[evt]) evt { return next(e) }); err != nil {
		t.Fatal(err)
	}

	if _, err := On(b, func(evt) {}); err == nil {
		t.Fatal("registering an emit listener on a waterfall event must fail")
	}
	if err := Emit(b, evt{}); err == nil {
		t.Fatal("dispatching a waterfall event as emit must fail")
	}
	if mode, ok := ModeOf[evt](b); !ok || mode != ModeWaterfall {
		t.Fatalf("ModeOf = %q, %v", mode, ok)
	}
}

func TestDisposeRemovesTheListener(t *testing.T) {
	b := New()
	calls := 0
	dispose, err := On(b, func(evt) { calls++ })
	if err != nil {
		t.Fatal(err)
	}
	if err := Emit(b, evt{}); err != nil {
		t.Fatal(err)
	}
	if err := dispose(); err != nil {
		t.Fatal(err)
	}
	if err := Emit(b, evt{}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("listener ran %d times after disposal", calls)
	}
}

func TestDispatchWithNoListenersIsANoop(t *testing.T) {
	b := New()
	out, err := Waterfall(b, evt{Value: "unchanged"})
	if err != nil || out.Value != "unchanged" {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
	if err := Emit(b, evt{}); err != nil {
		t.Fatal(err)
	}
}
