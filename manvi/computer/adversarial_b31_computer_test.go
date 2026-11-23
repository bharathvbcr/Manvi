package computer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"image"
	"image/color"
	"image/png"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

// Adversarial pass on b31-computer: node-identity masking, 16-bit parity and
// the approval capture. Tests named TestAdvB31Computer... PASS when the change
// holds. A failing one is a break; the comment above it names the root cause.

const advSecret = "4111111111111111"

// advDesktop adds hooks to the shared test desktop.
type advDesktop struct {
	*testDesktop
	hookMu      sync.Mutex
	observes    int
	focused     bool
	grants      int
	observeHook func(n int, focused bool, o *Observation) error
	afterAct    func(o *Observation)
}

func (d *advDesktop) Observe(ctx context.Context, s Session) (Observation, error) {
	o, err := d.testDesktop.Observe(ctx, s)
	if err != nil {
		return o, err
	}
	d.hookMu.Lock()
	d.observes++
	n, focused, hook := d.observes, d.focused, d.observeHook
	d.hookMu.Unlock()
	if hook != nil {
		if err := hook(n, focused, &o); err != nil {
			return Observation{}, err
		}
	}
	return o, nil
}
func (d *advDesktop) Focus(_ context.Context, s Session) (Session, error) {
	d.hookMu.Lock()
	d.focused = true
	d.hookMu.Unlock()
	return s, nil
}
func (d *advDesktop) Approve(_ context.Context, _ Session, _ Action) (string, error) {
	d.hookMu.Lock()
	d.grants++
	d.hookMu.Unlock()
	return "grant", nil
}
func (d *advDesktop) Act(ctx context.Context, s Session, a Action) (Receipt, error) {
	r, err := d.testDesktop.Act(ctx, s, a)
	d.mu.Lock()
	if d.afterAct != nil {
		d.afterAct(&d.observation)
	}
	d.mu.Unlock()
	return r, err
}
func (d *advDesktop) actCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.acts
}

