//go:build unix

package codingagent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// pidAlive says whether pid names a live process; a zombie still counts, as it
// does for kill(2).
func pidAlive(pid int) bool { return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }

// startGrandchildAgent starts perl as the provider with script, which receives
// the path where its grandchild writes its pid, and returns the connection and
// that pid once it is known. The grandchild is killed on cleanup whatever the
// test concluded, so a failing assertion never leaves it running.
func startGrandchildAgent(t *testing.T, script string) (*connection, int) {
	t.Helper()
	perl, err := exec.LookPath("perl")
	if err != nil {
		t.Skip("no perl to start a grandchild")
	}
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	c, err := startConnection(t.Context(), perl, []string{"-e", script, pidFile}, t.TempDir(), func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.close() })
	var pid int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && n > 0 {
			pid = n
			break
		}
	}
	if pid == 0 {
		t.Fatal("the grandchild never reported its pid")
	}
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	return c, pid
}

// A grandchild that ignores SIGTERM and holds the agent's stdout open does not
// survive close(): the agent leads its own process group and cancel kills the
// group while Wait is still pending. This pins that behaviour; it is not the
// case close() used to get wrong (see the test below).
func TestCloseKillsAGrandchildThatIgnoresStdinClosureAndHoldsStdout(t *testing.T) {
	c, grandchild := startGrandchildAgent(t, `
		if (fork() == 0) {
			open(F, ">$ARGV[0]"); print F $$; close F;
			$SIG{TERM} = "IGNORE"; $SIG{HUP} = "IGNORE";
			sleep 30; exit 0;
		}
		sleep 30`)
	exit, err := c.close()
	if !exit.Reaped {
		t.Fatalf("close: exit = %+v, err = %v, want the agent reaped", exit, err)
	}
	for deadline := time.Now().Add(2 * time.Second); pidAlive(grandchild) && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	if pidAlive(grandchild) {
		t.Fatalf("grandchild %d survived close()", grandchild)
	}
}

// The shape cmd.Cancel cannot reach: the agent exits, a grandchild in its group
// has redirected stdout and stderr away, so Wait has already returned and there
// is nothing left for cancel to interrupt. close() used to report the agent
// reaped with a nil error while the grandchild, which can still write the
// checkout, ran on.
func TestCloseKillsAGrandchildThatRedirectedItsStdioAfterTheAgentExited(t *testing.T) {
	c, grandchild := startGrandchildAgent(t, `
		if (fork() == 0) {
			open(STDOUT, ">/dev/null"); open(STDERR, ">/dev/null");
			open(F, ">$ARGV[0]"); print F $$; close F;
			sleep 30; exit 0;
		}
		exit 0`)
	<-c.done
	if !pidAlive(grandchild) {
		t.Fatalf("grandchild %d is already gone before close(); the scenario was not set up", grandchild)
	}
	exit, err := c.close()
	if !exit.Reaped || exit.Code != 0 || err != nil {
		t.Fatalf("close: exit = %+v, err = %v, want the agent reaped with code 0 and no error", exit, err)
	}
	if pidAlive(grandchild) {
		t.Fatalf("grandchild %d is still running after close() reported the agent reaped", grandchild)
	}
}

// A member that outlives SIGKILL cannot be produced with a real process, so the
// probe is replaced. close() must then say the group is not gone, within the
// budget, and stay able to confirm it on a later call (the reaper in serve
// calls it again) once the member is.
func TestCloseReportsAProcessGroupThatOutlivesSIGKILL(t *testing.T) {
	var alive atomic.Bool
	alive.Store(true)
	oldProbe, oldBudget := probeGroup, reapBudget
	probeGroup, reapBudget = func(int) bool { return alive.Load() }, 150*time.Millisecond
	t.Cleanup(func() { probeGroup, reapBudget = oldProbe, oldBudget })
	c, err := startConnection(t.Context(), "/bin/sh", []string{"-c", "exit 0"}, t.TempDir(), func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	<-c.done
	start := time.Now()
	exit, err := c.close()
	if err == nil || !strings.Contains(err.Error(), "still has a live member") {
		t.Fatalf("close: err = %v, want the live group reported", err)
	}
	if !exit.Reaped || exit.Code != 0 || !exit.GroupLive {
		t.Fatalf("close: exit = %+v, want the direct child reported reaped with code 0 and GroupLive beside the error", exit)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("close took %v, want it bounded by the reap budget", elapsed)
	}
	alive.Store(false)
	if exit, err = c.close(); err != nil || !exit.Reaped || exit.GroupLive {
		t.Fatalf("close once the group is gone: exit = %+v, err = %v, want a clean confirmation", exit, err)
	}
}
