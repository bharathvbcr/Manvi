package workflow

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// State, Event and Result are written to durable traces and evidence bundles and
// are later decoded and compared against a recomputed value. That comparison is
// only sound if every value these constructors produce survives a JSON
// round-trip unchanged, so the invariant is asserted here rather than left to
// the consumers that would otherwise discover it as a replay failure.

// assertRoundTrip fails when v does not decode back to an equal value.
func assertRoundTrip(t *testing.T, label string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: marshal: %v", label, err)
	}
	fresh := reflect.New(reflect.TypeOf(v))
	if err := json.Unmarshal(raw, fresh.Interface()); err != nil {
		t.Fatalf("%s: unmarshal: %v", label, err)
	}
	if back := fresh.Elem().Interface(); !reflect.DeepEqual(v, back) {
		t.Errorf("%s does not round-trip through JSON\n encoded: %s\noriginal: %#v\n decoded: %#v", label, raw, v, back)
	}
	assertNoEagerEmptyOmitempty(t, label, reflect.ValueOf(v))
}

// assertNoEagerEmptyOmitempty reports the precise defect shape that breaks
// round-trip equality: a map or slice tagged omitempty but allocated empty.
// Encoding drops such a field and decoding yields nil, so DeepEqual fails even
// though no information was lost. Either drop omitempty or leave the field nil.
func assertNoEagerEmptyOmitempty(t *testing.T, label string, val reflect.Value) {
	t.Helper()
	for val.Kind() == reflect.Pointer {
		if val.IsNil() {
			return
		}
		val = val.Elem()
	}
	if val.Kind() != reflect.Struct {
		return
	}
	typ := val.Type()
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		fv := val.Field(i)
		name := label + "." + f.Name
		switch fv.Kind() {
		case reflect.Map, reflect.Slice:
			if strings.Contains(f.Tag.Get("json"), ",omitempty") && !fv.IsNil() && fv.Len() == 0 {
				t.Errorf("%s is tagged omitempty but allocated empty; encode drops it and decode yields nil", name)
			}
		case reflect.Struct, reflect.Pointer:
			assertNoEagerEmptyOmitempty(t, name, fv)
		}
	}
}

func TestStateRoundTripsThroughJSON(t *testing.T) {
	p := mustCompile(t, schemaBase(t))
	fresh, err := NewState(p, "run", "session", 1)
	if err != nil {
		t.Fatal(err)
	}
	assertRoundTrip(t, "NewState", fresh)

	started := start(t, p)
	assertRoundTrip(t, "started", started)

	// Drive the balance flow far enough to populate Outputs.
	s := started
	e := observed(s)
	if s, _, err = Reduce(p, s, e, nil); err != nil {
		t.Fatal(err)
	}
	assertRoundTrip(t, "afterObserve", s)

	cancelled, _, err := Reduce(p, started, Event{Sequence: started.Sequence + 1, RunID: started.RunID, SessionID: started.SessionID, Epoch: started.Epoch, Kind: "cancel"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertRoundTrip(t, "cancelled", cancelled)
}

// TestRecoveryStateRoundTripsThroughJSON covers the populated RecoveryCounts
// path as well as the empty one, because only the empty map was ever dropped.
func TestRecoveryStateRoundTripsThroughJSON(t *testing.T) {
	c := schemaBase(t)
	c.Recoveries = []Recovery{{
		ID:     "dismiss_overlay",
		When:   RecoveryWhen{Target: "search", Predicate: Predicate{Op: "equals", Expected: Ref{Source: "literal", Literal: Value{Type: "string", Text: "blocked"}}}},
		Action: RecoveryAction{Kind: "press", Target: "dismiss"},
		Max:    1,
	}}
	c.Steps = []Step{
		{ID: "search", Kind: "press", Target: "search", Effect: "read"},
		{ID: "ok", Kind: "conclude", Outcome: "success"},
	}
	c.Outputs = nil
	c.Outcomes = []OutcomeDef{{ID: "success", Kind: "success", Description: "ok", Outputs: []string{}}}
	p := mustCompile(t, c)

	s := start(t, p)
	assertRoundTrip(t, "beforeRecovery", s)

	e := observed(s)
	e.Observation = Observation{ID: "obs", TargetID: "node", Target: "search", Matches: 0, Complete: true, Value: Value{Type: "string", Text: "blocked"}}
	s, _, err := Reduce(p, s, e, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.ActiveRecovery != "dismiss_overlay" {
		t.Fatalf("recovery did not fire: %+v", s)
	}
	if len(s.RecoveryCounts) == 0 {
		t.Fatalf("recovery counts not populated: %+v", s)
	}
	assertRoundTrip(t, "afterRecovery", s)
}

func TestResultRoundTripsThroughJSON(t *testing.T) {
	p := mustCompile(t, schemaBase(t))
	base, err := NewState(p, "run", "session", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []Phase{Ready, Observing, Acting, Paused, Completed, Concluded, Failed, Cancelled, Unknown} {
		s := base
		s.Phase = phase
		assertRoundTrip(t, "Result/"+string(phase), ResultFromState(p, s))
	}

	withOutputs := base
	withOutputs.Phase = Completed
	withOutputs.Outputs = map[string]Value{"balance": {Type: "money", Integer: 125000, Currency: "USD"}}
	r := ResultFromState(p, withOutputs)
	if len(r.Outputs) != 1 {
		t.Fatalf("outputs not carried: %+v", r)
	}
	assertRoundTrip(t, "Result/withOutputs", r)
}

// TestEventRoundTripsThroughJSON pins the third type recorded in traces.
func TestEventRoundTripsThroughJSON(t *testing.T) {
	p := mustCompile(t, schemaBase(t))
	s := start(t, p)
	assertRoundTrip(t, "Event/input", inputEvent(s, "observed"))
	assertRoundTrip(t, "Event/observed", observed(s))
}