func advProgram(t *testing.T, strategies string) *workflow.Program {
	t.Helper()
	spec := `{
  "schema_version":1,"id":"pay.card","revision":"1","application":"pay",
  "parameters":[{"name":"card","type":"string","sensitive":true}],
  "targets":{
    "card":{"strategies":[` + strategies + `],"rationale":"card field","stability":"semantic"},
    "status":{"role":"text_field","name":"Status"}
  },
  "outputs":[{"name":"status","type":"string","description":"status"}],
  "outcomes":[{"id":"done","kind":"success","description":"entered","outputs":["status"]}],
  "steps":[
    {"id":"card","kind":"set_value","target":"card","effect":"read","input":{"source":"input","key":"card"}},
    {"id":"read","kind":"extract","target":"status","effect":"read","output":"status","output_type":"string"},
    {"id":"done","kind":"conclude","outcome":"done"}
  ],
  "limits":{"max_actions":40,"active_seconds":300,"observation_attempts":3,"intervention_seconds":600}
}`
	p, err := workflow.Compile([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const advDefaultRungs = `{"role":"text_field","name":"Card"},{"role":"text_field","identifier":"card-number"}`

func advStr(s string) *string { return &s }

func advCardNode(value string) Node {
	return Node{ID: "card", Role: "text_field", Name: "Card", Identifier: advStr("card-number"), Value: advStr(value), Enabled: true, Editable: true, Actions: []string{"set_value"}, Bounds: &Bounds{10, 10, 40, 20}}
}
func advStatusNode() Node {
	return Node{ID: "status", Role: "text_field", Name: "Status", Value: advStr("ready"), Enabled: true, Bounds: &Bounds{10, 60, 40, 10}}
}

// advFold keeps only digits, folding full-width digits, so a secret reformatted
// with any separator is still recognisable to the checker.
func advFold(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 0xFF10 && r <= 0xFF19:
			b.WriteRune('0' + (r - 0xFF10))
		}
	}
	return b.String()
}

// advLeaks lists where a recorded observation still carries the digits, either
// in one field or spread over the visible text of several nodes in order.
func advLeaks(records []Record, digits string) []string {
	var out []string
	for _, r := range records {
		o := r.Observation
		if o == nil {
			continue
		}
		var all strings.Builder
		field := func(where, s string) {
			if strings.Contains(advFold(s), digits) {
				out = append(out, r.Stage+"/"+where+": "+s)
			}
			all.WriteString(advFold(s))
		}
		field("window.title", o.Window.Title)
		for _, n := range o.Nodes {
			field("node "+n.ID+" name", n.Name)
			if n.Value != nil {
				field("node "+n.ID+" value", *n.Value)
			}
			if n.Identifier != nil {
				field("node "+n.ID+" identifier", *n.Identifier)
			}
		}
		if strings.Contains(all.String(), digits) && len(out) == 0 {
			out = append(out, r.Stage+"/spread across nodes: "+all.String())
		}
	}
	return out
}

func advRun(t *testing.T, prog *workflow.Program, d Desktop, o Observation, privacy PrivacyPolicy) ([]Record, RunResult, error) {
	t.Helper()
	var mu sync.Mutex
	var records []Record
	opts := RunOptions{
		Program: prog, Desktop: d, Privacy: privacy,
		Inputs:  map[string]workflow.Value{"card": {Type: "string", Text: advSecret}},
		Session: Session{ID: "session", RunID: "run", Epoch: 1, Window: o.Window},
		OnRecord: func(r Record) error {
			mu.Lock()
			records = append(records, r)
			mu.Unlock()
			return nil
		},
	}
	ctx := safetyContext(t)
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(ctx)
	mu.Lock()
	defer mu.Unlock()
	return append([]Record(nil), records...), result, err
}

var advVariants = map[string]string{
	"spaces":     "4111 1111 1111 1111",
	"dashes":     "4111-1111-1111-1111",
	"dots":       "4111.1111.1111.1111",
	"nbsp":       "4111\u00a01111\u00a01111\u00a01111",
	"zwj":        "4\u200d1\u200d1\u200d1\u200d1\u200d1\u200d1\u200d1\u200d1\u200d1\u200d1\u200d1\u200d1\u200d1\u200d1\u200d1",
	"fullwidth":  "\uff14\uff11\uff11\uff11\uff11\uff11\uff11\uff11\uff11\uff11\uff11\uff11\uff11\uff11\uff11\uff11",
	"fw-spaced":  "\uff14\uff11\uff11\uff11 \uff11\uff11\uff11\uff11 \uff11\uff11\uff11\uff11 \uff11\uff11\uff11\uff11",
	"bullet-ish": "4111\u20111111\u20111111\u20111111",
}

// echoRun types the secret into the card field and has the application echo
// into the place named. It returns what leaked into the records.
func echoRun(t *testing.T, place, echo string) []string {
	t.Helper()
	o := observationFixture(t)
	card := advCardNode("")
	nodes := []Node{card, advStatusNode()}
	switch place {
	case "target":
		nodes[0] = advCardNode(echo)
	case "sibling label":
		nodes = append(nodes, Node{ID: "preview", Role: "text", Name: "Preview", Value: advStr(echo), Enabled: true, Bounds: &Bounds{10, 40, 40, 10}})
	case "tooltip":
		nodes = append(nodes, Node{ID: "tip", Role: "tooltip", Name: "Card " + echo, Enabled: true, Bounds: &Bounds{60, 10, 30, 10}})
	case "list row":
		nodes = append(nodes, Node{ID: "row", Role: "row", Name: "Recent: " + echo, Value: advStr(""), Enabled: true, Bounds: &Bounds{60, 40, 30, 10}})
	case "window title":
		o.Window.Title = "Pay - " + echo
	default:
		t.Fatal("unknown place " + place)
	}
	o.Nodes = nodes
	d := &advDesktop{testDesktop: &testDesktop{observation: o}}
	records, result, err := advRun(t, advProgram(t, advDefaultRungs), d, o, PrivacyPolicy{})
	if err != nil || result.State.Phase != workflow.Completed {
		t.Fatalf("run did not complete: phase=%s err=%v", result.State.Phase, err)
	}
	if len(records) == 0 {
		t.Fatal("no records")
	}
	return advLeaks(records, advSecret)
}

func TestAdvB31ComputerEchoInTargetNodeIsMaskedForEverySeparator(t *testing.T) {
	for name, echo := range advVariants {
		t.Run(name, func(t *testing.T) {
			if leaks := echoRun(t, "target", echo); len(leaks) != 0 {
				t.Fatalf("secret reached a record: %q", leaks)
			}
		})
	}
}

func TestAdvB31ComputerLiteralEchoElsewhereIsScrubbed(t *testing.T) {
	for _, place := range []string{"sibling label", "tooltip", "list row", "window title"} {
		t.Run(place, func(t *testing.T) {
			if leaks := echoRun(t, place, advSecret); len(leaks) != 0 {
				t.Fatalf("literal secret reached a record: %q", leaks)
			}
		})
	}
}

// BREAKS: only the written node is masked by identity. Every other node is
// protected by the literal string match alone (Scrub / sensitive()), so a
// reformatted echo in a sibling, tooltip, list row or the window title passes.
func TestAdvB31ComputerReformattedEchoElsewhereIsMasked(t *testing.T) {
	for _, place := range []string{"sibling label", "tooltip", "list row", "window title"} {
		for name, echo := range advVariants {
			t.Run(place+"/"+name, func(t *testing.T) {
				if leaks := echoRun(t, place, echo); len(leaks) != 0 {
					t.Fatalf("reformatted secret reached a record: %q", leaks)
				}
			})
		}
	}
}

// BREAKS (same root cause): the secret split over two nodes never appears whole
// in any one field.
func TestAdvB31ComputerSecretSplitAcrossTwoNodesIsMasked(t *testing.T) {
	o := observationFixture(t)
	o.Nodes = []Node{advCardNode(""), advStatusNode(),
		{ID: "a", Role: "text", Name: "Left half", Value: advStr("4111 1111"), Enabled: true, Bounds: &Bounds{10, 40, 20, 10}},
		{ID: "b", Role: "text", Name: "Right half", Value: advStr("1111 1111"), Enabled: true, Bounds: &Bounds{40, 40, 20, 10}}}
	d := &advDesktop{testDesktop: &testDesktop{observation: o}}
	records, result, err := advRun(t, advProgram(t, advDefaultRungs), d, o, PrivacyPolicy{})
	if err != nil || result.State.Phase != workflow.Completed {
		t.Fatalf("%s %v", result.State.Phase, err)
	}
	if leaks := advLeaks(records, advSecret); len(leaks) != 0 {
		t.Fatalf("split secret reachable by concatenating nodes: %q", leaks)
	}
}

// DECISION: a last-4 echo in another node is not masked and should not be: it
// is the conventional display form, it is not the secret, and masking every
// node that merely shares four digits would blank unrelated numbers. The node
// the secret was typed into is masked whatever it echoes (first subtest).
func TestAdvB31ComputerPartialEchoPolicy(t *testing.T) {
	const secret = advSecret
	o := observationFixture(t)
	o.Nodes = []Node{advCardNode("ending 1111"), advStatusNode(),
		{ID: "last4", Role: "text", Name: "Saved card", Value: advStr("card ending 1111"), Enabled: true, Bounds: &Bounds{10, 40, 40, 10}}}
	d := &advDesktop{testDesktop: &testDesktop{observation: o}}
	records, result, err := advRun(t, advProgram(t, advDefaultRungs), d, o, PrivacyPolicy{})
	if err != nil || result.State.Phase != workflow.Completed {
		t.Fatalf("%s %v", result.State.Phase, err)
	}
	_ = secret
	sawTarget, sawLast4 := false, false
	for _, r := range records {
		if r.Observation == nil {
			continue
		}
		for _, n := range r.Observation.Nodes {
			if n.ID == "card" && n.Value != nil {
				sawTarget = true
				if *n.Value != "[redacted]" {
					t.Fatalf("target node partial echo not masked: %q", *n.Value)
				}
			}
			if n.ID == "last4" && n.Value != nil && strings.Contains(*n.Value, "1111") {
				sawLast4 = true
			}
		}
	}
	if !sawTarget || !sawLast4 {
		t.Fatalf("expected the target masked and the last-4 label visible (target=%v last4=%v)", sawTarget, sawLast4)
	}
}

// A new node id between steps must not matter: selectors never use the id.
func TestAdvB31ComputerTargetRecreatedWithNewNodeIDStaysMasked(t *testing.T) {
	o := observationFixture(t)
	o.Nodes = []Node{advCardNode(""), advStatusNode()}
	d := &advDesktop{testDesktop: &testDesktop{observation: o}}
	d.afterAct = func(o *Observation) {
		o.Nodes[0] = advCardNode(advVariants["spaces"])
		o.Nodes[0].ID = "card-recreated-9"
	}
	records, result, err := advRun(t, advProgram(t, advDefaultRungs), d, o, PrivacyPolicy{})
	if err != nil || result.State.Phase != workflow.Completed {
		t.Fatalf("%s %v", result.State.Phase, err)
	}
	if leaks := advLeaks(records, advSecret); len(leaks) != 0 {
		t.Fatalf("%q", leaks)
	}
}

// BREAKS: a rung is a conjunction of role, name and identifier, so when the
// application re-creates the control with a regenerated identifier (or renames
// it, as many apps do once a card is recognised) the node no longer matches
// the rung and the reformatted echo is unmasked. Single combined rung used
// here; the two separate rungs of the default capability would still catch it
// by name, which is why this case uses one rung.
func TestAdvB31ComputerTargetRecreatedWithRegeneratedIdentifierOrNameStaysMasked(t *testing.T) {
	rung := `{"role":"text_field","name":"Card","identifier":"card-number"}`
	for name, change := range map[string]func(n *Node){
		"new identifier": func(n *Node) { n.Identifier = advStr("card-number-7f3a") },
		"renamed":        func(n *Node) { n.Name = "Card ending 1111" },
	} {
		t.Run(name, func(t *testing.T) {
			o := observationFixture(t)
			o.Nodes = []Node{advCardNode(""), advStatusNode()}
			d := &advDesktop{testDesktop: &testDesktop{observation: o}}
			d.afterAct = func(o *Observation) {
				n := advCardNode(advVariants["spaces"])
				change(&n)
				o.Nodes[0] = n
			}
			records, result, err := advRun(t, advProgram(t, rung), d, o, PrivacyPolicy{})
			if err != nil || result.State.Phase != workflow.Completed {
				t.Fatalf("%s %v", result.State.Phase, err)
			}
			if leaks := advLeaks(records, advSecret); len(leaks) != 0 {
				t.Fatalf("%q", leaks)
			}
		})
	}
}

// Over-masking guard: a role-only rung would name every control of that role.
// Compile refuses it (validateTargetSelector), so sensitiveInputTargets cannot
// be handed one through a compiled program.
func TestAdvB31ComputerRoleOnlyRungCannotCompile(t *testing.T) {
	c := advProgram(t, advDefaultRungs).Capability()
	c.Targets["card"] = workflow.Selector{Strategies: []workflow.Selector{{Role: "text_field"}}, Rationale: "r", Stability: "semantic"}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = workflow.Compile(raw); err == nil || !strings.Contains(err.Error(), "bounded name or identifier") {
		t.Fatalf("a role-only rung compiled or failed for another reason: %v", err)
	}
}

// Fail-closed cost. These mirror what real AX can return.
func TestAdvB31ComputerBoundlessSensitiveControlFailsClosedWithoutExposingIt(t *testing.T) {
	o := observationFixture(t)
	n := advCardNode(advVariants["spaces"])
	n.Bounds = nil
	o.Nodes = []Node{n, advStatusNode()}
	d := &advDesktop{testDesktop: &testDesktop{observation: o}}
	records, result, _ := advRun(t, advProgram(t, advDefaultRungs), d, o, PrivacyPolicy{})
	if result.State.Phase == workflow.Completed {
		t.Fatal("run completed over a sensitive control it could not mask")
	}
	if leaks := advLeaks(records, advSecret); len(leaks) != 0 {
		t.Fatalf("%q", leaks)
	}
}

// BREAKS: the refusal does not say which node. An operator sees only
// "sensitive region has no reliable bounds".
func TestAdvB31ComputerBoundlessSensitiveControlFailureNamesTheNode(t *testing.T) {
	o := observationFixture(t)
	n := advCardNode("x")
	n.Bounds = &Bounds{0, 0, 0, 0}
	o.Nodes = []Node{n}
	_, err := Sanitize(o, PrivacyPolicy{PID: 1, WindowID: 2, SensitiveTargets: []Selector{{Role: "text_field", Name: "Card"}}})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "Card") && !strings.Contains(err.Error(), "card-number") && !strings.Contains(err.Error(), "text_field") {
		t.Fatalf("refusal is not actionable, it names no node/role/identifier: %q", err)
	}
}

