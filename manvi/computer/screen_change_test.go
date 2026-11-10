package computer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

func recoveryOptions(t *testing.T, d *brokerDesktop, mutate func(*RunOptions)) RunOptions {
	t.Helper()
	o := observationFixture(t)
	status, balance := "Member found", "$1,250.00"
	o.Nodes = []Node{
		{ID: "search", Role: "button", Name: "Search", Enabled: true, Actions: []string{"press"}},
		{ID: "status", Role: "text_field", Name: "Status", Value: &status, Enabled: true},
		{ID: "balance", Role: "text_field", Name: "Balance", Value: &balance, Enabled: true},
		{ID: "dismiss", Role: "button", Name: "Dismiss", Enabled: true, Actions: []string{"press"}},
	}
	d.testDesktop = &testDesktop{observation: o}
	p, err := workflow.Compile([]byte(recoveryBankJSON))
	if err != nil {
		t.Fatal(err)
	}
	opts := RunOptions{Program: p, Desktop: d, Session: Session{ID: "session", RunID: "run", Epoch: 1, Window: o.Window}}
	if mutate != nil {
		mutate(&opts)
	}
	return opts
}

// refuseFirstDismiss makes the recovery click fail as not sent (the window
// moved), once.
func refuseFirstDismiss() func(Action) error {
	var once sync.Once
	return func(a Action) error {
		var err error
		if a.TargetID == "dismiss" {
			once.Do(func() { err = &BrokerError{Code: "window_changed", Delivery: "not_sent", Message: "moved"} })
		}
		return err
	}
}

// A recovery click that is not sent keeps the step and retries with a window
// activation before each capture. The frame taken right after an activation has
// had no settle either: the extract that follows the wait must not be answered
// from it. The fake's first frame after the activation is stale.
func TestRunnerNeverReadsTheFrameTakenRightAfterAWindowActivation(t *testing.T) {
	d := &brokerDesktop{}
	opts := recoveryOptions(t, d, nil)
	d.actErr = refuseFirstDismiss()
	failed := 0
	d.script = func(nodes []Node, acts int, first bool) []Node {
		var out []Node
		sawFail := d.actFailed()
		if sawFail {
			failed++
		}
		for _, n := range nodes {
			switch {
			case !sawFail && acts == 1 && n.Name == "Status" && !first:
				continue // the overlay hides the status until the recovery is tried
			case sawFail && failed == 1 && n.Name == "Status":
				v := "Searching" // the wait is not met on the first frame after the activation
				n.Value = &v
			case sawFail && failed == 2 && n.Name == "Balance":
				v := "$999.00" // right after the activation: not repainted yet
				n.Value = &v
			}
			out = append(out, n)
		}
		return out
	}
	ctx := safetyContext(t)
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	d.mu2.Lock()
	focuses := d.focuses
	d.mu2.Unlock()
	t.Logf("%d window activations, %d frames after the refused click", focuses, failed)
	if result.State.Phase != workflow.Completed || result.State.Outputs["balance"].Integer != 125000 {
		t.Fatalf("balance %d phase %s, the settled screen shows 125000", result.State.Outputs["balance"].Integer, result.State.Phase)
	}
	if focuses < 2 || failed < 3 {
		t.Fatalf("the scenario must activate the window at least twice and take three frames after the refused click: focuses=%d frames=%d", focuses, failed)
	}
}

// injectingReducer wraps the real reducer and, after every complete fresh
// capture, makes the runner answer a resolve against it: the reducer fault the
// runner's own rule exists to survive. The injected resolve is answered, noted
// and swallowed, and the real transition then proceeds.
type injectingReducer struct {
	mu      sync.Mutex
	seen    map[string]bool
	pending *struct {
		state    workflow.State
		cmds     []workflow.Command
		captured string
		actionID string
	}
	answers map[string]string // capture id -> the event kind the runner answered with
}

func (x *injectingReducer) reduce(p *workflow.Program, s workflow.State, e workflow.Event, in map[string]workflow.Value) (workflow.State, []workflow.Command, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.pending != nil && e.ActionID == x.pending.actionID && (e.Kind == "reuse_declined" || e.Kind == "observed") {
		x.answers[x.pending.captured] = e.Kind
		n, cmds := x.pending.state, x.pending.cmds
		n.Sequence, n.ElapsedMillis = e.Sequence, e.ElapsedMillis
		x.pending = nil
		return n, cmds, nil
	}
	n, cmds, err := workflow.Reduce(p, s, e, in)
	if err != nil || e.Kind != "observed" || !e.Observation.Complete || len(cmds) == 0 || cmds[0].Kind == "resolve" || x.seen[e.Observation.ID] {
		return n, cmds, err
	}
	x.seen[e.Observation.ID] = true
	x.pending = &struct {
		state    workflow.State
		cmds     []workflow.Command
		captured string
		actionID string
	}{n, cmds, e.Observation.ID, e.ActionID}
	inject := workflow.Command{Kind: "resolve", ActionID: e.ActionID, ObservationID: e.Observation.ID, Epoch: n.Epoch,
		Step:     workflow.Step{ID: "injected", Kind: "extract", Target: "balance", Effect: "read", OutputType: "money", Currency: "USD"},
		Selector: workflow.Selector{Role: "text_field", Name: "Balance"}}
	return n, []workflow.Command{inject}, nil
}

