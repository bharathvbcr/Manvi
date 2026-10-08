// Package bus is the harness's typed event bus, with the two dispatch modes
// the plugin model is built on.
//
//	Waterfall  around-middleware: a listener may wrap, replace, or short-circuit
//	Serial     ordered, may fail, first error stops the chain
//
// Waterfall is the one that matters. A listener receives (event, next): call
// next to delegate to the rest of the chain, or return without calling it to
// short-circuit and own the decision. That is how the write gate denies a tool
// call without the loop containing any knowledge of policy.
//
// Dispatch mode is part of an event's contract, not a per-call choice.
// Dispatching a waterfall event serially would silently drop every listener's
// return value, so the bus refuses it.
package bus

import (
	"context"
	"fmt"
	"reflect"
	"sync"
)

// Dispose removes a listener.
type Dispose func() error

// Mode is how an event type is dispatched.
type Mode string

const (
	ModeWaterfall Mode = "waterfall"
	ModeSerial    Mode = "serial"
)

// Next delegates to the remainder of a waterfall chain.
type Next[E any] func(E) E

type listener struct {
	id uint64
	fn any
}

type slot struct {
	mode      Mode
	listeners []listener
}

// Bus holds listeners keyed by event type.
type Bus struct {
	mu     sync.RWMutex
	slots  map[reflect.Type]*slot
	nextID uint64
}

// New returns an empty bus.
func New() *Bus {
	return &Bus{slots: map[reflect.Type]*slot{}}
}

func register[E any](b *Bus, mode Mode, fn any) (Dispose, error) {
	key := reflect.TypeOf((*E)(nil)).Elem()

	b.mu.Lock()
	defer b.mu.Unlock()

	s, ok := b.slots[key]
	if !ok {
		s = &slot{mode: mode}
		b.slots[key] = s
	}
	if s.mode != mode {
		return nil, fmt.Errorf("bus: %s is a %s event; cannot register a %s listener",
			key, s.mode, mode)
	}

	b.nextID++
	l := listener{id: b.nextID, fn: fn}
	s.listeners = append(s.listeners, l)

	id := l.id
	return func() error {
		b.mu.Lock()
		defer b.mu.Unlock()
		kept := s.listeners[:0]
		for _, existing := range s.listeners {
			if existing.id != id {
				kept = append(kept, existing)
			}
		}
		s.listeners = kept
		return nil
	}, nil
}

func snapshot[E any](b *Bus, want Mode) ([]any, error) {
	key := reflect.TypeOf((*E)(nil)).Elem()
	b.mu.RLock()
	defer b.mu.RUnlock()
	s, ok := b.slots[key]
	if !ok {
		return nil, nil
	}
	if s.mode != want {
		return nil, fmt.Errorf("bus: %s is a %s event; cannot dispatch it as %s",
			key, s.mode, want)
	}
	out := make([]any, len(s.listeners))
	for i, l := range s.listeners {
		out[i] = l.fn
	}
	return out, nil
}

// OnWaterfall registers around-middleware.
func OnWaterfall[E any](b *Bus, fn func(E, Next[E]) E) (Dispose, error) {
	return register[E](b, ModeWaterfall, fn)
}

// Waterfall runs the chain. A listener that returns without calling next
// short-circuits: the listeners behind it never run, and the caller cannot tell
// which one decided — which is the point.
func Waterfall[E any](b *Bus, event E) (E, error) {
	fns, err := snapshot[E](b, ModeWaterfall)
	if err != nil {
		return event, err
	}
	var run func(i int, v E) E
	run = func(i int, v E) E {
		if i >= len(fns) {
			return v
		}
		return fns[i].(func(E, Next[E]) E)(v, func(nv E) E { return run(i+1, nv) })
	}
	return run(0, event), nil
}

// OnSerial registers an ordered, fallible listener.
func OnSerial[E any](b *Bus, fn func(context.Context, E) error) (Dispose, error) {
	return register[E](b, ModeSerial, fn)
}

// Serial runs listeners in order and stops at the first error.
func Serial[E any](b *Bus, ctx context.Context, event E) error {
	fns, err := snapshot[E](b, ModeSerial)
	if err != nil {
		return err
	}
	for _, fn := range fns {
		if err := fn.(func(context.Context, E) error)(ctx, event); err != nil {
			return err
		}
	}
	return nil
}
