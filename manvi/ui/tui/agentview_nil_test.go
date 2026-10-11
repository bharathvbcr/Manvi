package tui

import "testing"

// App.Current returns nil when no session is active, and callers read through
// it directly, so the accessors on a nil view answer "nothing" rather than
// panicking.
func TestNilAgentViewAccessorsAreInert(t *testing.T) {
	var v *AgentView
	v.SetBusy(true, "x")
	v.SetTitle("x")
	if v.Approval() != nil || v.PendingApprovals() != 0 || v.Queued() != nil {
		t.Fatal("a nil view reported state")
	}
}