// The runner's own rule, tested without the reducer's help: whatever the reducer
// asks, the runner answers reuse_declined for a frame that was the first taken
// after an act, a recovery click, a window activation, or after only failed
// captures, and does answer a frame that follows another since then. What each
// capture is, is read off the calls the desktop actually received.
func TestRunnerDeclinesWhatItsOwnRuleForbidsWhateverTheReducerAsks(t *testing.T) {
	hide := func(d *brokerDesktop) func([]Node, int, bool) []Node {
		return func(nodes []Node, acts int, first bool) []Node {
			var out []Node
			for _, n := range nodes {
				if acts == 1 && n.Name == "Status" && !d.actFailed() && !first {
					continue // the overlay hides the status, so a recovery fires
				}
				out = append(out, n)
			}
			return out
		}
	}
	scenarios := map[string]func(d *brokerDesktop){
		// The recovery click goes out; a capture fails once and is taken again.
		"recovery click sent, one failed capture": func(d *brokerDesktop) {
			d.script = hide(d)
			var calls int
			d.obsErr = func() error {
				calls++
				if calls == 2 {
					return &BrokerError{Code: "window_changed", Delivery: "not_sent", Message: "moved"}
				}
				return nil
			}
		},
		// The click is refused: every capture after it activates the window first,
		// and the wait is not met on the first of them, so two activated frames
		// follow each other with no act in between.
		"recovery click refused, consecutive activated frames": func(d *brokerDesktop) {
			d.actErr = refuseFirstDismiss()
			failed := 0
			d.script = func(nodes []Node, acts int, first bool) []Node {
				sawFail := d.actFailed()
				if sawFail {
					failed++
				}
				var out []Node
				for _, n := range nodes {
					switch {
					case !sawFail && acts == 1 && n.Name == "Status" && !first:
						continue
					case sawFail && failed <= 2 && n.Name == "Status":
						v := "Searching"
						n.Value = &v
					}
					out = append(out, n)
				}
				return out
			}
		},
	}
	for name, setup := range scenarios {
		t.Run(name, func(t *testing.T) { runnerDeclines(t, setup) })
	}
}

func runnerDeclines(t *testing.T, setup func(d *brokerDesktop)) {
	d := &brokerDesktop{}
	x := &injectingReducer{seen: map[string]bool{}, answers: map[string]string{}}
	opts := recoveryOptions(t, d, func(o *RunOptions) { o.reduce = x.reduce })
	setup(d)
	ctx := safetyContext(t)
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = run.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	// The oracle: replay what the desktop saw.
	d.mu2.Lock()
	log := append([]string(nil), d.log...)
	d.mu2.Unlock()
	kept, since := map[string]bool{}, 0
	for _, entry := range log {
		if entry == "change" {
			since = 0
			continue
		}
		since++
		kept[entry] = since >= 2
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	declined, answered := 0, 0
	for id, kind := range x.answers {
		want := "reuse_declined"
		if kept[id] {
			want = "observed"
			answered++
		} else {
			declined++
		}
		if kind != want {
			t.Errorf("capture %s: the runner answered %s, want %s (desktop log %v)", id, kind, want, log)
		}
	}
	t.Logf("%d injected resolves: %d declined, %d answered; desktop saw %v", len(x.answers), declined, answered, log)
	if declined < 3 {
		t.Fatalf("the scenario must exercise the decline path at least three times, got %d declined and %d answered", declined, answered)
	}
}

// When the error event itself cannot be journaled the final phase is set
// without Reduce. The state it leaves must still carry neither Reuse nor
// Captures: a strict decoder rejects unknown fields.
func TestFinishedStateAfterAJournalFailureCarriesNoReuseFields(t *testing.T) {
	o := observationFixture(t)
	p, err := workflow.Compile([]byte(bankBalanceJSON))
	if err != nil {
		t.Fatal(err)
	}
	status, balance, member := "Member found", "$1,250.00", ""
	o.Nodes = []Node{
		{ID: "member", Role: "text_field", Name: "Member ID", Value: &member, Enabled: true, Editable: true, Actions: []string{"set_value"}},
		{ID: "search", Role: "button", Name: "Search", Enabled: true, Actions: []string{"press"}},
		{ID: "status", Role: "text_field", Name: "Status", Value: &status, Enabled: true},
		{ID: "balance", Role: "text_field", Name: "Balance", Value: &balance, Enabled: true},
	}
	d := &brokerDesktop{testDesktop: &testDesktop{observation: o}}
	var mu sync.Mutex
	failing := false
	opts := RunOptions{Program: p, Desktop: d, Session: Session{ID: "session", RunID: "run", Epoch: 1, Window: o.Window},
		Inputs: map[string]workflow.Value{"member_id": {Type: "string", Text: "M-1001"}},
		OnRecord: func(r Record) error {
			mu.Lock()
			defer mu.Unlock()
			if r.Kind == "action_started" && r.Stage == "resolve" {
				failing = true // a resolve is now pending: Reuse and Captures are set
			}
			if failing {
				return errors.New("journal unavailable")
			}
			return nil
		}}
	ctx := safetyContext(t)
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := run.Wait(context.WithoutCancel(ctx))
	if result.State.Phase != workflow.Failed && result.State.Phase != workflow.Unknown {
		t.Fatalf("the run should end failed or unknown, got %s", result.State.Phase)
	}
	b, err := json.Marshal(result.State)
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Reuse != "" || result.State.Captures != 0 || bytes.Contains(b, []byte(`"reuse"`)) || bytes.Contains(b, []byte(`"captures"`)) {
		t.Fatalf("a finished state carries a reuse field: %s", b)
	}
}
