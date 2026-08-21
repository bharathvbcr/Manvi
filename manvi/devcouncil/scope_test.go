package devcouncil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"manvi/core/bus"
	"manvi/dc/store"
	"manvi/flags"
	"manvi/gate"
	"manvi/internal/testsupport"
	"manvi/tools"
)

// secondSession builds a fresh tool surface over the same repository and the
// same store: a new process's worth of state, with its own grant ledger.
//
// It is how the durability claim is actually tested. A grant lives in a ledger
// that dies with the process; scope written into the task does not, and the
// only way to tell those apart is to throw the ledger away and try again.
func (f *fixture) secondSession(t *testing.T) *fixture {
	t.Helper()
	client := store.New(testsupport.DCStore(t), f.db)

	regFlags := flags.New()
	if err := flags.DefineHarnessFlags(regFlags); err != nil {
		t.Fatal(err)
	}
	if err := regFlags.LoadConfig(map[string]string{flags.HarnessPosture: flags.PostureStrict}); err != nil {
		t.Fatal(err)
	}
	g, err := gate.New(regFlags, f.root, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := New(Deps{
		Store: client, Gate: g, Root: f.root, LeaseTTL: 10 * time.Minute,
		VerifierBinary: testsupport.DCVerify(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	pipe := tools.NewRegistry(bus.New())
	if err := reg.Register(pipe); err != nil {
		t.Fatal(err)
	}
	return &fixture{reg: reg, pipe: pipe, root: f.root, db: f.db, t: t}
}

// The flexibility this rung exists for: the test file beside the file being
// fixed, with no repo map built and no override asked for.
func TestASiblingOfAPlannedFileNeedsNoOverride(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	res := f.call("devcouncil_write_file", map[string]string{
		"path": "src/calc_test.go", "content": "package calc\n",
	})
	if res.IsError {
		t.Fatalf("a sibling of the planned file should not need an override: %s", res.Text)
	}
	if _, err := os.Stat(filepath.Join(f.root, "src/calc_test.go")); err != nil {
		t.Fatalf("the write did not reach the filesystem: %v", err)
	}
	// It is a pass reached without the subsystem check, and it says so.
	if !res.Qualified() {
		t.Fatalf("a proximity allow is not an unqualified pass: %+v", res)
	}
	if len(res.Degraded) == 0 {
		t.Fatalf("the allow must carry the check that could not run: %+v", res)
	}
}

// The durability claim, end to end: an argument made once survives the grant
// that recorded it, and survives the process that issued it.
func TestAWideningOutlivesTheGrantAndTheProcess(t *testing.T) {
	first := newFixture(t)
	first.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	granted := first.payload("devcouncil_request_override", map[string]string{
		"path": "internal/helper.go", "rule": "scope.unplanned",
		"reason": "the fix needs a helper the plan did not enumerate",
	})
	if granted["granted"] != true || granted["scope_persisted"] != true {
		t.Fatalf("the override must be granted and recorded in the task's scope: %v", granted)
	}
	first.payload("devcouncil_release_task", map[string]any{})

	// A new session: new gate, new ledger, no grant anywhere in it.
	second := first.secondSession(t)
	second.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	res := second.call("devcouncil_write_file", map[string]string{
		"path": "internal/helper.go", "content": "package internal\n",
	})
	if res.IsError {
		t.Fatalf("the widening should still authorise this write: %s", res.Text)
	}
	if res.GrantID != "" {
		t.Fatalf("no grant exists in this session; the scope authorised it: %+v", res)
	}
	if res.Widened != "internal/helper.go" {
		t.Fatalf("the write must name the widening that authorised it: %+v", res)
	}

	// And it is still not a clean pass, a process later, with nobody around who
	// remembers the argument.
	if !res.Qualified() {
		t.Fatal("a write on self-appended scope never becomes an ordinary pass")
	}
}

// The ratchet: one file argued into scope must not carry its neighbours in
// with it.
func TestAWideningDoesNotDragItsDirectoryIntoScope(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	granted := f.payload("devcouncil_request_override", map[string]string{
		"path": "internal/helper.go", "rule": "scope.unplanned", "reason": "needed by the fix",
	})
	if granted["scope_persisted"] != true {
		t.Fatalf("setup: the widening was not recorded: %v", granted)
	}

	res := f.call("devcouncil_write_file", map[string]string{
		"path": "internal/unrelated.go", "content": "package internal\n",
	})
	if !res.IsError {
		t.Fatal("a file beside an appended one is not itself in scope")
	}
}

// A widening is written under the lease, and a widening asked for without one
// is refused before it reaches the store.
func TestAWideningNeedsTheLease(t *testing.T) {
	f := newFixture(t)
	res := f.call("devcouncil_request_override", map[string]string{
		"path": "internal/helper.go", "rule": "scope.unplanned", "reason": "no lease held",
	})
	if !res.IsError {
		t.Fatal("an override with no task checked out must be refused")
	}

	// And nothing was written to the task that was never checked out.
	client := store.New(testsupport.DCStore(t), f.db)
	task, err := client.Task(context.Background(), "TASK-001")
	if err != nil {
		t.Fatal(err)
	}
	if len(task.AgentAppended) != 0 {
		t.Fatalf("scope was widened without a lease: %+v", task.AgentAppended)
	}
}

// The scope-widening attack this whole path has to survive: an override argued
// for a *pattern* rather than a file. The target is quoted before it is
// written, so what lands in the task's scope covers itself and nothing else.
func TestAnOverrideForAWildcardWidensOnlyTheLiteralPath(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	granted := f.payload("devcouncil_request_override", map[string]string{
		"path": "**", "rule": "scope.unplanned", "reason": "everything, please",
	})
	if granted["granted"] != true {
		t.Fatalf("setup: the override was not issued: %v", granted)
	}
	if appended, _ := granted["scope_appended"].(string); appended != "[*][*]" {
		t.Fatalf("the appended pattern must be the quoted literal, got %q", appended)
	}

	// The repository is not now writable.
	for _, target := range []string{"internal/anything.go", "top.go", "docs/x.md"} {
		if res := f.call("devcouncil_write_file", map[string]string{
			"path": target, "content": "x\n",
		}); !res.IsError {
			t.Errorf("%s became writable through a wildcard override: %+v", target, res)
		}
	}
	// The literal file the pattern was quoted into is the one thing it covers.
	if res := f.call("devcouncil_write_file", map[string]string{"path": "**", "content": "x\n"}); res.IsError {
		t.Errorf("the quoted pattern must still cover the path it was issued for: %s", res.Text)
	}

	// And a secret is still refused, override or not.
	if res := f.call("devcouncil_write_file", map[string]string{"path": ".env", "content": "K=v"}); !res.IsError {
		t.Fatal(".env must never be writable")
	}
}

// Scope is bounded. An agent that argues for path after path meets a ceiling
// rather than an unbounded plan.
func TestWideningIsBoundedPerTask(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	// Well under the ceiling, to keep this a test of the mechanism rather than
	// of how long 256 store invocations take.
	for i := 0; i < 5; i++ {
		got := f.payload("devcouncil_request_override", map[string]string{
			"path":   fmt.Sprintf("internal/f%d.go", i),
			"rule":   "scope.unplanned",
			"reason": "the fix needs it",
		})
		if got["scope_persisted"] != true {
			t.Fatalf("widening %d was not recorded: %v", i, got)
		}
	}

	client := store.New(testsupport.DCStore(t), f.db)
	task, err := client.Task(context.Background(), "TASK-001")
	if err != nil {
		t.Fatal(err)
	}
	if len(task.AgentAppended) != 5 {
		t.Fatalf("appended scope = %d entries, want the five that were argued", len(task.AgentAppended))
	}
	// The plan is untouched by any of it.
	if got := task.Domain().PlannedFiles; len(got) != 1 || got[0].Path != "src/calc.go" {
		t.Fatalf("the planner's own scope must be unchanged: %+v", got)
	}
}
