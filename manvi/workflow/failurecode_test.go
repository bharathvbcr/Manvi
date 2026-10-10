package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func codeOf(t *testing.T, p *Program, s State) FailureCode {
	t.Helper()
	r := ResultFromState(p, s)
	if r.Failure == nil {
		t.Fatalf("phase %s reported no failure: %+v", s.Phase, r)
	}
	return r.Failure.Code
}

func TestResultFailureCodeMatchesCause(t *testing.T) {
	p := fixture(t, false)

	t.Run("deadline", func(t *testing.T) {
		s := start(t, p)
		e := inputEvent(s, "observed")
		e.ElapsedMillis = int64(p.Capability().Limits.ActiveSeconds)*1000 + 1
		s, _, err := Reduce(p, s, e, nil)
		if err != nil || s.Phase != Failed {
			t.Fatalf("%+v %v", s, err)
		}
		if got := codeOf(t, p, s); got != FailureDeadline {
			t.Fatalf("code = %q, want %q", got, FailureDeadline)
		}
	})

	t.Run("action limit", func(t *testing.T) {
		s := start(t, p)
		s.Actions = p.Capability().Limits.MaxActions
		s, _, err := Reduce(p, s, observed(s), nil) // press is a read step: acts
		if err != nil {
			t.Fatal(err)
		}
		r := inputEvent(s, "receipt")
		r.ActionID = s.ActionID(p)
		r.Delivery = "sent"
		s, _, err = Reduce(p, s, r, nil)
		if err != nil {
			t.Fatal(err)
		}
		o := observed(s)
		o.ActionID = s.ActionID(p)
		s, _, err = Reduce(p, s, o, nil)
		if err != nil || s.Phase != Failed {
			t.Fatalf("%+v %v", s, err)
		}
		if got := codeOf(t, p, s); got != FailureActionLimit {
			t.Fatalf("code = %q, want %q", got, FailureActionLimit)
		}
	})

	t.Run("external interaction pause", func(t *testing.T) {
		s := start(t, p)
		e := inputEvent(s, "external_interaction")
		e.Reason = "system dialog"
		s, _, err := Reduce(p, s, e, nil)
		if err != nil || s.Phase != Paused {
			t.Fatalf("%+v %v", s, err)
		}
		if got := codeOf(t, p, s); got != FailureExternalInteraction {
			t.Fatalf("code = %q, want %q", got, FailureExternalInteraction)
		}
		// The runner re-applies the pause with the fenced epoch; the cause survives it.
		f := inputEvent(s, "pause")
		f.NextEpoch = s.Epoch + 1
		f.Reason = s.Reason
		f.FailureCode = string(s.Cause)
		s, _, err = Reduce(p, s, f, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := codeOf(t, p, s); got != FailureExternalInteraction {
			t.Fatalf("code after fence = %q, want %q", got, FailureExternalInteraction)
		}
	})

	t.Run("human takeover", func(t *testing.T) {
		s := start(t, p)
		e := inputEvent(s, "pause")
		e.NextEpoch = s.Epoch + 1
		e.Reason = "human takeover"
		e.FailureCode = string(FailureHumanTakeover)
		s, _, err := Reduce(p, s, e, nil)
		if err != nil || s.Phase != Paused {
			t.Fatalf("%+v %v", s, err)
		}
		if got := codeOf(t, p, s); got != FailureHumanTakeover {
			t.Fatalf("code = %q, want %q", got, FailureHumanTakeover)
		}
	})

	t.Run("a code outside the taxonomy is not recorded", func(t *testing.T) {
		s := start(t, p)
		e := inputEvent(s, "pause")
		e.NextEpoch = s.Epoch + 1
		e.FailureCode = "made_up"
		s, _, err := Reduce(p, s, e, nil)
		if err != nil {
			t.Fatal(err)
		}
		if s.Cause != "" {
			t.Fatalf("cause = %q", s.Cause)
		}
	})

	t.Run("not actionable pause", func(t *testing.T) {
		s := start(t, p)
		e := observed(s)
		e.Observation.Actionable = false
		s, _, err := Reduce(p, s, e, nil)
		if err != nil || s.Phase != Paused {
			t.Fatalf("%+v %v", s, err)
		}
		if got := codeOf(t, p, s); got != FailureNotActionable {
			t.Fatalf("code = %q, want %q", got, FailureNotActionable)
		}
	})

	t.Run("human denial", func(t *testing.T) {
		cp := fixture(t, true)
		s := start(t, cp)
		s, _, err := Reduce(cp, s, observed(s), nil)
		if err != nil || s.Phase != AwaitingApproval {
			t.Fatalf("%+v %v", s, err)
		}
		d := inputEvent(s, "deny")
		d.ActionID = s.ActionID(cp)
		s, _, err = Reduce(cp, s, d, nil)
		if err != nil || s.Phase != Cancelled {
			t.Fatalf("%+v %v", s, err)
		}
		r := ResultFromState(cp, s)
		if r.Kind != ResultCancelled || r.Failure == nil || r.Failure.Code != FailureHumanDenied {
			t.Fatalf("result = %+v", r)
		}
	})

	t.Run("a plain cancel is not a denial", func(t *testing.T) {
		s := start(t, p)
		s, _, err := Reduce(p, s, inputEvent(s, "cancel"), nil)
		if err != nil || s.Phase != Cancelled {
			t.Fatalf("%+v %v", s, err)
		}
		if r := ResultFromState(p, s); r.Failure != nil {
			t.Fatalf("cancel reported a failure: %+v", r.Failure)
		}
	})

	t.Run("cause does not outlive a resume", func(t *testing.T) {
		s := start(t, p)
		e := inputEvent(s, "external_interaction")
		s, _, err := Reduce(p, s, e, nil)
		if err != nil {
			t.Fatal(err)
		}
		f := inputEvent(s, "resume")
		f.NextEpoch = s.Epoch + 1
		s, _, err = Reduce(p, s, f, nil)
		if err != nil {
			t.Fatal(err)
		}
		if s.Cause != "" || s.Reason != "" {
			t.Fatalf("stale cause %q / reason %q after resume", s.Cause, s.Reason)
		}
	})

	t.Run("a state recorded before causes existed keeps the phase default", func(t *testing.T) {
		if got := codeOf(t, p, State{Phase: Failed, Reason: "x"}); got != FailureCheckpoint {
			t.Fatalf("failed = %q", got)
		}
		if got := codeOf(t, p, State{Phase: Paused, Reason: "x"}); got != FailureAmbiguousTarget {
			t.Fatalf("paused = %q", got)
		}
		raw, err := json.Marshal(State{Phase: Paused})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"cause"`) {
			t.Fatalf("an empty cause must be omitted: %s", raw)
		}
	})
}