// BREAKS (over-fail): a sensitive control scrolled fully outside the window has
// no pixels in the frame to leak, yet fails the whole capture.
func TestAdvB31ComputerSensitiveControlOutsideTheFrameDoesNotFailTheCapture(t *testing.T) {
	o := observationFixture(t)
	n := advCardNode("x")
	n.Bounds = &Bounds{10, 500, 40, 20}
	o.Nodes = []Node{n}
	if _, err := Sanitize(o, PrivacyPolicy{PID: 1, WindowID: 2, SensitiveTargets: []Selector{{Role: "text_field", Name: "Card"}}}); err != nil {
		t.Fatalf("off-screen sensitive control failed the capture: %v", err)
	}
}

// BREAKS: the effective policy (host policy plus identity targets) exists only
// inside StartRun. Consumers that sanitise outside a run (Jarvis
// observeUnknown after a fence, discovery capture, anchor snapshot) have no
// exported way to obtain it, so identity masking does not apply to them.
func advExportedReturningPrivacyPolicy(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, pkg := range pkgs {
		for fname, f := range pkg.Files {
			if strings.HasSuffix(fname, "_test.go") {
				continue
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || !fn.Name.IsExported() || fn.Type.Results == nil {
					continue
				}
				for _, res := range fn.Type.Results.List {
					if id, ok := res.Type.(*ast.Ident); ok && id.Name == "PrivacyPolicy" {
						found = append(found, fn.Name.Name)
					}
				}
			}
		}
	}
	return found
}

