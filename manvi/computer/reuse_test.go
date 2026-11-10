package computer

import (
	"context"
	"encoding/json"
	"strings"
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
	// staleAfterAct makes the capture taken right after a dispatch show the
	// previous member's balance; every later capture shows the settled one.
	staleAfterAct bool
	// script, when set, decides what the window shows at each capture: the nodes,
	// how many dispatches came before it, and whether it is the first capture
	// after one.
	script    func(nodes []Node, acts int, first bool) []Node
	acts      int
	lastNodes []Node
	focused   bool
	focuses   int
	// actErr, when set, may refuse an act as not sent.
	actErr func(a Action) error
	// obsErr, when set, may fail a capture before it produces a frame.
	obsErr    func() error
	failedAct bool
	// log is what the desktop saw, in order: "change" for every act and window
	// activation, the capture id for every frame it handed out.
	log []string
}

// actFailed reports whether an act has been refused as not sent.
func (d *brokerDesktop) actFailed() bool { return d.failedAct }

func (d *brokerDesktop) Act(ctx context.Context, s Session, a Action) (Receipt, error) {
	d.mu2.Lock()
	d.acted = true
	d.acts++
	d.current = "" // a dispatch outdates the broker's current observation
	d.log = append(d.log, "change")
	actErr := d.actErr
	d.mu2.Unlock()
	if actErr != nil {
		if err := actErr(a); err != nil {
			d.mu2.Lock()
			d.failedAct = true
			d.mu2.Unlock()
			return Receipt{ActionID: a.ActionID, Delivery: "not_sent"}, err
		}
	}
	return d.testDesktop.Act(ctx, s, a)
}

// Focus activates the window: like an act it can change what is on screen, and
// it outdates the broker's current observation.
func (d *brokerDesktop) Focus(_ context.Context, s Session) (Session, error) {
	d.mu2.Lock()
	defer d.mu2.Unlock()
	d.focused = true
	d.focuses++
	d.current = ""
	d.log = append(d.log, "change")
	return s, nil
}

func (d *brokerDesktop) Observe(ctx context.Context, s Session) (Observation, error) {
	if d.obsErr != nil {
		if err := d.obsErr(); err != nil {
			return Observation{}, err
		}
	}
	o, err := d.testDesktop.Observe(ctx, s)
	d.mu2.Lock()
	defer d.mu2.Unlock()
	d.observes++
	d.current = o.ID
	d.log = append(d.log, o.ID)
	if d.afterAct == nil {
		d.afterAct = map[string]bool{}
	}
	d.afterAct[o.ID], d.acted, d.focused = d.acted || d.focused, false, false
	if d.script != nil {
		o.Nodes = d.script(o.Nodes, d.acts, d.afterAct[o.ID])
	}
	d.lastNodes = append([]Node(nil), o.Nodes...)
	if d.staleAfterAct && d.afterAct[o.ID] {
		// The app has not repainted yet: the previous member's balance.
		for i := range o.Nodes {
			if o.Nodes[i].Name == "Balance" {
				v := "$999.00"
				o.Nodes[i].Value = &v
			}
		}
	}
	return o, err
}

func (d *brokerDesktop) Resolve(ctx context.Context, s Session, id string, sel Selector) (Node, error) {
	d.mu2.Lock()
	if d.resolvesByID == nil {
		d.resolvesByID = map[string]int{}
	}
	d.resolvesByID[id]++
	n := d.resolvesByID[id]
	current, reused := d.current, n > 1
	reject, ambiguous := d.rejectReuse, d.ambiguous
	d.mu2.Unlock()
	if id != current || (reject && reused) {
		return Node{}, &BrokerError{Code: "stale_observation", Delivery: "not_sent", Message: "Observe the window again"}
	}
	if ambiguous && reused {
		return Node{}, &BrokerError{Code: "target_ambiguous", Delivery: "not_sent", Message: "many"}
	}
	d.mu2.Lock()
	nodes := d.lastNodes
	d.mu2.Unlock()
	for _, n := range nodes {
		if n.Name == sel.Name {
			return n, nil
		}
	}
	return Node{}, &BrokerError{Code: "target_missing", Delivery: "not_sent"}
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
	// The frame taken right after each press is stale; only a settled one may be read.
	result, d, records := bankRun(t, func(d *brokerDesktop) { d.staleAfterAct = true })
	if result.State.Phase != workflow.Completed || result.State.Outputs["balance"].Integer != 125000 {
		t.Fatalf("%+v", result.State)
	}
	// Before reuse this run captured 7 times. Two post-action captures, the first
	// two steps and the wait are fresh; the branch and
	// the extract read the wait's snapshot.
	if d.observes != 5 {
		t.Fatalf("native captures = %d, want 5", d.observes)
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
	t.Run("a timeout of the step's own context falls back to a fresh observe", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		r := performResolve(ctx, opts, session, nil, cmd, fresh())
		if r.err != nil || r.event.Kind != "reuse_declined" {
			t.Fatalf("want a decline, got %+v", r)
		}
	})
}

