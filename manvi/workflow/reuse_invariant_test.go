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
	res := play(t, p, inputs, desk{frameFor: func(int, bool, int, string) frame { return frame{status: "Member found", balance: settled} }})
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
			t.Fatalf("%s is a %s, capture #%d since the last screen change", id, info.kind, info.sinceLast)
		}
	}
}

// press -> wait (an overlay hides the status; a recovery clicks dismiss) ->
// extract. The frame taken right after the recovery click still shows the
// previous member's balance; the extract must be answered from a later one.
func TestCaptureAfterARecoveryClickIsNeverReused(t *testing.T) {
	p, inputs := recoveryBank(t)
	res := play(t, p, inputs, desk{frameFor: func(era int, first bool, fresh int, target string) frame {
		switch {
		case fresh == 3:
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

// A recovery click that is not sent (the window moved) keeps the step and
// retries with a window activation before every capture. The frame taken right
// after an activation has had no settle either, so the extract that follows the
// wait must not be answered from it.
func TestCaptureAfterAWindowActivationIsNeverReused(t *testing.T) {
	p, inputs := recoveryBank(t)
	res := play(t, p, inputs, desk{
		frameFor: func(era int, first bool, fresh int, target string) frame {
			switch fresh {
			case 3:
				return frame{blocked: true}
			case 4:
				return frame{status: "Searching"} // the wait is not met yet
			case 5:
				return frame{status: "Member found", balance: 99900} // right after the activation
			default:
				return frame{status: "Member found", balance: settled}
			}
		},
		mishap: func(c Command) string {
			if c.Kind == "act" && c.RecoveryID != "" {
				return "not_sent"
			}
			return ""
		},
	})
	refocused := 0
	for _, c := range res.commands {
		if c.Refocus {
			refocused++
		}
	}
	t.Logf("commands per run: %v, %d refocusing captures", res.counts, refocused)
	if refocused < 2 {
		t.Fatalf("the scenario must retry with a refocus at least twice, got %d", refocused)
	}
	if res.state.Phase != Completed || res.state.Outputs["balance"].Integer != settled {
		t.Fatalf("balance %d phase %s, the settled screen shows %d", res.state.Outputs["balance"].Integer, res.state.Phase, settled)
	}
	if len(res.violations) != 0 || res.counts["resolve"] != 0 {
		t.Fatalf("a capture taken right after a window activation was reused: %v (commands %v)", res.violations, res.counts)
	}
}

// An incomplete frame is not a usable frame, so it neither settles the screen
// nor counts towards reuse: the runner keeps only complete frames.
func TestIncompleteFramesDoNotCountTowardsReuse(t *testing.T) {
	p, inputs := compileBank(t)
	firstAfter := 0
	res := play(t, p, inputs, desk{
		frameFor: func(int, bool, int, string) frame { return frame{status: "Member found", balance: settled} },
		mishap: func(c Command) string {
			// The first post-action capture comes back incomplete and is taken again.
			// Only the retry is a usable frame, and it is the first one after the press,
			// so it must not be offered to the wait that follows. Counting the incomplete
			// frame would make that retry the second.
			if c.Kind == "observe_after" {
				firstAfter++
				if firstAfter == 2 { // the search press's post-action capture, not the field set's
					return "incomplete"
				}
			}
			return ""
		},
	})
	if len(res.violations) != 0 {
		t.Fatalf("%v", res.violations)
	}
	if res.state.Phase != Completed || res.counts["observe_after"] != 3 {
		t.Fatalf("the scenario must retry one post-action capture and complete: %+v %v", res.state, res.counts)
	}
}

// The invariant, asserted on the command stream of generated runs. The oracle's
// own definition of "the screen may have changed" is read off the commands and
// events it sees, not off the reducer's state: any act, any command carrying
// Refocus, and any resume restarts it (a resume whether or not a person
// dispatched). No resolve may name a capture from an earlier era or the first
// complete one in its era. Deterministic: a fixed seed, bounded size.
func TestPropertyNoResolveNamesAFirstOrOlderCapture(t *testing.T) {
	rng := rand.New(rand.NewSource(20261109))
	var resolves, recoveries, humans, resumes, refocuses, incompletes, branchResolves int
	for i := 0; i < 600; i++ {
		p := randomProgram(t, rng)
		mishapRate := 0.05 + 0.3*rng.Float64()
		res := play(t, p, map[string]Value{}, desk{
			frameFor: func(era int, first bool, fresh int, target string) frame {
				f := frame{status: "ok", balance: int64(100 + fresh)}
				switch r := rng.Float64(); {
				case r < 0.15:
					f.status = "pending"
				case r < 0.30:
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
					return []string{"not_sent", "not_sent", "external"}[rng.Intn(3)]
				case "resolve":
					return []string{"declined", "ambiguous", "failed", "incomplete"}[rng.Intn(4)]
				}
				return []string{"failed", "ambiguous", "incomplete"}[rng.Intn(3)]
			},
			humanDispatch: func() bool {
				humans++
				return rng.Intn(2) == 0
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
			if c.Refocus {
				refocuses++
			}
			if c.Kind == "resolve" && c.Step.Kind == "branch" {
				branchResolves++
			}
		}
		for _, e := range res.events {
			if e.Kind == "resume" {
				resumes++
			}
			if e.Kind == "observed" && !e.Observation.Complete {
				incompletes++
			}
		}
		// A finished or paused state must encode neither field: Jarvis decodes
		// traces strictly and rejects unknown fields.
		if s := res.state; s.Phase != Observing && s.Phase != PostAction {
			b, err := json.Marshal(s)
			if err != nil {
				t.Fatalf("program %d: %v", i, err)
			}
			if bytes.Contains(b, []byte(`"reuse"`)) || bytes.Contains(b, []byte(`"captures"`)) {
				t.Fatalf("program %d ended in %s carrying a reuse field: %s", i, s.Phase, b)
			}
		}
	}
	t.Logf("600 generated programs: %d resolves (%d for branches), %d recovery clicks, %d refocusing captures, %d resumes, %d incomplete frames", resolves, branchResolves, recoveries, refocuses, resumes, incompletes)
	if resolves == 0 || recoveries == 0 || resumes == 0 || refocuses == 0 || incompletes == 0 || branchResolves == 0 {
		t.Fatal("the generator never reached a path it is meant to cover")
	}
}

// randomProgram builds 2-7 steps from reads, branches, changes and presses, with
// a recovery on several of the targets (not just the first).
func randomProgram(t *testing.T, rng *rand.Rand) *Program {
	t.Helper()
	targets := map[string]Selector{"dismiss": {Role: "button", Name: "Dismiss"}}
	var steps []Step
	n := 2 + rng.Intn(6)
	var recoverable []string
	for i := 0; i < n; i++ {
		tgt := fmt.Sprintf("t%d", i)
		targets[tgt] = Selector{Role: "text_field", Name: fmt.Sprintf("Field %d", i)}
		if rng.Intn(2) == 0 {
			recoverable = append(recoverable, tgt)
		}
		id := fmt.Sprintf("s%d", i)
		if i == 0 && rng.Intn(2) == 0 {
			id = "settle" // the driver's capture-failure hook keys on this name
		}
		match := Predicate{Op: "contains", Expected: Ref{Source: "literal", Literal: Value{Type: "string", Text: "ok"}}}
		switch rng.Intn(6) {
		case 0:
			steps = append(steps, Step{ID: id, Kind: "press", Target: tgt, Effect: "read"})
		case 1:
			steps = append(steps, Step{ID: id, Kind: "press", Target: tgt, Effect: "change"})
		case 2:
			steps = append(steps, Step{ID: id, Kind: "wait", Target: tgt, Effect: "read", Predicate: match})
		case 3:
			steps = append(steps, Step{ID: id, Kind: "assert", Target: tgt, Effect: "read", Predicate: match})
		case 4:
			steps = append(steps, Step{ID: id, Kind: "branch", Target: tgt, Effect: "read", Predicate: match})
		default:
			steps = append(steps, Step{ID: id, Kind: "extract", Target: tgt, Effect: "read", Output: "o" + id, OutputType: "money", Currency: "USD"})
		}
	}
	targets["tend"] = Selector{Role: "text_field", Name: "Total"}
	steps = append(steps, Step{ID: "send", Kind: "extract", Target: "tend", Effect: "read", Output: "total", OutputType: "money", Currency: "USD"})
	for i := range steps[:len(steps)-1] {
		if steps[i].Kind == "branch" {
			steps[i].Next, steps[i].Otherwise = steps[i+1].ID, "send"
		}
	}
	var recoveries []Recovery
	for _, tgt := range recoverable {
		recoveries = append(recoveries, Recovery{ID: "dismiss_" + tgt,
			When:   RecoveryWhen{Target: tgt, Predicate: Predicate{Op: "equals", Expected: Ref{Source: "literal", Literal: Value{Type: "string", Text: "blocked"}}}},
			Action: RecoveryAction{Kind: "press", Target: "dismiss"}, Max: 2})
	}
	c := Capability{SchemaVersion: 1, ID: "gen", Revision: "1", Application: "bank", Targets: targets, Limits: DefaultLimits(), Steps: steps, Recoveries: recoveries}
	return mustCompile(t, c)
}