func TestAdvB31ComputerEffectivePolicyIsExported(t *testing.T) {
	if got := advExportedReturningPrivacyPolicy(t); len(got) == 0 {
		t.Fatal("no exported function or method returns a PrivacyPolicy: the identity targets StartRun adds are unreachable to callers of Sanitize/CreateVisualAnchor")
	}
}

// ---- approval ----

func TestAdvB31ComputerApprovalConcurrentCallsAgree(t *testing.T) {
	policy := PrivacyPolicy{PID: 1, WindowID: 2, SensitiveTargets: []Selector{{Name: "Member ID"}}}
	raw := parityObservation(t, 64, 48, "alpha", []Node{sensitiveNode("s", "Member ID", 5, 5, 20, 10)})
	for name, sel := range map[string]workflow.Selector{"semantic": {Role: "button", Name: "Go"}, "visual": {Visual: &workflow.VisualAnchor{}}} {
		var want [32]byte
		{
			var r workResult
			var err error
			if want, err = r.approval(raw, policy, sel); err != nil {
				t.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		results := make([][32]byte, 32)
		errs := make([]error, 32)
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var r workResult
				results[i], errs[i] = r.approval(raw, policy, sel)
			}()
		}
		wg.Wait()
		for i := range results {
			if errs[i] != nil || results[i] != want {
				t.Fatalf("%s: call %d: %x %v want %x", name, i, results[i], errs[i], want)
			}
		}
	}
}

