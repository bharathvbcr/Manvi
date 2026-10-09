package budget

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

func testLedger(t *testing.T, limit int64) *Ledger {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "ledger.json"), limit)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func snapshot(t *testing.T, l *Ledger) Snapshot {
	t.Helper()
	s, err := l.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func accepted(t *testing.T, l *Ledger, amount int64) uint64 {
	t.Helper()
	id, err := l.reserve(context.Background(), "m", amount)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.transportResult(id, transport.AttemptResult{State: transport.AttemptAccepted}, false); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUnknownAndRejectedRetainReservations(t *testing.T) {
	l := testLedger(t, 400)
	ctx := context.Background()
	first, err := l.reserve(ctx, "m", 200)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.transportResult(first, transport.AttemptResult{State: transport.AttemptUnknown}, false); err != nil {
		t.Fatal(err)
	}
	second, err := l.reserve(ctx, "m", 200)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.transportResult(second, transport.AttemptResult{State: transport.AttemptRejected, Status: 503}, false); err != nil {
		t.Fatal(err)
	}
	if _, err = l.reserve(ctx, "m", 1); !errors.Is(err, ErrExhausted) {
		t.Fatalf("unknown charges must exhaust conservative reservations, got %v", err)
	}
	s := snapshot(t, l)
	if s.RetainedNanoUSD != 400 || s.RetainedAttempts != 2 || len(s.InFlight) != 0 {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestProvenNoSendRefundsAndUsageSettles(t *testing.T) {
	l := testLedger(t, 400)
	id, err := l.reserve(context.Background(), "m", 200)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.transportResult(id, transport.AttemptResult{State: transport.AttemptNotSent}, false); err != nil {
		t.Fatal(err)
	}
	if s := snapshot(t, l); s.CommittedNanoUSD() != 0 || len(s.InFlight) != 0 {
		t.Fatalf("not-sent attempt kept its reservation: %+v", s)
	}
	id = accepted(t, l, 200)
	if err = l.settle(id, 24); err != nil {
		t.Fatal(err)
	}
	s := snapshot(t, l)
	if s.SpentNanoUSD != 24 || s.Settled != 1 || s.CommittedNanoUSD() != 24 || s.Overruns != 0 {
		t.Fatalf("wrong charges %+v", s)
	}
}

func TestOverrunIsRecordedInFull(t *testing.T) {
	l := testLedger(t, 400)
	id := accepted(t, l, 10)
	if err := l.settle(id, 50); err != nil {
		t.Fatal(err)
	}
	if s := snapshot(t, l); s.SpentNanoUSD != 50 || s.Overruns != 1 {
		t.Fatalf("an overrun must be charged in full and counted: %+v", s)
	}
}

func TestCancelledReserveRecordsNothing(t *testing.T) {
	l := testLedger(t, 400)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.reserve(ctx, "m", 1); err == nil {
		t.Fatal("cancelled attempt admitted")
	}
	if s := snapshot(t, l); len(s.InFlight) != 0 || s.NextID != 1 {
		t.Fatalf("cancelled attempt recorded: %+v", s)
	}
}

func TestCancellationWhileWaitingForTheLockDoesNotReserve(t *testing.T) {
	l := testLedger(t, 400)
	unlock, err := lockFile(context.Background(), l.path+".lock")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() { _, err := l.reserve(ctx, "m", 1); returned <- err }()
	cancel()
	err = <-returned
	unlock()
	if err == nil {
		t.Fatal("cancelled waiter reserved an attempt")
	}
	if s := snapshot(t, l); len(s.InFlight) != 0 {
		t.Fatalf("cancelled waiter changed the ledger: %+v", s)
	}
}

func TestReservationsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	l, err := Open(path, 200)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.reserve(context.Background(), "m", 200); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(path, 200)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.reserve(context.Background(), "m", 1); !errors.Is(err, ErrExhausted) {
		t.Fatalf("restart erased an unresolved reservation: %v", err)
	}
}

func TestHandlesSharingOneFileCannotOverspend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	var handles []*Ledger
	for range 4 {
		l, err := Open(path, 100)
		if err != nil {
			t.Fatal(err)
		}
		handles = append(handles, l)
	}
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := range 60 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := handles[i%len(handles)].reserve(context.Background(), "m", 5); err == nil {
				admitted.Add(1)
			} else if !errors.Is(err, ErrExhausted) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 20 {
		t.Fatalf("admitted %d reservations of 5 under a limit of 100, want exactly 20", admitted.Load())
	}
}

// TestProcessesSharingOneFileCannotOverspend runs the same race across real
// processes, which is the case the per-operation file lock exists for.
func TestProcessesSharingOneFileCannotOverspend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	const workers, each = 4, 15
	var cmds []*exec.Cmd
	for range workers {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "BUDGET_HELPER_LEDGER="+path, "BUDGET_HELPER_COUNT="+strconv.Itoa(each))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.InFlightNanoUSD(); got != 50 || len(s.InFlight) != 50 {
		t.Fatalf("%d processes reserved %d nano-USD over %d attempts under a limit of 50", workers, got, len(s.InFlight))
	}
}

// TestMain doubles as the worker for TestProcessesSharingOneFileCannotOverspend:
// started with BUDGET_HELPER_LEDGER set, the test binary reserves and exits
// instead of running the suite.
func TestMain(m *testing.M) {
	if path := os.Getenv("BUDGET_HELPER_LEDGER"); path != "" {
		os.Exit(reserveWorker(path))
	}
	os.Exit(m.Run())
}

func reserveWorker(path string) int {
	count, err := strconv.Atoi(os.Getenv("BUDGET_HELPER_COUNT"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	l, err := Open(path, 50)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	for range count {
		if _, err := l.reserve(context.Background(), "m", 1); err != nil && !errors.Is(err, ErrExhausted) {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	return 0
}

func TestFileStaysBoundedAsSpendAccumulates(t *testing.T) {
	l := testLedger(t, 1_000_000)
	cycle := func() {
		if err := l.settle(accepted(t, l, 10), 3); err != nil {
			t.Fatal(err)
		}
	}
	cycle()
	info, err := os.Stat(l.path)
	if err != nil {
		t.Fatal(err)
	}
	for range 300 {
		cycle()
	}
	after, err := os.Stat(l.path)
	if err != nil {
		t.Fatal(err)
	}
	s := snapshot(t, l)
	if s.Settled != 301 || s.SpentNanoUSD != 903 {
		t.Fatalf("snapshot = %+v", s)
	}
	if after.Size() > info.Size()+8 {
		t.Fatalf("ledger grew from %d to %d bytes; settled attempts must fold into totals", info.Size(), after.Size())
	}
}

func TestFailedPersistenceKeepsTheReservation(t *testing.T) {
	l := testLedger(t, 400)
	id := accepted(t, l, 200)
	dir := filepath.Dir(l.path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	err := l.settle(id, 1)
	if chmodErr := os.Chmod(dir, 0o700); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	if err == nil {
		t.Fatal("failed save returned success")
	}
	s := snapshot(t, l)
	if len(s.InFlight) != 1 || s.InFlight[0].State != stateAccepted || s.InFlight[0].ReservedNanoUSD != 200 || s.SpentNanoUSD != 0 {
		t.Fatalf("failed persistence released a reservation: %+v", s)
	}
}

func TestSettlementRequiresAcceptedSingleUseAttempt(t *testing.T) {
	l := testLedger(t, 400)
	id, err := l.reserve(context.Background(), "m", 200)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.settle(id, 1); err == nil {
		t.Fatal("unaccepted request settled")
	}
	if err := l.transportResult(id, transport.AttemptResult{State: transport.AttemptNotSent}, false); err != nil {
		t.Fatal(err)
	}
	if err := l.settle(id, 1); err == nil {
		t.Fatal("unsent request settled")
	}
	if err := l.transportResult(id, transport.AttemptResult{State: transport.AttemptAccepted}, false); err == nil {
		t.Fatal("transport result rewritten")
	}
	if err := l.transportResult(99, transport.AttemptResult{State: transport.AttemptAccepted}, false); err == nil {
		t.Fatal("unknown attempt accepted")
	}
	settled := accepted(t, l, 10)
	if err := l.settle(settled, 1); err != nil {
		t.Fatal(err)
	}
	if err := l.settle(settled, 1); err == nil {
		t.Fatal("attempt settled twice")
	}
}

func TestRetainIsIdempotent(t *testing.T) {
	l := testLedger(t, 400)
	id := accepted(t, l, 30)
	for range 3 {
		if err := l.retain(id); err != nil {
			t.Fatal(err)
		}
	}
	if s := snapshot(t, l); s.RetainedNanoUSD != 30 || s.RetainedAttempts != 1 {
		t.Fatalf("retain counted more than once: %+v", s)
	}
}

func TestOpenRejectsIncoherentLedgers(t *testing.T) {
	inflight := func(id uint64, state string, amount int64) string {
		return fmt.Sprintf(`{"id":%d,"model":"m","state":%q,"reserved_nano_usd":%d,"pid":1,"started_unix":1}`, id, state, amount)
	}
	ledger := func(spent int64, next uint64, attempts string) string {
		return fmt.Sprintf(`{"schema_version":2,"spent_nano_usd":%d,"retained_nano_usd":0,"settled":0,"retained_attempts":0,"overruns":0,"next_id":%d,"in_flight":[%s]}`, spent, next, attempts)
	}
	for name, raw := range map[string]string{
		"campaign_v1":      `{"schema_version":1,"limit_nano_usd":400,"prices":{},"attempts":[]}`,
		"id_not_issued":    ledger(0, 2, inflight(2, stateReserved, 5)),
		"duplicate_id":     ledger(0, 3, inflight(1, stateReserved, 5)+","+inflight(1, stateReserved, 5)),
		"invented_state":   ledger(0, 2, inflight(1, "settled", 5)),
		"zero_reservation": ledger(0, 2, inflight(1, stateAccepted, 0)),
		"negative_spend":   ledger(-1, 1, ""),
		"overflow":         ledger(9223372036854775807, 2, inflight(1, stateReserved, 1)),
		"unknown_field":    `{"schema_version":2,"next_id":1,"in_flight":[],"forged":true}`,
		"trailing_data":    ledger(0, 1, "") + `{}`,
		"truncated":        `{"schema_version":2,"next_id":1,`,
		"model_unnamed":    ledger(0, 2, `{"id":1,"model":"","state":"reserved","reserved_nano_usd":5,"pid":1,"started_unix":1}`),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger.json")
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path, 400); err == nil {
				t.Fatal("invalid saved ledger accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != raw {
				t.Fatal("a refused ledger must be left as found, not reset")
			}
		})
	}
}

func TestOpenRefusesRelativePathAndBadLimit(t *testing.T) {
	if _, err := Open("ledger.json", 1); err == nil {
		t.Fatal("relative path accepted")
	}
	for _, limit := range []int64{0, -1} {
		if _, err := Open(filepath.Join(t.TempDir(), "l.json"), limit); err == nil {
			t.Fatalf("limit %d accepted", limit)
		}
	}
}

func TestUSD(t *testing.T) {
	for nano, want := range map[int64]string{
		0: "$0.00", 4_999_999: "$0.00", 5_000_000: "$0.01", 200_000_000_000: "$200.00", 1_234_567_890: "$1.23", -10_000_000: "-$0.01",
	} {
		if got := USD(nano); got != want {
			t.Errorf("USD(%d) = %s, want %s", nano, got, want)
		}
	}
}

func TestAnUnbilledRejectionReleasesItsReservation(t *testing.T) {
	l := testLedger(t, 400)
	id, err := l.reserve(context.Background(), "m", 200)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.transportResult(id, transport.AttemptResult{State: transport.AttemptRejected, Status: 429}, true); err != nil {
		t.Fatal(err)
	}
	s := snapshot(t, l)
	if s.CommittedNanoUSD() != 0 || s.RefusedAttempts != 1 || s.RetainedAttempts != 0 {
		t.Fatalf("an unbilled rejection kept its reservation: %+v", s)
	}
	// The refund applies to rejections only: an unknown outcome is never
	// released on the provider's say-so.
	id, err = l.reserve(context.Background(), "m", 200)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.transportResult(id, transport.AttemptResult{State: transport.AttemptUnknown}, true); err != nil {
		t.Fatal(err)
	}
	if s := snapshot(t, l); s.RetainedNanoUSD != 200 {
		t.Fatalf("an unknown outcome was refunded: %+v", s)
	}
}

func TestRetainAtRecordsTheTighterBound(t *testing.T) {
	l := testLedger(t, 400)
	if err := l.retainAt(accepted(t, l, 200), 30); err != nil {
		t.Fatal(err)
	}
	if err := l.retainAt(accepted(t, l, 10), 25); err != nil {
		t.Fatal(err)
	}
	if s := snapshot(t, l); s.RetainedNanoUSD != 55 || s.RetainedAttempts != 2 {
		t.Fatalf("retained %+v, want the two bounds (30 + 25), not the reservations", s)
	}
}

func TestAWedgedLockHolderCannotHangAnOperation(t *testing.T) {
	l := testLedger(t, 400)
	unlock, err := lockFile(context.Background(), l.path+".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	saved := lockWait
	lockWait = 50 * time.Millisecond
	defer func() { lockWait = saved }()
	if _, err := l.reserve(context.Background(), "m", 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the bounded wait to expire", err)
	}
}
