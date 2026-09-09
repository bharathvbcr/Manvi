package budget

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

func testLedger(t *testing.T) *Ledger {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "campaign.json"), 400, Prices{1, 2, 100, 50, "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	})
	return l
}
func TestUnknownAndRejectedRetainReservations(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	first, err := l.reserve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.transportResult(first, transport.AttemptResult{State: transport.AttemptUnknown}); err != nil {
		t.Fatal(err)
	}
	second, err := l.reserve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.transportResult(second, transport.AttemptResult{State: transport.AttemptRejected, Status: 503}); err != nil {
		t.Fatal(err)
	}
	if _, err = l.reserve(ctx); err == nil {
		t.Fatal("unknown charges must exhaust conservative reservations")
	}
}
func TestProvenNoSendRefundAndUsageSettlement(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	id, err := l.reserve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.transportResult(id, transport.AttemptResult{State: transport.AttemptNotSent}); err != nil {
		t.Fatal(err)
	}
	id, err = l.reserve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.transportResult(id, transport.AttemptResult{State: transport.AttemptAccepted}); err != nil {
		t.Fatal(err)
	}
	if err = l.settle(id, llm.Usage{InputTokens: 10, OutputTokens: 5, ReasoningTokens: 2}); err != nil {
		t.Fatal(err)
	}
	s := l.Snapshot()
	if s.Attempts[0].ReservedNanoUSD != 0 || s.Attempts[1].ChargedNanoUSD != 24 || s.Attempts[1].ReservedNanoUSD != 0 {
		t.Fatalf("wrong charges %+v", s)
	}
	s.Attempts[1].Usage.InputTokens = 999
	if l.Snapshot().Attempts[1].Usage.InputTokens != 10 {
		t.Fatal("snapshot aliases canonical usage")
	}
}
func TestCancellationAndExclusiveOwnership(t *testing.T) {
	l := testLedger(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.reserve(ctx); err == nil {
		t.Fatal("cancelled attempt admitted")
	}
	if len(l.Snapshot().Attempts) != 0 {
		t.Fatal("cancelled attempt recorded")
	}
	if other, err := Open(l.path, l.data.LimitNanoUSD, l.data.Prices); err == nil {
		other.Close()
		t.Fatal("two processes could own one campaign")
	}
}
func TestMissingOrOversizeUsageKeepsReservation(t *testing.T) {
	l := testLedger(t)
	id, err := l.reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []llm.Usage{{}, {InputTokens: 101}, {InputTokens: 1, OutputTokens: 40, ReasoningTokens: 11}, {InputTokens: 1, OutputTokens: -1}} {
		if err = l.settle(id, u); err == nil {
			t.Fatalf("accepted invalid usage %+v", u)
		}
	}
	if l.Snapshot().Attempts[0].ReservedNanoUSD != 200 {
		t.Fatal("unreconciled usage refunded")
	}
}
func TestReservationsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "campaign.json")
	p := Prices{1, 2, 100, 50, "test"}
	l, err := Open(path, 200, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.reserve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	l, err = Open(path, 200, p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err = l.reserve(context.Background()); err == nil {
		t.Fatal("restart erased unresolved reservation")
	}
}
