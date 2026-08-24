package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"manvi/dc"
	"manvi/internal/testsupport"
)

// binary returns the real dcstore binary. The tests drive it over a real
// process boundary rather than a fake: the thing under test *is* the boundary,
// and a mock of it would prove nothing about whether Go and Rust actually agree.
func binary(t *testing.T) string {
	t.Helper()
	return testsupport.DCStore(t)
}

func client(t *testing.T) *Client {
	t.Helper()
	return New(binary(t), filepath.Join(t.TempDir(), "state.sqlite"))
}

func TestAcquireReleaseRoundTrip(t *testing.T) {
	c := client(t)
	ctx := context.Background()

	lease, err := c.Acquire(ctx, AcquireRequest{TaskID: "TASK-001", Owner: "builder-1", TTL: 15 * time.Minute})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if lease.TaskID != "TASK-001" || lease.Owner != "builder-1" || lease.Token == "" {
		t.Fatalf("lease = %+v", lease)
	}
	if lease.ExpiresAt == "" {
		t.Fatal("a lease with a TTL must carry an expiry")
	}

	ok, err := c.Valid(ctx, "TASK-001", lease.Token)
	if err != nil || !ok {
		t.Fatalf("fresh lease should validate: ok=%v err=%v", ok, err)
	}

	released, err := c.Release(ctx, "TASK-001", lease.Token)
	if err != nil || !released {
		t.Fatalf("release: released=%v err=%v", released, err)
	}
	if ok, _ := c.Valid(ctx, "TASK-001", lease.Token); ok {
		t.Fatal("a released lease must stop validating")
	}
}

// TestTwoBuildersRacingOneTaskProduceExactlyOneHolder is the Phase 2 gate. It
// is the reason the store exists.
func TestTwoBuildersRacingOneTaskProduceExactlyOneHolder(t *testing.T) {
	c := client(t)
	ctx := context.Background()

	first, err := c.Acquire(ctx, AcquireRequest{TaskID: "TASK-001", Owner: "builder-1", TTL: time.Minute})
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	_, err = c.Acquire(ctx, AcquireRequest{TaskID: "TASK-001", Owner: "builder-2", TTL: time.Minute})
	var conflict *Conflict
	if !errors.As(err, &conflict) {
		t.Fatalf("second acquire = %v, want *Conflict", err)
	}
	if conflict.Holder != "builder-1" {
		t.Fatalf("conflict names %q as holder, want builder-1", conflict.Holder)
	}
	// Contention must be routable, not just an error string.
	if conflict.Code() != dc.LeaseHeldByOther {
		t.Fatalf("conflict code = %q", conflict.Code())
	}

	leases, err := c.ActiveLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Token != first.Token {
		t.Fatalf("active leases = %+v; exactly one builder may hold the task", leases)
	}
}

