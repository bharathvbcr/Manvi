package serve

import (
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// ManagedRunner.Close takes its deadline from codingagent's own close budget, so it
// cannot give up while a connection is still inside the time its close may take.
func TestManagedCloseWaitsAtLeastCodingagentsCloseBudget(t *testing.T) {
	if got, floor := managedCloseWait(), codingagent.CloseBudget(); got <= floor {
		t.Fatalf("Close waits %v, which is not beyond codingagent's close budget %v", got, floor)
	}
}

// A provider Close of 6.5 s, which the old five-second wait reported as unresolved,
// is inside codingagent's close budget: Close waits for it and reports nothing.
func TestShutdownWaitsOutAProviderCloseWithinTheBudget(t *testing.T) {
	root := scanRepo(t, map[string]string{"CLAUDE.md": "project rules\n"})
	var session *managedTestSession
	m := unconfirmedLifecycle(t, root, &session, func(s *managedTestSession) {
		s.closeDelay = 6500 * time.Millisecond
	})
	prepared, err := m.runner.Prepare(t.Context(), preparing("launch", ""))
	if err != nil {
		t.Fatal(err)
	}
	activate(t, m, prepared)
	if err := m.runner.Close(); err != nil {
		t.Fatalf("Close = %v, want the run reaped inside the budget", err)
	}
}
