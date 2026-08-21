package devcouncil

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"manvi/flags"
	"manvi/tools"
)

// callAs dispatches one tool call under a named session, the way a dispatched
// sub-agent's calls arrive.
func (f *fixture) callAs(sess *Session, name string, args any) tools.Result {
	f.t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.pipe.Run(WithSession(context.Background(), sess), tools.Call{
		ID: "c1", Name: name, Arguments: raw,
	})
}

func (f *fixture) payloadAs(sess *Session, name string, args any) map[string]any {
	f.t.Helper()
	result := f.callAs(sess, name, args)
	var out map[string]any
	if err := json.Unmarshal([]byte(result.Text), &out); err != nil {
		f.t.Fatalf("%s returned unparseable payload: %v (%q)", name, err, result.Text)
	}
	return out
}

// TestOneChildsCheckoutIsInvisibleToItsSiblings is the defect this file was
// added for.
//
// Every sub-agent used to write through the Registry's single Session, so two
// children checking out two tasks at the same moment raced on one TaskID and
// one Token. Under the race detector an eight-way fan-out reported four races,
// and a child holding TASK-C ran its verification against TASK-E.
//
// The lease itself was never wrong — the store's partial unique index gave each
// task to exactly one holder. What was wrong was the harness's own record of
// which lease it held, which is what the write gate and the verifier read.
func TestOneChildsCheckoutIsInvisibleToItsSiblings(t *testing.T) {
	f := newFixture(t)
	insertTask(t, storeBinary(t), f.db, "TASK-002", `[{"path":"src/other.go","allowed_change":"modify"}]`)

	root := f.reg.RootSession()
	first := root.NewChildSession(nil)
	second := root.NewChildSession(nil)

	if got := f.payloadAs(first, "devcouncil_checkout_task",
		map[string]any{"task_id": "TASK-001"}); got["acquired"] != true {
		t.Fatalf("first child could not check out: %v", got)
	}
	if got := f.payloadAs(second, "devcouncil_checkout_task",
		map[string]any{"task_id": "TASK-002"}); got["acquired"] != true {
		t.Fatalf("second child could not check out: %v", got)
	}

	if got := first.State().TaskID; got != "TASK-001" {
		t.Errorf("the first child now believes it holds %q; its sibling's checkout overwrote its own", got)
	}
	if got := second.State().TaskID; got != "TASK-002" {
		t.Errorf("the second child holds %q, want TASK-002", got)
	}
	// The dispatching agent checked nothing out and must still hold nothing.
	if got := f.reg.Session().TaskID; got != "" {
		t.Errorf("the dispatching agent's session says it holds %q; a child's checkout leaked into it", got)
	}
}

// TestConcurrentChildCheckoutsDoNotRace drives the same shape the race detector
// caught in a live fan-out. It is meaningful only under -race, and harmless
// without it.
func TestConcurrentChildCheckoutsDoNotRace(t *testing.T) {
	f := newFixture(t)
	ids := []string{"TASK-001", "TASK-R2", "TASK-R3", "TASK-R4"}
	for _, id := range ids[1:] {
		insertTask(t, storeBinary(t), f.db, id, `[{"path":"src/calc.go","allowed_change":"modify"}]`)
	}

	root := f.reg.RootSession()
	var wg sync.WaitGroup
	sessions := make([]*Session, len(ids))
	for i, id := range ids {
		sessions[i] = root.NewChildSession(nil)
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			f.callAs(sessions[i], "devcouncil_checkout_task", map[string]any{"task_id": id})
			// A read on the same path the write gate takes, so a torn read is
			// reported rather than merely made possible.
			f.callAs(sessions[i], "devcouncil_policy_check_write", map[string]any{"path": "src/calc.go"})
		}(i, id)
	}
	wg.Wait()

	for i, id := range ids {
		if got := sessions[i].State().TaskID; got != id {
			t.Errorf("child %d holds %q, want %q — sessions are being shared", i, got, id)
		}
	}
}

// TestAChildCannotReleaseTheLeaseItInherited.
//
// A child copies its parent's session so its writes are judged against the task
// the parent holds. That copy carries a token the store will honour, so without
// this the child could hand back a lease the parent still believes it owns —
// and the next sibling's write would be judged against a lease nobody holds.
func TestAChildCannotReleaseTheLeaseItInherited(t *testing.T) {
	f := newFixture(t)
	if got := f.payload("devcouncil_checkout_task",
		map[string]any{"task_id": "TASK-001"}); got["acquired"] != true {
		t.Fatalf("the parent could not check out: %v", got)
	}

	child := f.reg.RootSession().NewChildSession(nil)
	if got := child.State().TaskID; got != "TASK-001" {
		t.Fatalf("the child did not inherit the parent's task, it has %q", got)
	}

	for _, name := range []string{"devcouncil_release_task", "devcouncil_renew_lease"} {
		res := f.callAs(child, name, map[string]any{})
		if !res.IsError {
			t.Errorf("%s succeeded for a child acting on a lease it does not own: %q", name, res.Text)
		}
		if res.Rule != "lease.not_owned" {
			t.Errorf("%s refused with rule %q, want lease.not_owned", name, res.Rule)
		}
	}
	// And the parent still holds it.
	if got := f.reg.Session().TaskID; got != "TASK-001" {
		t.Errorf("the parent's lease record is now %q; the child released it out from under them", got)
	}
}

