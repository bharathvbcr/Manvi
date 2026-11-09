package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
)

// bank.balance has no recovery, so its counts do not move: 5 captures, 2 of
// them post-action, 2 resolves, and the only capture a resolve may name is the
// wait's.
func TestReadStepsShareOneFreshSnapshotAndNothingElseIsReused(t *testing.T) {
	p, inputs := compileBank(t)
	res := play(t, p, inputs, desk{frameFor: func(int, bool, int) frame { return frame{status: "Member found", balance: settled} }})
	if res.state.Phase != Completed {
		t.Fatalf("%+v", res.state)
	}
	t.Logf("bank.balance commands per run: %v", res.counts)
	if res.counts["observe"]+res.counts["observe_after"] != 5 || res.counts["observe_after"] != 2 || res.counts["resolve"] != 2 {
		t.Fatalf("want 5 fresh captures (2 post-action) and 2 resolves, got %v", res.counts)
	}
	if len(res.violations) != 0 {
		t.Fatalf("reuse invariant broken: %v", res.violations)
	}
	named := map[string]string{}
	for _, c := range res.commands {
		if c.Kind == "resolve" {
			info := res.captures[c.ObservationID]
			named[c.ObservationID] = info.kind + " for " + c.Step.ID
		}
	}
	if len(named) != 1 {
		t.Fatalf("every resolve must read the one snapshot the wait took, got %v", named)
	}
	for id := range named {
		if info := res.captures[id]; info.kind != "observe" || info.sinceLast < 2 {
			t.Fatalf("%s is a %s, capture #%d since the last dispatch", id, info.kind, info.sinceLast)
		}
	}
}

// press -> wait (an overlay hides the status; a recovery clicks dismiss) ->
// extract. The frame taken right after the recovery click still shows the
// previous member's balance; the extract must be answered from a later one.
func TestCaptureAfterARecoveryClickIsNeverReused(t *testing.T) {
	p, inputs := recoveryBank(t)
	res := play(t, p, inputs, desk{frameFor: func(dispatches int, first bool, fresh int) frame {
		switch {
		case dispatches == 1 && !first:
			return frame{blocked: true} // overlay still up when the wait looks
		case first:
			return frame{status: "Member found", balance: 99900} // not repainted yet
		default:
			return frame{status: "Member found", balance: settled}
		}
	}})
	t.Logf("recovery capability commands per run: %v", res.counts)
	if res.state.Phase != Completed || res.state.Outputs["balance"].Integer != settled {
		t.Fatalf("balance %d phase %s, the settled screen shows %d (commands %v)", res.state.Outputs["balance"].Integer, res.state.Phase, settled, res.counts)
	}
	if len(res.violations) != 0 || res.counts["resolve"] != 0 {
		t.Fatalf("a capture taken after a click was reused: %v (commands %v)", res.violations, res.counts)
	}
	if res.counts["act"] != 2 || res.counts["observe"]+res.counts["observe_after"] != 5 {
		t.Fatalf("want 2 dispatches and 5 fresh captures, got %v", res.counts)
	}
}

