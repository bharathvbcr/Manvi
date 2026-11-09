package computer

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

const bankBalanceJSON = `{
  "schema_version":1,"id":"bank.balance","revision":"1","application":"jarvis-bank",
  "parameters":[{"name":"member_id","type":"string","sensitive":true}],
  "targets":{
    "member":{"strategies":[{"role":"text_field","name":"Member ID"}],"rationale":"member field","stability":"semantic"},
    "search":{"strategies":[{"role":"button","name":"Search"}],"rationale":"search","stability":"semantic"},
    "status":{"strategies":[{"role":"text_field","name":"Status"}],"rationale":"status","stability":"semantic"},
    "balance":{"strategies":[{"role":"text_field","name":"Balance"}],"rationale":"balance","stability":"semantic"}
  },
  "outputs":[{"name":"balance","type":"money","currency":"USD","description":"USD balance"}],
  "outcomes":[
    {"id":"success","kind":"success","description":"found","outputs":["balance"]},
    {"id":"not_found","kind":"business","description":"missing","outputs":[]}
  ],
  "steps":[
    {"id":"member","kind":"set_value","target":"member","effect":"read","input":{"source":"input","key":"member_id"}},
    {"id":"search","kind":"press","target":"search","effect":"read"},
    {"id":"settle","kind":"wait","target":"status","effect":"read","predicate":{"op":"contains","expected":{"source":"literal","literal":{"type":"string","text":"Member"}}}},
    {"id":"route","kind":"branch","target":"status","effect":"read","predicate":{"op":"equals","expected":{"source":"literal","literal":{"type":"string","text":"Member found"}}},"next":"balance","otherwise":"not_found"},
    {"id":"balance","kind":"extract","target":"balance","effect":"read","output":"balance","output_type":"money","currency":"USD","next":"ok"},
    {"id":"ok","kind":"conclude","outcome":"success"},
    {"id":"not_found","kind":"conclude","outcome":"not_found"}
  ],
  "limits":{"max_actions":40,"active_seconds":300,"observation_attempts":3,"intervention_seconds":600}
}`

// brokerDesktop behaves like the broker for the one rule reuse depends on: only
// the latest capture can be resolved against.
type brokerDesktop struct {
	*testDesktop
	mu2          sync.Mutex
	observes     int
	resolvesByID map[string]int
	current      string
	acted        bool
	afterAct     map[string]bool
	// rejectReuse fails every resolve of a capture that was already resolved once,
	// which is exactly what a second step resolving against it looks like.
	rejectReuse bool
	ambiguous   bool
}

func (d *brokerDesktop) Act(ctx context.Context, s Session, a Action) (Receipt, error) {
	d.mu2.Lock()
	d.acted = true
	d.mu2.Unlock()
	return d.testDesktop.Act(ctx, s, a)
}

func (d *brokerDesktop) Observe(ctx context.Context, s Session) (Observation, error) {
	o, err := d.testDesktop.Observe(ctx, s)
	d.mu2.Lock()
	defer d.mu2.Unlock()
	d.observes++
	d.current = o.ID
	if d.afterAct == nil {
		d.afterAct = map[string]bool{}
	}
	d.afterAct[o.ID], d.acted = d.acted, false
	return o, err
}

func (d *brokerDesktop) Resolve(ctx context.Context, s Session, id string, sel Selector) (Node, error) {
	d.mu2.Lock()
	if d.resolvesByID == nil {
		d.resolvesByID = map[string]int{}
	}
	d.resolvesByID[id]++
	n := d.resolvesByID[id]
	current, reused := d.current, n > 1 || d.afterAct[id]
	reject, ambiguous := d.rejectReuse, d.ambiguous
	d.mu2.Unlock()
	if id != current || (reject && reused) {
		return Node{}, &BrokerError{Code: "stale_observation", Delivery: "not_sent", Message: "Observe the window again"}
	}
	if ambiguous && reused {
		return Node{}, &BrokerError{Code: "target_ambiguous", Delivery: "not_sent", Message: "many"}
	}
	return d.testDesktop.Resolve(ctx, s, id, sel)
}

func bankRun(t *testing.T, mutate func(*brokerDesktop)) (RunResult, *brokerDesktop, []Record) {
	t.Helper()
	o := observationFixture(t)
	status, balance, member := "Member found", "$1,250.00", ""
	o.Nodes = []Node{
		{ID: "member", Role: "text_field", Name: "Member ID", Value: &member, Enabled: true, Editable: true, Actions: []string{"set_value"}},
		{ID: "search", Role: "button", Name: "Search", Enabled: true, Actions: []string{"press"}},
		{ID: "status", Role: "text_field", Name: "Status", Value: &status, Enabled: true},
		{ID: "balance", Role: "text_field", Name: "Balance", Value: &balance, Enabled: true},
	}
	p, err := workflow.Compile([]byte(bankBalanceJSON))
	if err != nil {
		t.Fatal(err)
	}
	d := &brokerDesktop{testDesktop: &testDesktop{observation: o}}
	if mutate != nil {
		mutate(d)
	}
	var mu sync.Mutex
	var records []Record
	opts := RunOptions{Program: p, Desktop: d, Session: Session{ID: "session", RunID: "run", Epoch: 1, Window: o.Window},
		Inputs: map[string]workflow.Value{"member_id": {Type: "string", Text: "M-1001"}},
		OnRecord: func(r Record) error {
			mu.Lock()
			records = append(records, r)
			mu.Unlock()
			return nil
		}}
	ctx := safetyContext(t)
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return result, d, records
}

