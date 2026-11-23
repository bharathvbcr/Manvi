package budget

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

func advOwnStart(t *testing.T) int64 {
	t.Helper()
	start, ok := processStartUnix(os.Getpid())
	if !ok {
		t.Skip("this platform reports no process start time")
	}
	return start
}

// setStarted rewrites the recorded start of attempt id, which is what a wall
// clock that stepped between this process's start and its reservation leaves.
func advSetStarted(t *testing.T, l *Ledger, id uint64, unix int64) {
	t.Helper()
	if err := l.update(context.Background(), func(s *Snapshot) error {
		i := s.find(id)
		if i < 0 {
			t.Fatalf("attempt %d is not in flight", id)
		}
		s.InFlight[i].StartedUnix = unix
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAdvB31OpsOwnProcessStartTimeIsSane(t *testing.T) {
	start := advOwnStart(t)
	now := time.Now().Unix()
	if start > now+1 || start < now-3600 {
		t.Fatalf("processStartUnix(self) = %d, now = %d: not a plausible start for a test binary", start, now)
	}
}

// A record written by this very process must never be judged "predates this
// process". Skews of a few seconds, the slack boundary, and a clock stepped back
// by an hour.
func TestAdvB31OpsOwnRecordSurvivesClockSkew(t *testing.T) {
	start := advOwnStart(t)
	for _, c := range []struct {
		name string
		skew int64
	}{
		{"same second", 0},
		{"3s", 3},
		{"5s slack boundary", 5},
		{"10s", 10},
		{"1h step back", 3600},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := testLedger(t, 1_000)
			id, err := l.reserve(context.Background(), "m", 1)
			if err != nil {
				t.Fatal(err)
			}
			advSetStarted(t, l, id, start-c.skew)
			if ownerGone(Attempt{PID: os.Getpid(), StartedUnix: start - c.skew}) {
				t.Errorf("ownerGone is true for this process's own live attempt recorded %ds before the process started", c.skew)
			}
			reclaimed, err := l.Reconcile(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(reclaimed) != 0 {
				t.Errorf("own live attempt reclaimed with skew %ds: %+v", c.skew, reclaimed)
			}
		})
	}
}

// The consequence of the above: the live attempt then cannot be settled, and the
// request that holds it errors although the provider answered.
func TestAdvB31OpsOwnAttemptStillSettlesAfterBackwardClockStep(t *testing.T) {
	start := advOwnStart(t)
	l := testLedger(t, 1_000)
	id, err := l.reserve(context.Background(), "m", 10)
	if err != nil {
		t.Fatal(err)
	}
	advSetStarted(t, l, id, start-3600)
	// A sibling goroutine of this process admits another attempt, which reclaims.
	if _, err := l.reserve(context.Background(), "m", 1); err != nil {
		t.Fatal(err)
	}
	if err := l.transportResult(context.Background(), id, transport.AttemptResult{State: transport.AttemptAccepted}, false); err != nil {
		t.Fatalf("a live attempt could not record its transport result: %v", err)
	}
	if err := l.settle(context.Background(), id, 4); err != nil {
		t.Fatalf("a live attempt could not settle: %v", err)
	}
	s := snapshot(t, l)
	if s.SpentNanoUSD != 4 || s.RetainedNanoUSD != 0 {
		t.Fatalf("spend = %d retained = %d, want 4 and 0: %+v", s.SpentNanoUSD, s.RetainedNanoUSD, s)
	}
}

// A clock stepped forward leaves records in the future: never reclaimed.
func TestAdvB31OpsFutureDatedOwnRecordIsKept(t *testing.T) {
	advOwnStart(t)
	l := testLedger(t, 1_000)
	id, err := l.reserve(context.Background(), "m", 1)
	if err != nil {
		t.Fatal(err)
	}
	advSetStarted(t, l, id, time.Now().Unix()+3600)
	if r, err := l.Reconcile(context.Background()); err != nil || len(r) != 0 {
		t.Fatalf("reclaimed %+v, %v", r, err)
	}
}

// An unreadable start time says nothing: the owner stays alive.
func TestAdvB31OpsUnreadableStartTimeKeepsOwner(t *testing.T) {
	a := Attempt{PID: os.Getpid(), StartedUnix: 100}
	if startedAfter(a, func(int) (int64, bool) { return 1 << 40, false }) {
		t.Fatal("an unreadable start time condemned the owner")
	}
	if startedAfter(Attempt{PID: 1, StartedUnix: 0}, func(int) (int64, bool) { return 1 << 40, true }) {
		t.Fatal("a record with no time condemned the owner")
	}
	if startedAfter(Attempt{PID: 1, StartedUnix: -5}, func(int) (int64, bool) { return 1 << 40, true }) {
		t.Fatal("a record with a negative time condemned the owner")
	}
}

// pid 1 belongs to another user (root): kill(pid, 0) says EPERM and the process
// table must still be readable. Neither may condemn it.
func TestAdvB31OpsAnotherUsersProcessIsAliveAndReadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: pid 1 is not another user's")
	}
	if processGone(1) {
		t.Fatal("processGone(1) = true for a live process of another user")
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		start, ok := processStartUnix(1)
		if !ok {
			t.Logf("start time of another user's process is unreadable here (treated as alive: acceptable)")
		} else if start > time.Now().Unix() {
			t.Fatalf("pid 1 start %d is in the future", start)
		}
	}
	if ownerGone(Attempt{PID: 1, StartedUnix: time.Now().Unix()}) {
		t.Fatal("ownerGone(pid 1, now) = true")
	}
}

