package serve

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// Adversarial tests for the w23-managed fix of ft-50d0119f (serve side).

// flakyResolve refuses the first failFirst receipts of decisions.resolve and
// passes the rest, as a store that is briefly busy would.
type flakyResolve struct {
	managedStore
	failFirst atomic.Int32
	calls     atomic.Int32
}

func (f *flakyResolve) Workbench(ctx context.Context, method string, input json.RawMessage) (json.RawMessage, error) {
	if method == "decisions.resolve" {
		f.calls.Add(1)
		if f.failFirst.Add(-1) >= 0 {
			return nil, errors.New("injected: the store refused the receipt")
		}
	}
	return f.managedStore.Workbench(ctx, method, input)
}

func advW23Rows(m *managedLifecycle) map[string]int {
	out := map[string]int{}
	for _, r := range m.decisions() {
		out[r.State]++
	}
	return out
}

// Attack 5: the claim succeeded, then the write found the callback withdrawn.
// The row ends cancelled (legal from a claimed row), a duplicate withdrawal of
// the same callback and extra wakes change nothing, and the run completes.
func TestAdvW23ClaimedThenWithdrawnRowIsCancelledOnceAndDuplicatesAreInert(t *testing.T) {
	var m *managedLifecycle
	m = newManagedLifecycle(t, func(ctx context.Context, o codingagent.Options) *managedTestSession {
		s := m.fixtureSession(ctx, o)
		s.expires, s.completeAfter = time.Minute, 2500*time.Millisecond
		s.respondErr = codingagent.ErrCallbackWithdrawn
		return s
	})
	m.start("launch")
	row := m.awaitDecisions(1)[0]
	m.allow(row)
	deadline := time.Now().Add(5 * time.Second)
	for advW23Rows(m)["cancelled"] != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	m.session.events <- codingagent.Event{CancelledID: row.Request}
	m.session.events <- codingagent.Event{CancelledID: row.Request}
	for i := 0; i < 3; i++ {
		if err := m.runner.Wake(nil); err != nil {
			t.Fatal(err)
		}
	}
	run, raw := m.finished()
	if run.ProviderState != "completed" || m.session.replies.Load() != 0 {
		t.Fatalf("provider_state=%s replies=%d (%.300s)", run.ProviderState, m.session.replies.Load(), raw[strings.Index(string(raw), `"reason"`):])
	}
	if rows := advW23Rows(m); rows["cancelled"] != 1 || len(m.decisions()) != 1 {
		t.Fatalf("rows = %v", rows)
	}
}

// Attack 6: a receipt that cannot be written for a provider-confirmed callback
// ends the run with the error, the provider is closed, and the row the loop left
// in job.callbacks still gets its terminal receipt from the final settle.
func TestAdvW23ResolveFailureEndsTheRunClosesTheProviderAndSettlesTheRow(t *testing.T) {
	var m *managedLifecycle
	m = newManagedLifecycle(t, func(ctx context.Context, o codingagent.Options) *managedTestSession {
		s := m.fixtureSession(ctx, o)
		s.expires = time.Minute
		return s
	})
	flaky := &flakyResolve{managedStore: m.runner.client}
	flaky.failFirst.Store(1)
	m.runner.client = flaky
	m.start("launch")
	row := m.awaitDecisions(1)[0]
	m.session.events <- codingagent.Event{ResolvedID: row.Request}
	m.session.events <- codingagent.Event{Text: "must not be reached", Status: "completed"}
	run, raw := m.finished()
	if run.ProviderState != "failed" {
		t.Fatalf("provider_state=%s, want failed (%.300s)", run.ProviderState, raw)
	}
	if m.session.closes.Load() != 1 {
		t.Fatalf("provider closed %d times, want once", m.session.closes.Load())
	}
	if rows := advW23Rows(m); rows["pending"]+rows["decided"]+rows["dispatching"] != 0 {
		t.Fatalf("a row was left live after the run ended: %v", rows)
	}
}

// The same when the store never takes the receipt: the run still ends, failed,
// the provider is still closed, and nothing hangs.
func TestAdvW23ResolveNeverSucceedingStillEndsAndClosesTheRun(t *testing.T) {
	var m *managedLifecycle
	m = newManagedLifecycle(t, func(ctx context.Context, o codingagent.Options) *managedTestSession {
		s := m.fixtureSession(ctx, o)
		s.expires = time.Minute
		return s
	})
	errReceipt := errors.New("decision store refused every receipt")
	m.runner.client = failingResolve{managedStore: m.runner.client, err: errReceipt}
	m.start("launch")
	row := m.awaitDecisions(1)[0]
	m.session.events <- codingagent.Event{CancelledID: row.Request}
	run, raw := m.finished()
	if run.ProviderState != "failed" || !strings.Contains(string(raw), errReceipt.Error()) {
		t.Fatalf("provider_state=%s (%.300s)", run.ProviderState, raw)
	}
	if m.session.closes.Load() != 1 {
		t.Fatalf("provider closed %d times, want once", m.session.closes.Load())
	}
}