// Two approvals sent at once to one run must produce one grant and one input.
func TestAdvB31ComputerConcurrentApproveSendsDispatchOnce(t *testing.T) {
	opts, base := runFixture(t, true)
	d := &advDesktop{testDesktop: base}
	opts.Desktop = d
	ctx := safetyContext(t)
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	s := waitPhase(t, ctx, run, workflow.AwaitingApproval)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = run.Send(ctx, Control{Kind: "approve", Epoch: s.Epoch, ActionID: s.ActionID(opts.Program), ObservationID: s.Observation.ID})
		}()
	}
	wg.Wait()
	result, err := run.Wait(ctx)
	if err != nil || result.State.Phase != workflow.Completed {
		t.Fatalf("%s %v", result.State.Phase, err)
	}
	d.hookMu.Lock()
	grants := d.grants
	d.hookMu.Unlock()
	if grants != 1 || d.actCount() != 1 {
		t.Fatalf("grants=%d acts=%d, want 1 and 1", grants, d.actCount())
	}
}

// A capture that fails at the approval re-capture must stop the run without a
// grant or an input, and must not be treated as an approved zero digest.
func TestAdvB31ComputerApprovalCaptureErrorNeverDispatches(t *testing.T) {
	for name, hook := range map[string]func(n int, focused bool, o *Observation) error{
		"incomplete": func(n int, focused bool, o *Observation) error {
			if focused {
				o.Complete = false
			}
			return nil
		},
		"corrupt png": func(n int, focused bool, o *Observation) error {
			if focused {
				o.Screenshot.Base64 = base64.StdEncoding.EncodeToString([]byte("not a png"))
			}
			return nil
		},
		"observe error": func(n int, focused bool, o *Observation) error {
			if focused {
				return errors.New("capture failed")
			}
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			opts, base := runFixture(t, true)
			d := &advDesktop{testDesktop: base, observeHook: hook}
			opts.Desktop = d
			ctx := safetyContext(t)
			run, err := StartRun(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			s := waitPhase(t, ctx, run, workflow.AwaitingApproval)
			if err := run.Send(ctx, Control{Kind: "approve", Epoch: s.Epoch, ActionID: s.ActionID(opts.Program), ObservationID: s.Observation.ID}); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if snap := run.Snapshot(); snap.Terminal() || (snap.Phase == workflow.AwaitingApproval && snap.Sequence > s.Sequence) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			d.hookMu.Lock()
			grants := d.grants
			d.hookMu.Unlock()
			phase := run.Snapshot().Phase
			if grants != 0 || d.actCount() != 0 || phase == workflow.Completed {
				t.Fatalf("grants=%d acts=%d phase=%s after a failed approval capture", grants, d.actCount(), phase)
			}
			_ = run.Send(ctx, Control{Kind: "cancel", Epoch: run.Snapshot().Epoch})
		})
	}
}