// The invariant, asserted on the command stream of generated runs: no resolve
// names a capture older than the last dispatch, or the first one taken after a
// dispatch of any kind (a step's action, a recovery, an approved action, a
// human action while paused). Deterministic: a fixed seed, bounded size.
func TestPropertyNoResolveNamesAFirstOrOlderCapture(t *testing.T) {
	rng := rand.New(rand.NewSource(20261109))
	resolves, recoveries, humans, pauses := 0, 0, 0, 0
	for i := 0; i < 400; i++ {
		p := randomProgram(t, rng)
		inputs := map[string]Value{}
		mishapRate := 0.05 + 0.25*rng.Float64()
		res := play(t, p, inputs, desk{
			frameFor: func(dispatches int, first bool, fresh int) frame {
				f := frame{status: "ok", balance: int64(100 + fresh)}
				switch r := rng.Float64(); {
				case r < 0.15:
					f.status = "pending"
				case r < 0.25:
					f.blocked = true
				}
				return f
			},
			mishap: func(c Command) string {
				if rng.Float64() >= mishapRate {
					return ""
				}
				switch c.Kind {
				case "act":
					return []string{"not_sent", "external"}[rng.Intn(2)]
				case "resolve":
					return []string{"declined", "ambiguous", "failed"}[rng.Intn(3)]
				}
				return []string{"failed", "ambiguous"}[rng.Intn(2)]
			},
			humanDispatch: func() bool {
				if rng.Intn(2) == 0 {
					humans++
					return true
				}
				return false
			},
		})
		if len(res.violations) != 0 {
			t.Fatalf("program %d broke the reuse invariant: %v\ncommands: %v", i, res.violations, res.counts)
		}
		resolves += res.counts["resolve"]
		for _, c := range res.commands {
			if c.RecoveryID != "" {
				recoveries++
			}
		}
		for _, e := range res.events {
			if e.Kind == "resume" {
				pauses++
			}
		}
		// A finished or paused state must encode neither field: Jarvis decodes
		// traces strictly and rejects unknown fields.
		if s := res.state; s.Phase != Observing && s.Phase != PostAction {
			b, _ := json.Marshal(s)
			if bytes.Contains(b, []byte(`"reuse"`)) || bytes.Contains(b, []byte(`"captures"`)) {
				t.Fatalf("program %d ended in %s carrying a reuse field: %s", i, s.Phase, b)
			}
		}
	}
	t.Logf("400 generated programs: %d resolves, %d recovery clicks, %d resumes, %d human dispatches", resolves, recoveries, pauses, humans)
	if resolves == 0 || recoveries == 0 || pauses == 0 || humans == 0 {
		t.Fatal("the generator never reached a path it is meant to cover")
	}
}

// randomProgram builds 2-7 steps from reads, changes and presses, with a
// recovery on the first target that a read can find hidden.
func randomProgram(t *testing.T, rng *rand.Rand) *Program {
	t.Helper()
	targets := map[string]Selector{"dismiss": {Role: "button", Name: "Dismiss"}}
	var steps []Step
	n := 2 + rng.Intn(6)
	for i := 0; i < n; i++ {
		tgt := fmt.Sprintf("t%d", i)
		targets[tgt] = Selector{Role: "text_field", Name: fmt.Sprintf("Field %d", i)}
		id := fmt.Sprintf("s%d", i)
		if i == 0 && rng.Intn(2) == 0 {
			id = "settle" // the driver's capture-failure hook keys on this name
		}
		match := Predicate{Op: "contains", Expected: Ref{Source: "literal", Literal: Value{Type: "string", Text: "ok"}}}
		switch rng.Intn(5) {
		case 0:
			steps = append(steps, Step{ID: id, Kind: "press", Target: tgt, Effect: "read"})
		case 1:
			steps = append(steps, Step{ID: id, Kind: "press", Target: tgt, Effect: "change"})
		case 2:
			steps = append(steps, Step{ID: id, Kind: "wait", Target: tgt, Effect: "read", Predicate: match})
		case 3:
			steps = append(steps, Step{ID: id, Kind: "assert", Target: tgt, Effect: "read", Predicate: match})
		default:
			steps = append(steps, Step{ID: id, Kind: "extract", Target: tgt, Effect: "read", Output: "o" + id, OutputType: "money", Currency: "USD"})
		}
	}
	targets["tend"] = Selector{Role: "text_field", Name: "Total"}
	steps = append(steps, Step{ID: "send", Kind: "extract", Target: "tend", Effect: "read", Output: "total", OutputType: "money", Currency: "USD"})
	c := Capability{SchemaVersion: 1, ID: "gen", Revision: "1", Application: "bank", Targets: targets, Limits: DefaultLimits(), Steps: steps,
		Recoveries: []Recovery{{ID: "dismiss_overlay",
			When:   RecoveryWhen{Target: "t0", Predicate: Predicate{Op: "equals", Expected: Ref{Source: "literal", Literal: Value{Type: "string", Text: "blocked"}}}},
			Action: RecoveryAction{Kind: "press", Target: "dismiss"}, Max: 2}}}
	return mustCompile(t, c)
}
