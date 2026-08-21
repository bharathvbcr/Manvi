package bus

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

type stressEvent struct {
	N     int
	Trail []string
}

// The bus binds one dispatch mode per event type and refuses a second — a
// waterfall event cannot also be emitted. That is deliberate: a listener
// registered for the wrong mode would silently never run, and a policy
// listener that never runs is indistinguishable from one that ran and allowed.
// So the modes get distinct types here, as they do in the pipeline.
type plainEvent struct{ N int }
type serialEvent struct{ N int }

// Listeners register and dispose while events are in flight.
//
// This is not a hypothetical shape. The tool pipeline's three stages are
// waterfalls on this bus, and the harness now activates tools during a turn —
// so listeners come and go while dispatch is running. A registry that tore
// during that would show up as a policy listener that did not run, which is
// indistinguishable from one that ran and allowed.
func TestConcurrentRegisterDisposeAndEmit(t *testing.T) {
	b := New()
	var delivered atomic.Int64

	var wg sync.WaitGroup

	// Churn: register and immediately dispose. Bounded rather than spinning
	// until a stop signal, so the whole test stays cheap enough to run under
	// -race on every commit — a stress test that only runs when someone
	// remembers to is not a guard.
	const churners, churnEach = 4, 300
	for i := 0; i < churners; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < churnEach; j++ {
				dispose, err := OnWaterfall(b, func(e stressEvent, next Next[stressEvent]) stressEvent {
					e.N++
					return next(e)
				})
				if err != nil {
					t.Errorf("register: %v", err)
					return
				}
				if err := dispose(); err != nil {
					t.Errorf("dispose: %v", err)
					return
				}
			}
		}()
	}

	// Emitters, running against that churn.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				out, err := Waterfall(b, stressEvent{})
				if err != nil {
					t.Errorf("waterfall: %v", err)
					return
				}
				if out.N < 0 {
					t.Errorf("waterfall produced a negative count: %d", out.N)
					return
				}
				delivered.Add(1)
			}
		}()
	}

	// Plain listeners and serial listeners on the same bus at the same time.
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if err := Emit(b, plainEvent{N: j}); err != nil {
					t.Errorf("emit: %v", err)
					return
				}
				if err := Serial(b, context.Background(), serialEvent{N: j}); err != nil {
					t.Errorf("serial: %v", err)
					return
				}
			}
		}()
	}

	wg.Wait()

	if delivered.Load() == 0 {
		t.Fatal("no events were delivered; the stress ran against nothing")
	}
}

// A waterfall must apply every registered listener exactly once, in order, and
// hand the last one's value back.
//
// The pipeline depends on this precisely: a pre-execute listener rewrites the
// call — a path normaliser, a scope clamp — and the rewritten form is what runs.
// A listener applied twice would clamp a clamped path; one skipped would run
// the raw one.
func TestWaterfallAppliesEveryListenerOnceInOrder(t *testing.T) {
	for _, count := range []int{0, 1, 2, 5, 25, 100} {
		t.Run(fmt.Sprintf("listeners=%d", count), func(t *testing.T) {
			b := New()
			for i := 0; i < count; i++ {
				label := fmt.Sprintf("L%d", i)
				if _, err := OnWaterfall(b, func(e stressEvent, next Next[stressEvent]) stressEvent {
					e.N++
					e.Trail = append(e.Trail, label)
					return next(e)
				}); err != nil {
					t.Fatal(err)
				}
			}
			out, err := Waterfall(b, stressEvent{})
			if err != nil {
				t.Fatal(err)
			}
			if out.N != count {
				t.Fatalf("listener count = %d, want %d (each must run exactly once)", out.N, count)
			}
			if len(out.Trail) != count {
				t.Fatalf("trail = %v, want %d entries", out.Trail, count)
			}
			for i, label := range out.Trail {
				if want := fmt.Sprintf("L%d", i); label != want {
					t.Fatalf("trail[%d] = %q, want %q: listeners ran out of order", i, label, want)
				}
			}
		})
	}
}
func TestDisposeIsIdempotent(t *testing.T) {
	b := New()
	dispose, err := On(b, func(plainEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	if err := dispose(); err != nil {
		t.Fatalf("first dispose: %v", err)
	}
	if err := dispose(); err != nil {
		t.Fatalf("second dispose returned an error: %v", err)
	}
	// And the bus still works afterwards.
	if err := Emit(b, plainEvent{}); err != nil {
		t.Fatalf("emit after double dispose: %v", err)
	}
}
