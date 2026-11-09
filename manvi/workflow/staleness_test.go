package workflow

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// screen is what the bank window shows. The app repaints a moment after the
// press, so the capture taken immediately after a dispatch can still show the
// previous member's result.
type screen struct {
	status  string
	balance int64
}

// simulate plays a desktop whose screen changes between captures. frames
// yields what each successive FRESH capture shows; justActed marks the capture
// taken right after a dispatch (observe_after). A resolve answers from the
// frame of the capture it names, as the broker does. onResolve sees every
// resolve so a test can assert which captures were reused.
func simulate(t *testing.T, failWaitFirst int, frames func(afterDispatch bool, fresh int) screen) (State, map[string]int, map[string]string) {
	t.Helper()
	p, err := Compile([]byte(bankBalance))
	if err != nil {
		t.Fatal(err)
	}
	inputs := map[string]Value{"member_id": {Type: "string", Text: "M-1001"}}
	s, _ := NewState(p, "run", "session", 1)
	counts := map[string]int{}
	taken := map[string]screen{}
	stage := map[string]string{} // capture id -> the command kind that took it
	fresh, waitFails := 0, 0
	justActed := false
	ev := Event{Kind: "start"}
	for guard := 0; guard < 300 && !s.Terminal(); guard++ {
		ev.Sequence, ev.RunID, ev.SessionID, ev.Epoch = s.Sequence+1, s.RunID, s.SessionID, s.Epoch
		next, cmds, err := Reduce(p, s, ev, inputs)
		if err != nil {
			t.Fatalf("event %d %s: %v", ev.Sequence, ev.Kind, err)
		}
		s = next
		if len(cmds) == 0 {
			if s.Phase == Paused {
				ev = Event{Kind: "resume", NextEpoch: s.Epoch + 1}
			}
			continue
		}
		c := cmds[0]
		counts[c.Kind]++
		switch c.Kind {
		case "observe", "observe_after", "resolve":
			if c.Kind == "observe" && c.Step.ID == "settle" && waitFails < failWaitFirst {
				waitFails++
				ev = Event{Kind: "observe_failed", ActionID: c.ActionID, FailureCode: "capture_failed", Reason: "transient"}
				continue
			}
			id := c.ObservationID
			if c.Kind != "resolve" {
				fresh++
				id = fmt.Sprintf("obs-%d", fresh)
				taken[id] = frames(justActed, fresh)
				stage[id] = c.Kind
				justActed = false
			}
			f, ok := taken[id]
			if !ok {
				t.Fatalf("resolve names capture %q that was never taken", id)
			}
			o := Observation{ID: id, Complete: true, Matches: 1, TargetID: "node-" + c.Step.Target, Actionable: true, Target: c.Step.Target}
			switch c.Step.Kind {
			case "extract":
				o.Value = Value{Type: "money", Integer: f.balance, Currency: "USD"}
			case "assert", "branch", "wait":
				o.Value = Value{Type: "string", Text: f.status}
			}
			ev = Event{Kind: "observed", ActionID: c.ActionID, Observation: o}
		case "act":
			justActed = true
			ev = Event{Kind: "receipt", ActionID: c.ActionID, Delivery: "sent"}
		default:
			t.Fatalf("unexpected command %q", c.Kind)
		}
		if c.Kind == "resolve" && stage[c.ObservationID] == "observe_after" {
			counts["resolved_post_action_capture"]++
		}
	}
	return s, counts, stage
}

const settled = 125000

// The capture right after the press still shows the previous member's balance.
// The balance the run reports must be the one on the settled screen.
func TestExtractNeverReadsAValueFromThePostActionCapture(t *testing.T) {
	s, counts, _ := simulate(t, 0, func(afterDispatch bool, fresh int) screen {
		if afterDispatch {
			return screen{"Member found", 99900}
		}
		return screen{"Member found", settled}
	})
	if got := s.Outputs["balance"].Integer; got != settled || s.Outcome != "success" {
		t.Fatalf("reported balance %d outcome %q, the settled screen shows %d (commands %v)", got, s.Outcome, settled, counts)
	}
	if counts["resolved_post_action_capture"] != 0 {
		t.Fatalf("a step resolved against a post-action capture: %v", counts)
	}
}

// A wait whose condition was already true before the action must not pass on
// the frame taken before the screen repainted.
func TestWaitAlreadyTrueBeforeTheActionDoesNotPassOnThePostActionCapture(t *testing.T) {
	s, counts, _ := simulate(t, 0, func(afterDispatch bool, fresh int) screen {
		switch {
		case afterDispatch:
			return screen{"Member found", 99900} // previous member's result
		case fresh <= 5:
			return screen{"Searching", 0} // the first fresh frame after the press
		default:
			return screen{"Member found", settled}
		}
	})
	if s.Outputs["balance"].Integer != settled || s.Outcome != "success" {
		t.Fatalf("wait passed on stale data: balance %d outcome %q (commands %v)", s.Outputs["balance"].Integer, s.Outcome, counts)
	}
	if counts["resolved_post_action_capture"] != 0 {
		t.Fatalf("a step resolved against a post-action capture: %v", counts)
	}
}