// A kept capture is dated with a monotonic reading, so a wall-clock step cannot
// stretch or shrink its age.
func TestKeptCaptureAgeIsMonotonic(t *testing.T) {
	o := observationFixture(t)
	d := &brokerDesktop{testDesktop: &testDesktop{observation: o}}
	opts := RunOptions{Desktop: d, Privacy: PrivacyPolicy{PID: o.Window.PID, WindowID: o.Window.ID}}
	cmd := workflow.Command{Kind: "observe", ActionID: "run:read", Step: workflow.Step{ID: "read", Kind: "extract", Target: "t", Effect: "read", OutputType: "string"}, Selector: workflow.Selector{Name: "Member ID"}}
	r := perform(context.Background(), opts, Session{ID: "s", RunID: "run", Epoch: 1, Window: o.Window}, nil, cmd, [32]byte{}, nil)
	if r.err != nil || r.retain == nil {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.retain.at.String(), "m=") {
		t.Fatalf("capture time %q carries no monotonic reading", r.retain.at)
	}
}

const recoveryBankJSON = `{
  "schema_version":1,"id":"bank.recover","revision":"1","application":"jarvis-bank",
  "targets":{
    "search":{"strategies":[{"role":"button","name":"Search"}],"rationale":"search","stability":"semantic"},
    "status":{"strategies":[{"role":"text_field","name":"Status"}],"rationale":"status","stability":"semantic"},
    "balance":{"strategies":[{"role":"text_field","name":"Balance"}],"rationale":"balance","stability":"semantic"},
    "dismiss":{"strategies":[{"role":"button","name":"Dismiss"}],"rationale":"dismiss","stability":"semantic"}
  },
  "outputs":[{"name":"balance","type":"money","currency":"USD","description":"USD balance"}],
  "outcomes":[{"id":"success","kind":"success","description":"found","outputs":["balance"]}],
  "recoveries":[{"id":"dismiss_overlay","when":{"target":"status","predicate":{"op":"exists","expected":{"source":"","key":"","literal":{"type":""}}}},"action":{"kind":"press","target":"dismiss"},"max":1}],
  "steps":[
    {"id":"search","kind":"press","target":"search","effect":"read"},
    {"id":"settle","kind":"wait","target":"status","effect":"read","predicate":{"op":"contains","expected":{"source":"literal","literal":{"type":"string","text":"Member"}}}},
    {"id":"balance","kind":"extract","target":"balance","effect":"read","output":"balance","output_type":"money","currency":"USD","next":"ok"},
    {"id":"ok","kind":"conclude","outcome":"success"}
  ],
  "limits":{"max_actions":40,"active_seconds":300,"observation_attempts":3,"intervention_seconds":600}
}`

// press -> wait (an overlay hides the status, so a recovery clicks dismiss) ->
// extract. The first frame after the click still shows the previous member's
// balance; the extract must be answered from a later one.
func TestRunnerNeverReadsTheFrameTakenRightAfterARecoveryClick(t *testing.T) {
	o := observationFixture(t)
	status, balance := "Member found", "$1,250.00"
	o.Nodes = []Node{
		{ID: "search", Role: "button", Name: "Search", Enabled: true, Actions: []string{"press"}},
		{ID: "status", Role: "text_field", Name: "Status", Value: &status, Enabled: true},
		{ID: "balance", Role: "text_field", Name: "Balance", Value: &balance, Enabled: true},
		{ID: "dismiss", Role: "button", Name: "Dismiss", Enabled: true, Actions: []string{"press"}},
	}
	p, err := workflow.Compile([]byte(recoveryBankJSON))
	if err != nil {
		t.Fatal(err)
	}
	d := &brokerDesktop{testDesktop: &testDesktop{observation: o}}
	d.script = func(nodes []Node, acts int, first bool) []Node {
		var out []Node
		for _, n := range nodes {
			switch {
			case acts == 1 && !first && n.Name == "Status":
				continue // the overlay still hides the status field
			case acts == 2 && first && n.Name == "Balance":
				v := "$999.00" // not repainted since the click
				n.Value = &v
			}
			out = append(out, n)
		}
		return out
	}
	opts := RunOptions{Program: p, Desktop: d, Session: Session{ID: "session", RunID: "run", Epoch: 1, Window: o.Window}}
	ctx := safetyContext(t)
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Phase != workflow.Completed || result.State.Outputs["balance"].Integer != 125000 {
		t.Fatalf("balance %d phase %s, the settled screen shows 125000", result.State.Outputs["balance"].Integer, result.State.Phase)
	}
	d.mu2.Lock()
	acts, observes, resolves := d.acts, d.observes, 0
	for _, n := range d.resolvesByID {
		resolves += n
	}
	d.mu2.Unlock()
	t.Logf("recovery capability through the runner: %d dispatches, %d native observes, %d resolve calls", acts, observes, resolves)
	if acts != 2 {
		t.Fatalf("want the press and the recovery click, got %d dispatches", acts)
	}
}