func TestDiagnoseDistinguishesTheFailureModes(t *testing.T) {
	c := client(t)
	ctx := context.Background()

	lease, err := c.Acquire(ctx, AcquireRequest{TaskID: "TASK-001", Owner: "builder-1", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	// The right token on the right task.
	code, _, _, err := c.Diagnose(ctx, "TASK-001", lease.Token)
	if err != nil || code != dc.LeaseValid {
		t.Fatalf("code = %q err = %v, want valid", code, err)
	}

	// A token nobody issued is invalid, and the recovery says so.
	code, action, tool, err := c.Diagnose(ctx, "TASK-001", "not-a-real-token")
	if err != nil {
		t.Fatal(err)
	}
	if code != dc.LeaseInvalid && code != dc.LeaseHeldByOther {
		t.Fatalf("code = %q, want invalid or held-by-other", code)
	}
	if action == "" || tool == "" {
		t.Fatalf("a failure code must carry a recovery, got action=%q tool=%q", action, tool)
	}
}

func TestRenewExtendsAndReportsExpiry(t *testing.T) {
	c := client(t)
	ctx := context.Background()

	lease, err := c.Acquire(ctx, AcquireRequest{TaskID: "TASK-001", Owner: "b1", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := c.Renew(ctx, "TASK-001", lease.Token, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if renewed == nil {
		t.Fatal("renewing a live lease must return it")
	}
	if renewed.ExpiresAt == lease.ExpiresAt {
		t.Fatal("renew must move the expiry")
	}

	// Renewing something that was never held is a normal outcome, not an error.
	gone, err := c.Renew(ctx, "TASK-404", "whatever", time.Minute)
	if err != nil {
		t.Fatalf("renewing an unheld task should not error: %v", err)
	}
	if gone != nil {
		t.Fatal("there was no lease to renew")
	}
}

// TestUnexpiringLeaseIsRefused: a lease that cannot expire is a task that stays
// locked forever when its holder dies.
func TestUnexpiringLeaseIsRefused(t *testing.T) {
	c := client(t)
	_, err := c.Acquire(context.Background(), AcquireRequest{TaskID: "T", Owner: "o"})
	if err == nil || !strings.Contains(err.Error(), "TTL") {
		t.Fatalf("error = %v, want a refusal to issue a lease with no TTL", err)
	}
}

// TestUnreachableStoreFailsClosed is the rule the Rust port paid for once
// already: unknown must never be reported as a healthy empty result.
func TestUnreachableStoreFailsClosed(t *testing.T) {
	ctx := context.Background()
	broken := New(filepath.Join(t.TempDir(), "does-not-exist"), filepath.Join(t.TempDir(), "s.sqlite"))

	ok, err := broken.Valid(ctx, "TASK-001", "token")
	if err == nil {
		t.Fatal("an unreachable store must report an error")
	}
	if ok {
		t.Fatal("an unreachable store must never validate a lease")
	}
	if err := broken.Available(ctx); err == nil {
		t.Fatal("Available must fail when the binary is missing")
	}
}

func TestMisconfigurationIsNamed(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]*Client{
		"no binary":   {DB: "/tmp/x.sqlite"},
		"no database": {Binary: "/bin/true"},
	} {
		if _, err := c.ActiveLeases(ctx); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// TestGoAndRustAgreeOnTheLeaseVocabulary guards the seam's shared contract:
// the codes Go branches on are the strings Rust emits.
func TestGoAndRustAgreeOnTheLeaseVocabulary(t *testing.T) {
	c := client(t)
	ctx := context.Background()

	lease, err := c.Acquire(ctx, AcquireRequest{TaskID: "T1", Owner: "o", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	code, _, _, err := c.Diagnose(ctx, "T1", lease.Token)
	if err != nil {
		t.Fatal(err)
	}
	// If Rust ever renamed a variant, this is where it surfaces — as a failed
	// comparison against Go's constant, not as a silently unmatched branch.
	switch code {
	case dc.LeaseValid, dc.LeaseExpired, dc.LeaseInvalid, dc.LeaseHeldByOther:
	default:
		t.Fatalf("Rust emitted lease code %q, which Go does not know", code)
	}
}

// TestConcurrentStressUnderHighLoad drives multiple goroutines through the real dcstore
// binary across shared and distinct tasks simultaneously.
func TestConcurrentStressUnderHighLoad(t *testing.T) {
	c := client(t)
	ctx := context.Background()

	const workers = 16
	errCh := make(chan error, workers)

	for i := 0; i < workers; i++ {
		workerID := i
		go func() {
			taskID := fmt.Sprintf("TASK-CONC-%d", workerID%4)
			owner := fmt.Sprintf("worker-%d", workerID)
			lease, err := c.Acquire(ctx, AcquireRequest{
				TaskID: taskID,
				Owner:  owner,
				TTL:    5 * time.Minute,
			})
			if err != nil {
				var conflict *Conflict
				if errors.As(err, &conflict) {
					// Expected contention when racing on shared tasks
					errCh <- nil
					return
				}
				errCh <- fmt.Errorf("worker %d acquire error: %w", workerID, err)
				return
			}

			// Validate
			valid, err := c.Valid(ctx, taskID, lease.Token)
			if err != nil || !valid {
				errCh <- fmt.Errorf("worker %d valid error: %w (valid=%v)", workerID, err, valid)
				return
			}

			// Release
			_, err = c.Release(ctx, taskID, lease.Token)
			if err != nil {
				errCh <- fmt.Errorf("worker %d release error: %w", workerID, err)
				return
			}
			errCh <- nil
		}()
	}

	for i := 0; i < workers; i++ {
		if err := <-errCh; err != nil {
			t.Errorf("concurrent stress failure: %v", err)
		}
	}
}

// TestAvailableNeverManufacturesTheStoreItChecks.
//
// health used to open the path with SQLite's default flags, so a mistyped --db
// was answered by a database this very call created: ok, schema 1, zero active
// leases, from a private file nobody else was using. Two harnesses configured
// with two spellings of one path therefore shared no exclusion at all while
// both reported healthy — which is the precise thing Available's contract says
// must never happen, that unknown is never reported as an empty-but-healthy
// store.
func TestAvailableNeverManufacturesTheStoreItChecks(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	real := filepath.Join(dir, "state.sqlite")
	typo := filepath.Join(dir, "staet.sqlite")

	// A command that does work still creates the store; that is how every cold
	// start begins, and the fix must not break it.
	if _, err := New(binary(t), real).ActiveLeases(ctx); err != nil {
		t.Fatalf("cold start: %v", err)
	}
	if err := New(binary(t), real).Available(ctx); err != nil {
		t.Fatalf("a real store reported unavailable: %v", err)
	}

	if err := New(binary(t), typo).Available(ctx); err == nil {
		t.Fatal("a store that does not exist reported itself healthy")
	}
	if _, err := os.Stat(typo); !os.IsNotExist(err) {
		t.Fatalf("the health check created %s; a mistyped path must not become a store", typo)
	}
}

// TestAvailableRequiresTheExclusionIndexAssertion.
//
// Identity and schema version were asserted; the index that actually enforces
// mutual exclusion was not. It is created with IF NOT EXISTS, which matches on
// name alone, so a database carrying a same-named index with any other
// definition silently turned acquire back into the check-then-insert it is
// meant to backstop — a 24-way race elected two winners while health answered
// ok. A store that cannot assert the index is now unavailable, because the
// alternative is two agents in one working tree.
func TestAvailableRequiresTheExclusionIndexAssertion(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "old-store.sh")
	body := "#!/bin/sh\necho '{\"ok\":true,\"store\":\"dc-store\",\"schema_version\":1,\"active_leases\":0}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	err := New(script, filepath.Join(dir, "state.sqlite")).Available(context.Background())
	if err == nil {
		t.Fatal("a store that never checked its exclusion index passed the availability gate")
	}
	if !strings.Contains(err.Error(), "exclusion index") {
		t.Fatalf("the refusal does not name what is missing: %v", err)
	}
}

// TestReadyTasksReOffersATaskWhoseHolderDied is the liveness half of the TTL.
//
// ready filtered on `status = 'active'` as stored, while the lease listing
// beside it applied lazy expiry. Expiry in this store is observed on read and
// there is no sweeper, so a lease nobody touched kept its stale active row and
// went on hiding its task forever. devcouncil_next_task calls ReadyTasks and
// nothing else, so an agent asking what to work on after a builder crashed was
// told there was nothing.
func TestReadyTasksReOffersATaskWhoseHolderDied(t *testing.T) {
	ctx := context.Background()
	c := client(t)
	if _, err := c.ActiveLeases(ctx); err != nil {
		t.Fatalf("cold start: %v", err)
	}
	seedTask(t, c, "TASK-1")

	ready, err := c.ReadyTasks(ctx)
	if err != nil {
		t.Fatalf("ready: %v", err)
	}
	if len(ready) != 1 || ready[0] != "TASK-1" {
		t.Fatalf("ready before the lease = %v", ready)
	}

	if _, err := c.Acquire(ctx, AcquireRequest{TaskID: "TASK-1", Owner: "builder-1", TTL: time.Second}); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if ready, err = c.ReadyTasks(ctx); err != nil || len(ready) != 0 {
		t.Fatalf("a held task was still offered: %v (%v)", ready, err)
	}

	// The holder is gone and its TTL has passed. Nothing else looks at the row
	// — no ActiveLeases, no Active, no Diagnose — because the whole defect was
	// that ready depended on some other call having done so.
	//
	// Two seconds for a one-second TTL: the store keeps expiry to whole epoch
	// seconds, so a lease minted at X.9 expires at X+1 and is not past it until
	// the clock reads X+2. Sleeping only just past the TTL would make this pass
	// or fail on where the second boundary happened to fall.
	time.Sleep(2200 * time.Millisecond)
	ready, err = c.ReadyTasks(ctx)
	if err != nil {
		t.Fatalf("ready: %v", err)
	}
	if len(ready) != 1 || ready[0] != "TASK-1" {
		t.Fatalf("ready after the lease expired = %v; a crashed builder's task was never re-offered", ready)
	}
}

// seedTask plants the row DevCouncil's planner would have written. Planning is
// not this client's job, so the task arrives through sqlite3 rather than
// through an API this package would otherwise have to grow.
func seedTask(t *testing.T, c *Client, id string) {
	t.Helper()
	sql := fmt.Sprintf(
		"INSERT INTO tasks (id,title,description,planned_files_json,status) VALUES ('%s','t','d','[]','ready');", id)
	cmd := exec.Command(testsupport.Tool(t, "sqlite3"), c.DB, sql)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seeding %s: %v %s", id, err, out)
	}
}
