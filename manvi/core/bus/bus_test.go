package bus

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

type evt struct{ Value string }

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

// TestDispatchModeIsPartOfTheContract: dispatching a waterfall event serially
// would drop every listener's return value silently.
func TestDispatchModeIsPartOfTheContract(t *testing.T) {
	b := New()
	if _, err := OnWaterfall(b, func(e evt, next Next[evt]) evt { return next(e) }); err != nil {
		t.Fatal(err)
	}

	if _, err := OnSerial(b, func(context.Context, evt) error { return nil }); err == nil {
		t.Fatal("registering a serial listener on a waterfall event must fail")
	}
	if err := Serial(b, context.Background(), evt{}); err == nil {
		t.Fatal("dispatching a waterfall event as serial must fail")
	}
}

func TestDisposeRemovesTheListener(t *testing.T) {
	b := New()
	calls := 0
	dispose, err := OnSerial(b, func(context.Context, evt) error { calls++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := Serial(b, context.Background(), evt{}); err != nil {
		t.Fatal(err)
	}
	if err := dispose(); err != nil {
		t.Fatal(err)
	}
	if err := Serial(b, context.Background(), evt{}); err != nil {
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
	if err := Serial(b, context.Background(), evt{}); err != nil {
		t.Fatal(err)
	}
}

// A listener whose stored function is not the type its mode dispatches is an
// error from the dispatch, not a panic in the middle of it.
func TestMistypedListenerIsAnErrorNotAPanic(t *testing.T) {
	b := New()
	if _, err := register[evt](b, ModeSerial, "not a function"); err != nil {
		t.Fatal(err)
	}
	if err := Serial(b, context.Background(), evt{}); err == nil {
		t.Fatal("Serial dispatched a listener of the wrong type without reporting it")
	}

	w := New()
	if _, err := register[evt](w, ModeWaterfall, 42); err != nil {
		t.Fatal(err)
	}
	if _, err := Waterfall(w, evt{}); err == nil {
		t.Fatal("Waterfall dispatched a listener of the wrong type without reporting it")
	}
}
