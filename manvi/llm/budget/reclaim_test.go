package budget

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

// exitedPID starts a worker that reserves count attempts in the ledger at
// path and exits without resolving them, which is what a crashed session
// leaves behind. It returns the dead worker's pid.
func exitedPID(t *testing.T, path string, count int) int {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "BUDGET_HELPER_LEDGER="+path, "BUDGET_HELPER_COUNT="+strconv.Itoa(count))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worker: %v: %s", err, out)
	}
	return cmd.Process.Pid
}

func TestReserveReclaimsAttemptsOfAnExitedProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	dead := exitedPID(t, path, 3)

	l, err := Open(path, 50)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, l)
	if len(before.InFlight) != 3 {
		t.Fatalf("setup: %d in flight, want the worker's 3", len(before.InFlight))
	}
	if _, err := l.reserve(context.Background(), "m", 1); err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, l)
	for _, a := range after.InFlight {
		if a.PID == dead {
			t.Fatalf("attempt #%d of exited pid %d is still in flight: %+v", a.ID, dead, after.InFlight)
		}
	}
	if after.RetainedAttempts != 3 || after.RetainedNanoUSD != 3 {
		t.Fatalf("a dead process's reservations must be retained as spend, not refunded: %+v", after)
	}
	if got, want := after.CommittedNanoUSD(), before.CommittedNanoUSD()+1; got != want {
		t.Fatalf("committed = %d, want %d: reclaiming may move a reservation, never release it", got, want)
	}
}

// TestALedgerFullOfDeadAttemptsStillAdmits is the failure reclaiming exists
// for: maxInFlight attempts left by crashed sessions used to refuse every later
// reservation for good.
func TestALedgerFullOfDeadAttemptsStillAdmits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	dead := exitedPID(t, filepath.Join(t.TempDir(), "scratch.json"), 0)
	s := Snapshot{SchemaVersion: schemaVersion, NextID: maxInFlight + 1, InFlight: []Attempt{}}
	for i := 1; i <= maxInFlight; i++ {
		s.InFlight = append(s.InFlight, Attempt{ID: uint64(i), Model: "m", State: stateAccepted, ReservedNanoUSD: 1, PID: dead})
	}
	if err := save(path, s); err != nil {
		t.Fatal(err)
	}
	l, err := Open(path, 1_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.reserve(context.Background(), "m", 1); err != nil {
		t.Fatalf("a ledger whose in-flight list is all dead attempts refused a reservation: %v", err)
	}
	if got := snapshot(t, l); len(got.InFlight) != 1 || got.RetainedAttempts != maxInFlight {
		t.Fatalf("in flight %d, retained %d; want 1 and %d", len(got.InFlight), got.RetainedAttempts, maxInFlight)
	}
}

// TestReclaimLeavesLiveOwnersAlone holds an attempt in a process that is still
// running. Its reservation belongs to code that will still settle it.
func TestReclaimLeavesLiveOwnersAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "BUDGET_HELPER_LEDGER="+path, "BUDGET_HELPER_COUNT=1", "BUDGET_HELPER_HOLD=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); _ = cmd.Wait() })
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || strings.TrimSpace(line) != "held" {
		t.Fatalf("worker did not report its reservation: %q %v", line, err)
	}

	l, err := Open(path, 50)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.reserve(context.Background(), "m", 1); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := l.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 0 {
		t.Fatalf("reclaimed a live process's attempts: %+v", reclaimed)
	}
	s := snapshot(t, l)
	owners := map[int]bool{}
	for _, a := range s.InFlight {
		owners[a.PID] = true
	}
	if !owners[cmd.Process.Pid] || !owners[os.Getpid()] || s.RetainedAttempts != 0 {
		t.Fatalf("live attempts must stay in flight: %+v", s)
	}
}

// TestReclaimTreatsANonPositivePIDAsGone checks the guard that keeps a corrupt
// pid from being signalled: kill(0, …) addresses the caller's process group.
func TestReclaimTreatsANonPositivePIDAsGone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	s := Snapshot{SchemaVersion: schemaVersion, NextID: 3, InFlight: []Attempt{
		{ID: 1, Model: "m", State: stateReserved, ReservedNanoUSD: 5, PID: 0},
		{ID: 2, Model: "m", State: stateReserved, ReservedNanoUSD: 7, PID: -1},
	}}
	if err := save(path, s); err != nil {
		t.Fatal(err)
	}
	l, err := Open(path, 1_000)
	if err != nil {
		t.Fatal(err)
	}
	reclaimed, err := l.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 2 {
		t.Fatalf("reclaimed %d, want both ownerless attempts", len(reclaimed))
	}
	if got := snapshot(t, l); got.RetainedNanoUSD != 12 || len(got.InFlight) != 0 {
		t.Fatalf("ownerless attempts must be retained at their reservation: %+v", got)
	}
}

// finishingStream finishes, and the caller's context is cancelled while its
// response is being read — the moment a fan-out is torn down just after a
// child answered.
type finishingStream struct {
	cancel context.CancelFunc
	usage  llm.Usage
}

func (s *finishingStream) Next() (llm.Chunk, error) { return llm.Chunk{}, nil }
func (s *finishingStream) Close() error             { return nil }
func (s *finishingStream) Response() (llm.Response, error) {
	s.cancel()
	return llm.Response{Usage: s.usage}, nil
}

func TestCancellationAfterTheResponseFinishedSettlesItsUsage(t *testing.T) {
	l := testLedger(t, 1_000_000_000)
	id := accepted(t, l, 900_000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	usage := llm.Usage{InputTokens: 1_000, CacheReadTokens: 800, OutputTokens: 50}
	want, err := tiered.Charge(usage)
	if err != nil {
		t.Fatal(err)
	}
	s := &budgetStream{ctx: ctx, inner: &finishingStream{cancel: cancel, usage: usage}, prices: tiered, outputBound: 1_000, ledger: l, id: id}

	if _, err := s.Response(); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the cancellation reported", err)
	}
	got := snapshot(t, l)
	if got.Settled != 1 || got.SpentNanoUSD != want || got.RetainedAttempts != 0 || len(got.InFlight) != 0 {
		t.Fatalf("a finished response was not settled at its usage (want spent %d): %+v", want, got)
	}
}

// BenchmarkContendedReserveSettle measures one attempt's full ledger cost —
// reserve, accept, settle — with 32 goroutines sharing one file, the width of
// the largest fan-out agents.max_fanout allows. Each operation takes the file
// lock and fsyncs twice, so this is the price of metering a wide fan-out.
func BenchmarkContendedReserveSettle(b *testing.B) {
	l, err := Open(filepath.Join(b.TempDir(), "ledger.json"), 1_000_000_000_000_000)
	if err != nil {
		b.Fatal(err)
	}
	b.SetParallelism(max(1, 32/runtime.GOMAXPROCS(0)))
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			id, err := l.reserve(context.Background(), "m", 10)
			if err != nil {
				b.Error(err)
				return
			}
			if err := l.transportResult(id, transport.AttemptResult{State: transport.AttemptAccepted}, false); err != nil {
				b.Error(err)
				return
			}
			if err := l.settle(id, 5); err != nil {
				b.Error(err)
				return
			}
		}
	})
}
