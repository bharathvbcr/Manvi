package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// Activate reads the saved run and writes the running receipt: two round trips
// to the task store. Follow, which the serve read loop answers inline, must not
// wait behind them, or a store that is slow holds up hello, policy checks and a
// queued stop for as long as it takes.

// heldStore holds one workbench method until released, and says when a call
// to it has arrived.
type heldStore struct {
	managedStore
	method   string
	entered  chan struct{}
	release  chan struct{}
	enterOne sync.Once
}

func (h *heldStore) Workbench(ctx context.Context, method string, input json.RawMessage) (json.RawMessage, error) {
	if method == h.method {
		h.enterOne.Do(func() { close(h.entered) })
		select {
		case <-h.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return h.managedStore.Workbench(ctx, method, input)
}

// answersWithin runs call and fails the test if it has not returned in d.
func answersWithin(t *testing.T, what string, d time.Duration, call func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); call() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Errorf("%s was not answered within %v while Activate waited on the store", what, d)
	}
}

func TestActivateDoesNotHoldTheJobAcrossStoreCalls(t *testing.T) {
	var m *managedLifecycle
	m = newManagedLifecycle(t, func(ctx context.Context, o codingagent.Options) *managedTestSession {
		s := m.fixtureSession(ctx, o)
		s.leaveOpen = true
		return s
	})
	prepared, err := m.runner.Prepare(t.Context(), json.RawMessage(`{"id":"run","request_id":"launch","expected_revision":1}`))
	if err != nil {
		t.Fatal(err)
	}
	held := &heldStore{managedStore: m.runner.client, method: "runs.started", entered: make(chan struct{}), release: make(chan struct{})}
	m.runner.client = held
	var releaseOnce sync.Once
	releaseStore := func() { releaseOnce.Do(func() { close(held.release) }) }
	t.Cleanup(releaseStore)

	activation := json.RawMessage(fmt.Sprintf(`{"id":"run","owner_id":%q,"session_id":%q,"process_id":123,"process_start":"fixture-native-observation"}`, prepared.Owner, prepared.Session))
	type outcome struct {
		result json.RawMessage
		err    error
	}
	first, retry := make(chan outcome, 1), make(chan outcome, 1)
	go func() {
		r, err := m.runner.Activate(t.Context(), activation)
		first <- outcome{r, err}
	}()
	select {
	case <-held.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("Activate never reached the store")
	}
	// A host that lost the reply asks again: it waits for the first and gets its
	// receipt, as it always has.
	go func() {
		r, err := m.runner.Activate(t.Context(), activation)
		retry <- outcome{r, err}
	}()

	answersWithin(t, "a follow-up status read", 2*time.Second, func() { _, _ = m.runner.Follow(json.RawMessage(`{"id":"run"}`)) })
	answersWithin(t, "a queued follow-up", 2*time.Second, func() {
		res, err := m.runner.Follow(json.RawMessage(`{"id":"run","message":"next"}`))
		if err != nil || res.State != "queued" {
			t.Errorf("Follow = %+v, %v, want the message queued", res, err)
		}
	})

	releaseStore()
	a, b := <-first, <-retry
	if a.err != nil || b.err != nil {
		t.Fatalf("Activate = %v, retry = %v", a.err, b.err)
	}
	if string(a.result) != string(b.result) {
		t.Fatalf("the retry did not get the first call's receipt:\n%s\n%s", a.result, b.result)
	}
}
