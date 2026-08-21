package store

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"manvi/dc"
	"manvi/internal/testsupport"
)

// scopeFixture is a real store with one planned task and a live lease on it.
// Everything here crosses the process boundary on purpose: the widening is a
// compare-and-swap in SQLite, and a fake would prove nothing about it.
type scopeFixture struct {
	c     *Client
	token string
	ctx   context.Context
}

func newScopeFixture(t *testing.T, plannedJSON string) *scopeFixture {
	t.Helper()
	c := client(t)
	ctx := context.Background()

	// Touching the store creates the schema; then the task is planted directly,
	// because planning is DevCouncil's job and not this client's.
	if _, err := c.ActiveLeases(ctx); err != nil {
		t.Fatalf("store: %v", err)
	}
	sql := "INSERT INTO tasks (id,title,description,planned_files_json,status) VALUES ('TASK-1','t','d','" +
		strings.ReplaceAll(plannedJSON, "'", "''") + "','ready');"
	cmd := exec.Command(testsupport.Tool(t, "sqlite3"), c.DB, sql)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seeding a task failed: %v %s", err, out)
	}

	lease, err := c.Acquire(ctx, AcquireRequest{TaskID: "TASK-1", Owner: "builder-1", TTL: 15 * time.Minute})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	return &scopeFixture{c: c, token: lease.Token, ctx: ctx}
}

func (f *scopeFixture) task(t *testing.T) *Task {
	t.Helper()
	task, err := f.c.Task(f.ctx, "TASK-1")
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	if task == nil {
		t.Fatal("the planted task disappeared")
	}
	return task
}