func observedIDs(events []workflow.Event) (all, distinct int) {
	seen := map[string]bool{}
	for _, e := range events {
		if e.Kind == "observed" {
			all++
			seen[e.Observation.ID] = true
		}
	}
	return all, len(seen)
}

func TestBankBalanceObservesOncePerDispatchNotOncePerStep(t *testing.T) {
	result, d, records := bankRun(t, nil)
	if result.State.Phase != workflow.Completed || result.State.Outputs["balance"].Integer != 125000 {
		t.Fatalf("%+v", result.State)
	}
	// Before reuse this run captured 7 times (3 more than its two dispatches
	// need). The first step's capture and the two post-action captures remain.
	if d.observes != 3 {
		t.Fatalf("native captures = %d, want 3", d.observes)
	}
	if all, distinct := observedIDs(result.Events); all <= distinct {
		t.Fatalf("expected a reused observation id in the event stream, got %d events over %d ids", all, distinct)
	}
	// Every read step still journals an observation of its own, so evidence that
	// pairs each admitted read action with a post-observation stays complete.
	for _, step := range []string{"settle", "route", "balance"} {
		n := 0
		for _, r := range records {
			if r.Kind == "observation" && r.ActionID == "run:"+step {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("step %s journaled %d observation records, want 1", step, n)
		}
	}
	// Offline replay of the journaled events reaches the same terminal state.
	p, _ := workflow.Compile([]byte(bankBalanceJSON))
	replayed, err := workflow.Replay(p, result.Initial, result.Events, map[string]workflow.Value{"member_id": {Type: "string", Text: "M-1001"}})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(replayed)
	b, _ := json.Marshal(result.State)
	if string(a) != string(b) {
		t.Fatalf("replay disagrees:\n%s\n%s", a, b)
	}
}

func TestRefusedResolveFallsBackToFreshObservationsAndStillCompletes(t *testing.T) {
	for name, mutate := range map[string]func(*brokerDesktop){
		"broker refuses the reused capture": func(d *brokerDesktop) { d.rejectReuse = true },
		"reused capture is ambiguous":       func(d *brokerDesktop) { d.ambiguous = true },
	} {
		t.Run(name, func(t *testing.T) {
			result, d, _ := bankRun(t, mutate)
			if result.State.Phase != workflow.Completed || result.State.Outputs["balance"].Integer != 125000 {
				t.Fatalf("%+v", result.State)
			}
			if d.observes != 7 {
				t.Fatalf("captures = %d, want today's 7 when every reuse is declined", d.observes)
			}
			if result.State.Attempts != 0 {
				t.Fatalf("a declined reuse used an observation attempt: %d", result.State.Attempts)
			}
		})
	}
}

func TestPerformResolveDeclinesWhatItCannotVouchFor(t *testing.T) {
	d := &brokerDesktop{testDesktop: &testDesktop{}}
	safe := observationFixture(t)
	safe.ID, safe.Epoch = "cap-1", 1
	d.current = "cap-1"
	fresh := func() *retainedObservation {
		return &retainedObservation{safe: safe, at: time.Now()}
	}
	cmd := workflow.Command{Kind: "resolve", ActionID: "run:read", ObservationID: "cap-1", Step: workflow.Step{ID: "read", Kind: "extract", Target: "t", Effect: "read"}, Selector: workflow.Selector{Role: "text_field", Name: "Member ID"}}
	session := Session{ID: "session", RunID: "run", Epoch: 1}
	opts := RunOptions{Desktop: d}
	cases := map[string]func() (*retainedObservation, workflow.Command, Session){
		"nothing retained": func() (*retainedObservation, workflow.Command, Session) { return nil, cmd, session },
		"another capture": func() (*retainedObservation, workflow.Command, Session) {
			c := cmd
			c.ObservationID = "cap-0"
			return fresh(), c, session
		},
		"too old": func() (*retainedObservation, workflow.Command, Session) {
			k := fresh()
			k.at = time.Now().Add(-reuseMaxAge - time.Second)
			return k, cmd, session
		},
		"another epoch": func() (*retainedObservation, workflow.Command, Session) {
			s := session
			s.Epoch = 2
			return fresh(), cmd, s
		},
		"broker no longer holds it": func() (*retainedObservation, workflow.Command, Session) {
			d.mu2.Lock()
			d.current = "cap-2"
			d.mu2.Unlock()
			return fresh(), cmd, session
		},
		"visual locator": func() (*retainedObservation, workflow.Command, Session) {
			c := cmd
			c.Selector = workflow.Selector{Strategies: []workflow.Selector{{Visual: &workflow.VisualAnchor{}}}}
			return fresh(), c, session
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			kept, c, s := build()
			r := performResolve(context.Background(), opts, s, nil, c, kept)
			if r.err != nil || r.event.Kind != "reuse_declined" || r.observation != nil {
				t.Fatalf("want a clean decline, got %+v", r)
			}
		})
	}
	t.Run("cancelled context is an error, not a decline", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r := performResolve(ctx, opts, session, nil, cmd, nil)
		if r.err == nil || r.event.Kind != "" {
			t.Fatalf("%+v", r)
		}
	})
}