// A stale post-action frame must not steer a branch.
func TestBranchIsNotSteeredByThePostActionCapture(t *testing.T) {
	s, counts, _ := simulate(t, 0, func(afterDispatch bool, fresh int) screen {
		if afterDispatch {
			return screen{"Member found", 99900} // the previous member
		}
		return screen{"Member missing", 0} // this member does not exist
	})
	if s.Outcome != "not_found" || s.Phase != Concluded {
		t.Fatalf("branch was steered by a stale frame: phase %s outcome %q (commands %v)", s.Phase, s.Outcome, counts)
	}
}

// Reuse is a consistent snapshot read more than once: consecutive read steps
// share one fresh capture, and nothing else does.
func TestReadStepsShareOneFreshSnapshotButNothingElseReusesACapture(t *testing.T) {
	s, counts, stage := simulate(t, 0, func(bool, int) screen { return screen{"Member found", settled} })
	if s.Phase != Completed {
		t.Fatalf("%+v", s)
	}
	t.Logf("bank.balance commands per run: %v", counts)
	if counts["observe"]+counts["observe_after"] != 5 || counts["observe_after"] != 2 || counts["resolve"] != 2 {
		t.Fatalf("want 5 fresh captures (2 post-action) and 2 resolves, got %v", counts)
	}
	for id, kind := range stage {
		if kind == "observe_after" {
			t.Logf("post-action capture %s is never reused", id)
		}
	}
}

// Whatever ends a run while a resolve is pending, the finished or paused state
// carries no Reuse: Jarvis decodes traces strictly and rejects unknown fields.
func TestNoFinishedOrPausedStateCarriesReuse(t *testing.T) {
	p, inputs := compileBank(t)
	// A run held at "balance" with a resolve pending against the wait's capture.
	pending := func() State {
		s, _ := NewState(p, "run", "session", 1)
		s.StepIndex, s.Phase, s.Reuse = p.positions["balance"], Observing, "obs-9"
		return s
	}
	step := func(kind string, mutate func(*Event)) State {
		s := pending()
		e := Event{Sequence: 1, RunID: "run", SessionID: "session", Epoch: 1, Kind: kind, ActionID: s.ActionID(p), Reason: "x"}
		if mutate != nil {
			mutate(&e)
		}
		n, _, err := Reduce(p, s, e, inputs)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		return n
	}
	cases := map[string]State{
		"cancel":               step("cancel", nil),
		"error":                step("error", nil),
		"deadline":             step("observed", func(e *Event) { e.ElapsedMillis = 301_000 }),
		"pause":                step("pause", func(e *Event) { e.NextEpoch = 2 }),
		"external interaction": step("external_interaction", nil),
	}
	for name, n := range cases {
		if n.Reuse != "" {
			t.Errorf("%s: phase %s still carries reuse %q", name, n.Phase, n.Reuse)
		}
		if b, _ := json.Marshal(n); n.Phase != Observing && jsonHas(b, `"reuse"`) {
			t.Errorf("%s: encoded state carries the reuse field: %s", name, b)
		}
	}
}

func jsonHas(b []byte, sub string) bool { return len(b) >= len(sub) && contains(string(b), sub) }
func contains(a, b string) bool {
	for i := 0; i+len(b) <= len(a); i++ {
		if a[i:i+len(b)] == b {
			return true
		}
	}
	return false
}

// testdata/preuse_failed_observes.json was recorded with the reducer at the
// merge base of this branch (627c316): the same bank.balance run, with three
// transient capture failures at the wait, so it pauses, is resumed, and
// completes. It was generated by running that reducer (the unmodified
// manvi/workflow of 627c316, in a throwaway module that replaces the Manvi
// module with a checkout of it) through a scripted desktop and writing
// {Final, Events}. A recording made before observations were reused must
// replay to the same final state.
func TestPreChangeRecordingWithFailedObservesAndAResumeStillReplays(t *testing.T) {
	raw, err := os.ReadFile("testdata/preuse_failed_observes.json")
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Final  State
		Events []Event
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	p, inputs := compileBank(t)
	initial, _ := NewState(p, "run", "session", 1)
	got, err := Replay(p, initial, rec.Events, inputs)
	if err != nil {
		t.Fatalf("replay of a pre-change recording failed: %v", err)
	}
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(rec.Final)
	if string(a) != string(b) {
		t.Fatalf("replay diverged:\n got %s\nwant %s", a, b)
	}
}