// After a takeover (pause, resume) a late change of a masked private value must
// still be caught, and a grant from before the takeover must not be accepted.
func TestAdvB31ComputerApprovalAfterTakeoverRevalidates(t *testing.T) {
	opts, base := runFixture(t, true)
	old := "old private value"
	base.observation.Nodes = append(base.observation.Nodes, Node{ID: "member", Role: "text_field", Name: "Member ID", Value: &old, Bounds: &Bounds{X: 10, Y: 10, Width: 20, Height: 10}})
	opts.Privacy.SensitiveTargets = []Selector{{Name: "Member ID"}}
	d := &advDesktop{testDesktop: base}
	opts.Desktop = d
	ctx := safetyContext(t)
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	s0 := waitPhase(t, ctx, run, workflow.AwaitingApproval)
	if err := run.Send(ctx, Control{Kind: "pause", Epoch: s0.Epoch}); err != nil {
		t.Fatal(err)
	}
	sp := waitPhase(t, ctx, run, workflow.Paused)
	if err := run.Send(ctx, Control{Kind: "resume", Epoch: sp.Epoch}); err != nil {
		t.Fatal(err)
	}
	s1 := waitPhase(t, ctx, run, workflow.AwaitingApproval)
	if s1.Epoch == s0.Epoch {
		t.Fatalf("epoch did not advance across the takeover")
	}
	// A grant addressed to the pre-takeover review is refused.
	if err := run.Send(ctx, Control{Kind: "approve", Epoch: s0.Epoch, ActionID: s0.ActionID(opts.Program), ObservationID: s0.Observation.ID}); err == nil {
		t.Fatal("a pre-takeover approval was accepted")
	}
	// The masked value changes after the review of the post-takeover frame.
	d.mu.Lock()
	changed := "value typed by the person during takeover"
	d.observation.Nodes[len(d.observation.Nodes)-1].Value = &changed
	d.mu.Unlock()
	if err := run.Send(ctx, Control{Kind: "approve", Epoch: s1.Epoch, ActionID: s1.ActionID(opts.Program), ObservationID: s1.Observation.ID}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if snap := run.Snapshot(); snap.Terminal() || snap.Sequence > s1.Sequence+1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.hookMu.Lock()
	grants := d.grants
	d.hookMu.Unlock()
	if grants != 0 || d.actCount() != 0 {
		t.Fatalf("a changed masked value was approved after takeover: grants=%d acts=%d", grants, d.actCount())
	}
	_ = run.Send(ctx, Control{Kind: "cancel", Epoch: run.Snapshot().Epoch})
}

// BREAKS (low): the digest binds the unmasked pixels and the semantic tree but
// not where the mask lands. Mask placement comes from node Bounds, which
// semanticFingerprint excludes, so two frames identical except for the
// sensitive node's bounds share a digest although the person was shown (and the
// record keeps) different masked pixels.
func TestAdvB31ComputerApprovalDigestBindsMaskPlacement(t *testing.T) {
	policy := PrivacyPolicy{PID: 1, WindowID: 2, SensitiveTargets: []Selector{{Name: "Member ID"}}}
	a := parityObservation(t, 120, 90, "rgba", []Node{sensitiveNode("s", "Member ID", 10, 10, 20, 10)})
	b := parityObservation(t, 120, 90, "rgba", []Node{sensitiveNode("s", "Member ID", 70, 60, 20, 10)})
	for name, sel := range map[string]workflow.Selector{"semantic": {Role: "button", Name: "Go"}, "visual": {Visual: &workflow.VisualAnchor{}}} {
		var ra, rb workResult
		da, err := ra.approval(a, policy, sel)
		if err != nil {
			t.Fatal(err)
		}
		db, err := rb.approval(b, policy, sel)
		if err != nil {
			t.Fatal(err)
		}
		if ra.observation.Screenshot.Base64 == rb.observation.Screenshot.Base64 {
			t.Fatalf("%s: test setup: the masked frames should differ", name)
		}
		if da == db {
			t.Fatalf("%s: two frames with different masked regions have the same approval digest", name)
		}
	}
}

// ---- 16-bit ----

func advDecode(t *testing.T, o Observation) image.Image {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(o.Screenshot.Base64)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func advAbs(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

func TestAdvB31Computer16BitMasksEdgesOddSizesAndOneByOne(t *testing.T) {
	policy := PrivacyPolicy{PID: 1, WindowID: 2, SensitiveTargets: []Selector{{Name: "Member ID"}}}
	sizes := [][2]int{{1, 1}, {3, 5}, {7, 1}, {1, 9}, {13, 11}, {257, 3}}
	for _, kind := range []string{"rgba64", "nrgba64", "gray16"} {
		for _, sz := range sizes {
			w, h := sz[0], sz[1]
			corners := map[string][2]int{"tl": {0, 0}, "tr": {w - 1, 0}, "bl": {0, h - 1}, "br": {w - 1, h - 1}}
			for cname, c := range corners {
				t.Run(kind+"/"+cname+"/"+advItoa(w)+"x"+advItoa(h), func(t *testing.T) {
					raw := parityObservation(t, w, h, kind, []Node{sensitiveNode("s", "Member ID", float64(c[0]), float64(c[1]), 1, 1)})
					requireDecodedAs(t, raw, kind)
					src := advDecode(t, raw)
					var r workResult
					if err := r.capture(raw, policy); err != nil {
						t.Fatal(err)
					}
					out := advDecode(t, *r.observation)
					if out.Bounds().Dx() != w || out.Bounds().Dy() != h {
						t.Fatalf("size %v", out.Bounds())
					}
					mask := image.Rect(c[0]-2, c[1]-2, c[0]+3, c[1]+3).Intersect(image.Rect(0, 0, w, h))
					for y := 0; y < h; y++ {
						for x := 0; x < w; x++ {
							gr, gg, gb, ga := out.At(x, y).RGBA()
							if image.Pt(x, y).In(mask) {
								if gr>>8 != 20 || gg>>8 != 20 || gb>>8 != 24 || ga>>8 != 255 {
									t.Fatalf("(%d,%d) inside the mask is %d,%d,%d,%d", x, y, gr>>8, gg>>8, gb>>8, ga>>8)
								}
								continue
							}
							sr, sg, sb, sa := src.At(x, y).RGBA()
							if advAbs(gr>>8, sr>>8) > 2 || advAbs(gg>>8, sg>>8) > 2 || advAbs(gb>>8, sb>>8) > 2 || advAbs(ga>>8, sa>>8) > 2 {
								t.Fatalf("(%d,%d) outside the mask changed: got %d,%d,%d,%d want %d,%d,%d,%d", x, y, gr>>8, gg>>8, gb>>8, ga>>8, sr>>8, sg>>8, sb>>8, sa>>8)
							}
						}
					}
				})
			}
		}
	}
}

func advItoa(n int) string { return strconv.Itoa(n) }

// Fully transparent 16-bit pixels (alpha 0) under and beside a mask.
func TestAdvB31Computer16BitAlphaZeroRegionsCaptureAndMask(t *testing.T) {
	policy := PrivacyPolicy{PID: 1, WindowID: 2, SensitiveTargets: []Selector{{Name: "Member ID"}}}
	for _, sz := range [][2]int{{1, 1}, {9, 7}, {20, 20}} {
		w, h := sz[0], sz[1]
		m := image.NewNRGBA64(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				a := uint16(0)
				if x%3 == 0 && y%2 == 0 {
					a = 40000
				}
				m.SetNRGBA64(x, y, color.NRGBA64{R: uint16(x*977 + 1), G: 65535, B: uint16(y*131 + 3), A: a})
			}
		}
		raw := Observation{ID: "o", Epoch: 1, Complete: true,
			Window:     Window{PID: 1, ID: 2, Bounds: Bounds{0, 0, float64(w), float64(h)}, Scale: 1},
			Nodes:      []Node{sensitiveNode("s", "Member ID", 0, 0, float64(w), float64(h))},
			Screenshot: Screenshot{MIMEType: "image/png", Base64: encodePNG(t, m), Width: dim32(w), Height: dim32(h)}}
		var r workResult
		if err := r.capture(raw, policy); err != nil {
			t.Fatalf("%dx%d: %v", w, h, err)
		}
		out := advDecode(t, *r.observation)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				_, _, _, a := out.At(x, y).RGBA()
				if a>>8 != 255 {
					t.Fatalf("%dx%d (%d,%d): a masked pixel is not opaque (alpha %d)", w, h, x, y, a>>8)
				}
			}
		}
		if _, err := frameFingerprint(raw); err != nil {
			t.Fatal(err)
		}
	}
}