func paths(files []dc.PlannedFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// The round trip, and the split that keeps a widening distinguishable from a
// plan on the way back out.
func TestAppendedScopeRoundTripsAndStaysDistinguishable(t *testing.T) {
	f := newScopeFixture(t, `[{"path":"src/calc.go","allowed_change":"modify"}]`)

	added, err := f.c.AppendPlannedFiles(f.ctx, "TASK-1", f.token, []dc.PlannedFile{
		{Path: "internal/helper.go", AllowedChange: dc.ChangeModify},
		{Path: "internal/helper_test.go", AllowedChange: dc.ChangeCreate},
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(added) != 2 {
		t.Fatalf("added = %v, want both entries", paths(added))
	}

	task := f.task(t)
	if got := paths(task.PlannedFiles); len(got) != 3 {
		t.Fatalf("the store returns the union the gate enforces, got %v", got)
	}
	if got := paths(task.AgentAppended); len(got) != 2 {
		t.Fatalf("the appended column must come back on its own, got %v", got)
	}

	// And the domain split: the plan is the plan, the widening is the widening.
	domain := task.Domain()
	if got := paths(domain.PlannedFiles); len(got) != 1 || got[0] != "src/calc.go" {
		t.Fatalf("planned files = %v, want only what the planner declared", got)
	}
	if got := paths(domain.AgentAppendedPlannedFiles); len(got) != 2 {
		t.Fatalf("appended files = %v, want both widenings", got)
	}
	if got := paths(domain.AllPlannedFiles()); len(got) != 3 {
		t.Fatalf("the union = %v, want everything in scope", got)
	}
}

// The column has no set semantics, so a retried argument would grow it on every
// attempt until it hit the ceiling.
func TestAppendingWhatIsAlreadyInScopeAddsNothing(t *testing.T) {
	f := newScopeFixture(t, `[{"path":"src/calc.go","allowed_change":"modify"}]`)
	entry := []dc.PlannedFile{{Path: "internal/helper.go", AllowedChange: dc.ChangeModify}}

	if added, err := f.c.AppendPlannedFiles(f.ctx, "TASK-1", f.token, entry); err != nil || len(added) != 1 {
		t.Fatalf("first append: added=%v err=%v", paths(added), err)
	}
	for i := 0; i < 3; i++ {
		added, err := f.c.AppendPlannedFiles(f.ctx, "TASK-1", f.token, entry)
		if err != nil {
			t.Fatalf("repeat append %d: %v", i, err)
		}
		if len(added) != 0 {
			t.Fatalf("repeat append %d added %v; the path was already in scope", i, paths(added))
		}
	}
	// A path the planner already declared is likewise nothing to add.
	added, err := f.c.AppendPlannedFiles(f.ctx, "TASK-1", f.token,
		[]dc.PlannedFile{{Path: "src/calc.go", AllowedChange: dc.ChangeModify}})
	if err != nil || len(added) != 0 {
		t.Fatalf("appending a planned path: added=%v err=%v", paths(added), err)
	}

	if got := paths(f.task(t).AgentAppended); len(got) != 1 {
		t.Fatalf("appended scope = %v, want exactly one entry after four attempts", got)
	}
}

// Widening a task's scope is a privileged act and the lease is the privilege.
func TestOnlyTheLeaseHolderCanWidenScope(t *testing.T) {
	f := newScopeFixture(t, `[]`)
	entry := []dc.PlannedFile{{Path: "internal/helper.go", AllowedChange: dc.ChangeModify}}

	_, err := f.c.AppendPlannedFiles(f.ctx, "TASK-1", "not-the-token", entry)
	if err == nil {
		t.Fatal("a widening with someone else's token must be refused")
	}
	if !strings.Contains(err.Error(), "lease") {
		t.Fatalf("the refusal must name the lease: %v", err)
	}

	if _, err := f.c.AppendPlannedFiles(f.ctx, "TASK-1", "", entry); err == nil {
		t.Fatal("a widening with no token at all must be refused")
	}
	if got := paths(f.task(t).AgentAppended); len(got) != 0 {
		t.Fatalf("nothing should have been written: %v", got)
	}
}

// Appended scope only ever widens. A restriction an executor writes about
// itself is not a restriction — it is a way to make the plan look stricter than
// the permissions actually in force.
func TestAppendedScopeCannotBeARestriction(t *testing.T) {
	f := newScopeFixture(t, `[]`)
	_, err := f.c.AppendPlannedFiles(f.ctx, "TASK-1", f.token,
		[]dc.PlannedFile{{Path: "src/calc.go", AllowedChange: dc.ChangeReadOnly}})
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("err = %v, want a refusal naming the read-only entry", err)
	}
}

func TestAWideningWithNoPathIsRefused(t *testing.T) {
	f := newScopeFixture(t, `[]`)
	if _, err := f.c.AppendPlannedFiles(f.ctx, "TASK-1", f.token,
		[]dc.PlannedFile{{Path: "   ", AllowedChange: dc.ChangeModify}}); err == nil {
		t.Fatal("an entry with no path widens nothing and must be refused")
	}
	if _, err := f.c.AppendPlannedFiles(f.ctx, "TASK-1", f.token, nil); err != nil {
		t.Fatalf("asking to add nothing is not an error: %v", err)
	}
}

// A plan that has grown this far past itself needs a human, not another append.
func TestAppendedScopeIsBounded(t *testing.T) {
	f := newScopeFixture(t, `[]`)
	batch := make([]dc.PlannedFile, 0, maxAppendedScopeEntries+1)
	for i := 0; i <= maxAppendedScopeEntries; i++ {
		batch = append(batch, dc.PlannedFile{
			Path: fmt.Sprintf("internal/f%03d.go", i), AllowedChange: dc.ChangeModify,
		})
	}
	if _, err := f.c.AppendPlannedFiles(f.ctx, "TASK-1", f.token, batch); err == nil {
		t.Fatal("a widening past the entry ceiling must be refused")
	}
	if got := paths(f.task(t).AgentAppended); len(got) != 0 {
		t.Fatalf("a refused widening must write nothing, got %d entries", len(got))
	}
}

func TestWideningAnUnknownTaskIsRefused(t *testing.T) {
	f := newScopeFixture(t, `[]`)
	_, err := f.c.AppendPlannedFiles(f.ctx, "TASK-NOPE", f.token,
		[]dc.PlannedFile{{Path: "x.go", AllowedChange: dc.ChangeModify}})
	if err == nil || !strings.Contains(err.Error(), "TASK-NOPE") {
		t.Fatalf("err = %v, want a refusal naming the task that does not exist", err)
	}
}

// The compare-and-swap under contention, which is where two defects hid.
//
// Two invariants, and they are different. The first is that nothing is silently
// lost: whatever reports success is in the store afterwards, and whatever does
// not says why. The second is that the swap *converges* — with as many attempts
// as contenders, every widening lands, because each lost race is another
// contender that succeeded. The second is the one that caught the deferred
// transaction: SQLite refuses a read-to-write upgrade without waiting, so
// half these writers used to fail with "database is locked" rather than queue.
func TestConcurrentWideningsAreNeverSilentlyLost(t *testing.T) {
	f := newScopeFixture(t, `[]`)

	const writers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded := map[string]bool{}
	var refusals []error

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("internal/w%d.go", i)
			added, err := f.c.AppendPlannedFiles(f.ctx, "TASK-1", f.token,
				[]dc.PlannedFile{{Path: path, AllowedChange: dc.ChangeModify}})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				refusals = append(refusals, err)
				return
			}
			if len(added) == 1 {
				succeeded[path] = true
			}
		}(i)
	}
	wg.Wait()

	stored := map[string]bool{}
	for _, p := range paths(f.task(t).AgentAppended) {
		if stored[p] {
			t.Fatalf("%s was written twice; the swap is not idempotent under contention", p)
		}
		stored[p] = true
	}
	for path := range succeeded {
		if !stored[path] {
			t.Errorf("%s reported success and is not in the store", path)
		}
	}
	// A refusal is allowed under contention, and must say what happened rather
	// than reporting a widening nobody performed.
	for _, err := range refusals {
		if !strings.Contains(err.Error(), "changed under this one") {
			t.Errorf("a contended refusal must name the contention: %v", err)
		}
	}
	if len(succeeded)+len(refusals) != writers {
		t.Fatalf("%d succeeded and %d refused, want %d accounted for", len(succeeded), len(refusals), writers)
	}
	// Convergence. writers <= maxScopeAppendAttempts, so no writer can be
	// displaced more times than it has attempts, and every one must land.
	if writers > maxScopeAppendAttempts {
		t.Fatalf("this test asserts convergence and needs writers (%d) <= attempts (%d)",
			writers, maxScopeAppendAttempts)
	}
	if len(succeeded) != writers {
		t.Fatalf("%d/%d widenings landed; with %d attempts available every one should queue and succeed (refusals: %v)",
			len(succeeded), writers, maxScopeAppendAttempts, refusals)
	}
}
