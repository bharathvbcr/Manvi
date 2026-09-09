package budget

import (
	"context"
	"encoding/json"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestOpenRejectsIncoherentSavedAttempts(t *testing.T) {
	prices := Prices{1, 2, 100, 50, "test"}
	for name, attempts := range map[string][]Attempt{
		"id_gap":              {{ID: 2, State: "reserved", ReservedNanoUSD: 200}},
		"missing_reservation": {{ID: 1, State: "unknown"}},
		"unknown_state":       {{ID: 1, State: "invented", ReservedNanoUSD: 200}},
		"overflow":            {{ID: 1, State: "reserved", ReservedNanoUSD: math.MaxInt64}, {ID: 2, State: "reserved", ReservedNanoUSD: math.MaxInt64}},
		"forged_charge":       {{ID: 1, State: "settled", ChargedNanoUSD: 1, Usage: &llm.Usage{InputTokens: 100, OutputTokens: 50}}},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "campaign.json")
			raw, err := json.Marshal(Snapshot{1, 400, prices, attempts})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			l, err := Open(path, 400, prices)
			if err == nil {
				l.Close()
				t.Fatal("invalid saved ledger accepted")
			}
		})
	}
}

type signaledContext struct {
	context.Context
	checked chan struct{}
	once    sync.Once
}

func (c *signaledContext) Err() error {
	err := c.Context.Err()
	c.once.Do(func() { close(c.checked) })
	return err
}
func TestCancellationWhileWaitingForAdmissionDoesNotReserve(t *testing.T) {
	l := testLedger(t)
	base, cancel := context.WithCancel(context.Background())
	ctx := &signaledContext{Context: base, checked: make(chan struct{})}
	l.mu.Lock()
	returned := make(chan error, 1)
	go func() { _, err := l.reserve(ctx); returned <- err }()
	<-ctx.checked
	cancel()
	l.mu.Unlock()
	if err := <-returned; err == nil {
		t.Fatal("cancelled waiter reserved an attempt")
	}
	if len(l.Snapshot().Attempts) != 0 {
		t.Fatal("cancelled waiter changed ledger")
	}
}
func TestClosedLedgerCannotSettleAfterReleasingCampaignOwnership(t *testing.T) {
	l := testLedger(t)
	id, err := l.reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.settle(id, llm.Usage{InputTokens: 1}); err == nil {
		t.Fatal("closed ledger wrote after exclusive lock release")
	}
}

func TestFailedPersistenceCannotReleaseReservationInMemory(t *testing.T) {
	l := testLedger(t)
	id, err := l.reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := l.transportResult(id, transport.AttemptResult{State: transport.AttemptAccepted}); err != nil {
		t.Fatal(err)
	}
	original := l.path
	l.path = filepath.Join(t.TempDir(), "missing-directory", "ledger.json")
	err = l.settle(id, llm.Usage{InputTokens: 1})
	l.path = original
	if err == nil {
		t.Fatal("failed save returned success")
	}
	attempt := l.Snapshot().Attempts[0]
	if attempt.ReservedNanoUSD != 200 || attempt.State != "accepted" || attempt.Usage != nil {
		t.Fatalf("failed persistence released a reservation: %+v", attempt)
	}
}
func TestSettlementRequiresAcceptedSingleUseAttempt(t *testing.T) {
	l := testLedger(t)
	id, err := l.reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := l.settle(id, llm.Usage{InputTokens: 1}); err == nil {
		t.Fatal("unaccepted request settled")
	}
	if err := l.transportResult(id, transport.AttemptResult{State: transport.AttemptNotSent}); err != nil {
		t.Fatal(err)
	}
	if err := l.settle(id, llm.Usage{InputTokens: 1}); err == nil {
		t.Fatal("unsent request settled")
	}
	if err := l.transportResult(id, transport.AttemptResult{State: transport.AttemptAccepted}); err == nil {
		t.Fatal("transport result rewritten")
	}
	if err := l.transportResult(99, transport.AttemptResult{State: transport.AttemptAccepted}); err == nil {
		t.Fatal("unknown attempt accepted")
	}
}