// BREAKS (low, design): the digest is taken from an 8-bit NRGBA conversion, so
// a change that lives only in the low byte of a 16-bit sample is not detected.
// The parity oracle uses the same conversion, which is why parity holds.
func TestAdvB31ComputerFrameDigestSeesTheLowByteOf16BitSamples(t *testing.T) {
	mk := func(v uint16) Observation {
		m := image.NewGray16(image.Rect(0, 0, 2, 2))
		for y := 0; y < 2; y++ {
			for x := 0; x < 2; x++ {
				m.SetGray16(x, y, color.Gray16{Y: 0x1200})
			}
		}
		m.SetGray16(1, 1, color.Gray16{Y: v})
		return Observation{ID: "o", Epoch: 1, Complete: true,
			Window:     Window{PID: 1, ID: 2, Bounds: Bounds{0, 0, 2, 2}, Scale: 1},
			Screenshot: Screenshot{MIMEType: "image/png", Base64: encodePNG(t, m), Width: 2, Height: 2}}
	}
	a, b := mk(0x1234), mk(0x1235)
	da, err := frameFingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := frameFingerprint(b)
	if err != nil {
		t.Fatal(err)
	}
	if da == db {
		t.Fatal("16-bit frames that differ only in the low byte share a frame digest")
	}
}