// A zombie (exited, not yet reaped) has a pid that kill(pid, 0) still accepts,
// so its reservations keep their in-flight slots until the parent reaps it.
func TestAdvB31OpsZombieOwnerIsReclaimed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	// #nosec G204 G702 -- re-execs this test binary as the ledger helper.
	cmd := exec.CommandContext(t.Context(), os.Args[0])
	cmd.Env = append(os.Environ(), "BUDGET_HELPER_LEDGER="+path, "BUDGET_HELPER_COUNT=2")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	_, _ = io.Copy(io.Discard, stdout) // EOF: the child closed its stdout, i.e. exited
	time.Sleep(200 * time.Millisecond) // let it become a zombie; we have not Wait()ed
	l, err := Open(context.Background(), path, 50)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, l); len(got.InFlight) != 2 {
		t.Fatalf("setup: %+v", got)
	}
	reclaimed, err := l.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 2 {
		t.Fatalf("an exited-but-unreaped owner's 2 attempts were not reclaimed (reclaimed %d); pid %d still probes as alive", len(reclaimed), cmd.Process.Pid)
	}
}

// Two handles (two reclaimers) and many goroutines racing over one ledger full
// of dead attempts: every dead reservation is retained exactly once and the
// committed total only grows by what is newly reserved.
func TestAdvB31OpsConcurrentReclaimersRetainEachDeadAttemptOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	dead := exitedPID(t, filepath.Join(t.TempDir(), "scratch.json"), 0)
	const n = 100
	s := Snapshot{SchemaVersion: schemaVersion, NextID: n + 1, InFlight: []Attempt{}}
	for i := 1; i <= n; i++ {
		s.InFlight = append(s.InFlight, Attempt{ID: uint64(i), Model: "m", State: stateReserved, ReservedNanoUSD: 3, PID: dead})
	}
	if err := save(path, s); err != nil {
		t.Fatal(err)
	}
	a, err := Open(context.Background(), path, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(context.Background(), path, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	reclaimedTotal := 0
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := a
			if i%2 == 1 {
				l = b
			}
			if i%4 < 2 {
				r, err := l.Reconcile(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				reclaimedTotal += len(r)
				mu.Unlock()
				return
			}
			if _, err := l.reserve(context.Background(), "m", 1); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got := snapshot(t, a)
	if got.RetainedAttempts != n || got.RetainedNanoUSD != 3*n {
		t.Fatalf("retained attempts %d / %d nano, want %d / %d: %+v", got.RetainedAttempts, got.RetainedNanoUSD, n, 3*n, got)
	}
	live := 0
	for _, x := range got.InFlight {
		if x.PID == dead {
			t.Fatalf("a dead attempt is still in flight: %+v", x)
		}
		live++
	}
	if live != 8 {
		t.Fatalf("%d live reservations, want the 8 reserving goroutines'", live)
	}
	if want := int64(3*n + 8); got.CommittedNanoUSD() != want {
		t.Fatalf("committed %d, want %d", got.CommittedNanoUSD(), want)
	}
}
