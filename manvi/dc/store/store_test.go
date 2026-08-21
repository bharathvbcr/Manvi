package store

import (
	"context"
	"errors"
	"fmt"
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
