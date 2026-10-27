//go:build !windows

package codingagent

import (
	"errors"
	"os/exec"
	"testing"
)

// The shape the managed lane's hold depends on: the agent exits 0, but a
// grandchild in its own session (a dev server, an MCP child) keeps the agent's
// stdout open. os/exec then returns ErrWaitDelay from Wait, and close() reports it
// beside Reaped: true on this and every later call. Reaped is what says the agent
// is gone; the error is about the pipe.
func TestCloseReportsAReapedAgentBesideWaitDelayWhenAGrandchildHoldsStdout(t *testing.T) {
	perl, err := exec.LookPath("perl")
	if err != nil {
		t.Skip("no perl to start a grandchild in its own session")
	}
	script := `use POSIX; if (fork() == 0) { POSIX::setsid(); sleep 8; exit 0 } exit 0`
	c, err := startConnection(t.Context(), perl, []string{"-e", script}, t.TempDir(), func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	<-c.done // the agent exits on its own; Wait gives up on the held pipe after WaitDelay
	for i := 0; i < 2; i++ {
		exit, cerr := c.close()
		if !exit.Reaped || exit.Code != 0 {
			t.Fatalf("close #%d: exit = %+v, want the agent reaped with code 0", i+1, exit)
		}
		if !errors.Is(cerr, exec.ErrWaitDelay) {
			t.Fatalf("close #%d: err = %v, want os/exec's ErrWaitDelay beside the reaped agent", i+1, cerr)
		}
	}
}