// recordingSink stands in for a fan-out's lease bookkeeping.
type recordingSink struct {
	mu      sync.Mutex
	held    []string
	dropped []string
}

func (s *recordingSink) HoldLease(taskID, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token == "" {
		panic("a lease was reported with no token; cleanup could not release it")
	}
	s.held = append(s.held, taskID)
}

func (s *recordingSink) DropLease(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropped = append(s.dropped, taskID)
}

// TestAChildsOwnCheckoutIsReportedToTheFanOut is the second half of the same
// defect.
//
// agents.Holder was only ever told about a lease the *dispatcher* named in
// tasks[].task_id. A child that called devcouncil_checkout_task — the tool it
// is given, and the only way it ever takes one — registered nothing, so the
// pool's cleanup had nothing to release. Measured on an eight-way fan-out of
// children that each checked out a task: seven leases stayed held for the full
// TTL after the run exited, and the fan-out reported "clean": true.
func TestAChildsOwnCheckoutIsReportedToTheFanOut(t *testing.T) {
	f := newFixture(t)
	sink := &recordingSink{}
	child := f.reg.RootSession().NewChildSession(sink)

	if got := f.payloadAs(child, "devcouncil_checkout_task",
		map[string]any{"task_id": "TASK-001"}); got["acquired"] != true {
		t.Fatalf("the child could not check out: %v", got)
	}
	if len(sink.held) != 1 || sink.held[0] != "TASK-001" {
		t.Fatalf("the fan-out was told about %v, want [TASK-001] — a lease nobody knows about "+
			"is a lease nobody can release", sink.held)
	}

	if res := f.callAs(child, "devcouncil_release_task", map[string]any{}); res.IsError {
		t.Fatalf("the child could not release a lease it took itself: %s", res.Text)
	}
	if len(sink.dropped) != 1 || sink.dropped[0] != "TASK-001" {
		t.Fatalf("the fan-out was not told the lease came back: %v — cleanup would try again", sink.dropped)
	}
}

// TestDelegationIsOffAtDepthZero.
//
// agents.max_spawn_depth was read from the catalogue, handed to a pool whose
// Child() nothing calls, and enforced at no value: 0 dispatched a full fan-out
// exactly as 2 did. A setting `manvi flags` reports and that binds nowhere
// reads as a control that is in force.
func TestDelegationIsOffAtDepthZero(t *testing.T) {
	f := newFixtureWith(t, map[string]string{
		flags.HarnessPosture:          flags.PostureStrict,
		flags.AgentsMaxSpawnDepth:     "0",
		flags.SubagentsDynamicEnabled: "true",
	})

	for _, tc := range []struct {
		name string
		args any
	}{
		{"devcouncil_spawn_subagents", map[string]any{
			"tasks": []map[string]any{{"label": "a", "prompt": "do a thing"}}}},
		{"devcouncil_invoke_subagent", map[string]any{
			"subagents": []map[string]any{{"type_name": "self", "role": "a", "prompt": "do a thing"}}}},
	} {
		res := f.call(tc.name, tc.args)
		if !res.IsError {
			t.Errorf("%s dispatched with %s=0: %q", tc.name, flags.AgentsMaxSpawnDepth, res.Text)
		}
		if res.Rule != flags.AgentsMaxSpawnDepth {
			t.Errorf("%s refused with rule %q, want %s — a refusal that does not name the setting "+
				"leaves the model rewording and retrying", tc.name, res.Rule, flags.AgentsMaxSpawnDepth)
		}
	}
}

// TestAChildIsJudgedAgainstTheTaskItsParentHolds is the behaviour the shared
// session got right, and the reason a child inherits rather than starting
// empty.
//
// A fan-out under a checked-out task exists to work on that task. If the child
// started with no lease, its writes would be judged with no scope at all — the
// gate's "no task authorises this" rung — and a fan-out would be strictly less
// able to do the work than the agent that dispatched it.
func TestAChildIsJudgedAgainstTheTaskItsParentHolds(t *testing.T) {
	f := newFixture(t)
	if got := f.payload("devcouncil_checkout_task",
		map[string]any{"task_id": "TASK-001"}); got["acquired"] != true {
		t.Fatalf("the parent could not check out: %v", got)
	}

	child := f.reg.RootSession().NewChildSession(nil)
	// src/calc.go is TASK-001's one planned file. Under the strict posture the
	// gate allows it only because a task authorises it.
	got := f.payloadAs(child, "devcouncil_policy_check_write", map[string]any{"path": "src/calc.go"})
	if got["allowed"] != true {
		t.Errorf("a planned write was refused for a child of the agent holding the plan: %v", got)
	}

	// And a file the plan does not reach is still refused, so inheritance
	// carries the scope rather than removing the check. A different directory,
	// not a sibling: the neighbour rule deliberately admits a file beside a
	// planned one, and testing against that would be testing the neighbour rule
	// rather than the inheritance.
	outside := f.payloadAs(child, "devcouncil_policy_check_write",
		map[string]any{"path": "vendor/elsewhere.go"})
	if outside["allowed"] == true {
		t.Errorf("a write the plan does not reach was allowed for an inheriting child: %v", outside)
	}
}
