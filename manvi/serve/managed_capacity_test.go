package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// capacityRunner is a runner whose store holds no runs, so any preparation
// that gets past the capacity check fails on the lookup — which is how the
// test tells "refused for capacity" from "admitted, then refused for cause".
func capacityRunner(t *testing.T) *ManagedRunner {
	t.Helper()
	client := store.New(testsupport.DCStore(t), filepath.Join(t.TempDir(), "profile.sqlite"))
	t.Cleanup(func() { client.Close() })
	runner, err := NewManagedRunner(client, func(context.Context, codingagent.Options) (ManagedSession, error) {
		t.Error("a capacity probe must never start a provider")
		return nil, fmt.Errorf("no provider in a capacity probe")
	}, func(err error) string { return err.Error() })
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

// occupy installs n jobs that are still running, and n more that ended.
func occupy(runner *ManagedRunner, n int) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for i := range n {
		live := &managedJob{id: fmt.Sprintf("live-%d", i), done: make(chan struct{}), ready: make(chan struct{})}
		runner.jobs[live.id] = live
		ended := &managedJob{id: fmt.Sprintf("ended-%d", i), done: make(chan struct{}), ready: make(chan struct{})}
		close(ended.done)
		runner.jobs[ended.id] = ended
	}
}

func managedPrepareInput(id string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"id":%q,"request_id":"launch-%s","expected_revision":1}`, id, id))
}

func refusedForCapacity(err error) bool {
	return err != nil && strings.Contains(err.Error(), "managed sessions are already active")
}

// The runner refuses only at its ceiling — never at two, the number it used
// to hard-code below what the store and the host had both admitted — and an
// ended session never holds a place.
func TestManagedRunnerAdmitsUpToItsCeilingAndNoFurther(t *testing.T) {
	if MaxManagedSessions != 64 {
		t.Fatalf("MaxManagedSessions = %d; it must equal dc-store's MAX_ACTIVE_RUNS_CEILING (64)", MaxManagedSessions)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	below := capacityRunner(t)
	occupy(below, MaxManagedSessions-1)
	if _, err := below.Prepare(ctx, managedPrepareInput("next")); err == nil || refusedForCapacity(err) {
		t.Fatalf("one below the ceiling must be admitted to the store lookup, got %v", err)
	}
	// Two and three live sessions are far below the ceiling.
	for _, live := range []int{2, 3} {
		runner := capacityRunner(t)
		occupy(runner, live)
		if _, err := runner.Prepare(ctx, managedPrepareInput("next")); refusedForCapacity(err) {
			t.Fatalf("%d live sessions must not refuse another: %v", live, err)
		}
	}

	full := capacityRunner(t)
	occupy(full, MaxManagedSessions)
	_, err := full.Prepare(ctx, managedPrepareInput("over"))
	if !refusedForCapacity(err) || !strings.Contains(err.Error(), fmt.Sprint(MaxManagedSessions)) {
		t.Fatalf("the ceiling must refuse and name itself, got %v", err)
	}
	full.mu.Lock()
	_, created := full.jobs["over"]
	ended := 0
	for id := range full.jobs {
		if strings.HasPrefix(id, "ended-") {
			ended++
		}
	}
	full.mu.Unlock()
	if created {
		t.Fatal("a refused preparation must not leave a job behind")
	}
	if ended != 0 {
		t.Fatalf("ended sessions must be swept before counting, %d remain", ended)
	}
}

// The owner a managed attempt records names this process and when it
// claimed, so a host can prove the owner gone after a crash. It must also
// stay within the store's opaque-id grammar (alphanumerics, '-', '_', at
// most 128) and the host's (at most 100), and stay unique per attempt.
func TestManagedOwnerNamesTheManviProcessThatClaimed(t *testing.T) {
	before := time.Now().UnixNano()
	seen := map[string]bool{}
	for range 64 {
		owner, err := managedOwnerID()
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(owner, "-")
		if len(parts) != 4 || parts[0] != "manvi" || parts[1] != fmt.Sprint(os.Getpid()) {
			t.Fatalf("owner %q does not name this process", owner)
		}
		var stamp int64
		if _, err := fmt.Sscan(parts[2], &stamp); err != nil || stamp < before || stamp > time.Now().UnixNano() {
			t.Fatalf("owner %q does not carry the claim time", owner)
		}
		if len(owner) > 100 || strings.Trim(owner, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
			t.Fatalf("owner %q is outside the id grammar", owner)
		}
		if seen[owner] {
			t.Fatalf("owner %q repeated", owner)
		}
		seen[owner] = true
	}
}

// Many hosts asking at once against a full runner: every one is refused for
// capacity and none starts a job, because the count and the insert happen
// under one lock.
func TestManagedRunnerCapacityHoldsUnderConcurrentPreparations(t *testing.T) {
	runner := capacityRunner(t)
	occupy(runner, MaxManagedSessions)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, 32)
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = runner.Prepare(ctx, managedPrepareInput(fmt.Sprintf("racer-%d", i)))
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if !refusedForCapacity(err) {
			t.Fatalf("racer %d was not refused for capacity: %v", i, err)
		}
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.jobs) != MaxManagedSessions {
		t.Fatalf("%d jobs after a refused race, want %d", len(runner.jobs), MaxManagedSessions)
	}
}
