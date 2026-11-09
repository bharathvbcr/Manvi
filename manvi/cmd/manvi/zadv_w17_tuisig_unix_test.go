//go:build unix

package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Adversarial tests for lane w17 (endLeftoverChildren). Each drives the sweep
// through a stand-in ps in a directory outside the agent's reach, so the real
// trust rule (trustedResolved) still applies to it.

// zadvRealPS is the system ps the stand-ins delegate to.
func zadvRealPS(t *testing.T) string {
	t.Helper()
	for _, c := range []string{"/bin/ps", "/usr/bin/ps"} {
		if p, ok := trustedResolved(c); ok {
			return p
		}
	}
	t.Fatal("no system ps")
	return ""
}

// zadvUsePS installs body as the only ps the sweep may use. realPS is spliced
// in where the body says REAL.
func zadvUsePS(t *testing.T, body string) {
	t.Helper()
	path := t.TempDir() + "/ps"
	script := "#!/bin/sh\n" + strings.ReplaceAll(body, "REAL", zadvRealPS(t)) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { // #nosec G306 -- an executable test stand-in.
		t.Fatal(err)
	}
	if _, ok := trustedResolved(path); !ok {
		t.Fatalf("the stand-in ps %s is not judged trusted; the test cannot run", path)
	}
	saved := psCandidates
	psCandidates = []string{path}
	t.Cleanup(func() { psCandidates = saved })
}

// zadvChild starts a sleeping child in its own group. When reap is false
// nobody waits for it, so once killed it stays a zombie of this process.
func zadvChild(t *testing.T, reap bool) *exec.Cmd {
	t.Helper()
	// #nosec G204 -- a literal sleep.
	c := exec.CommandContext(t.Context(), "sleep", "300")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	if reap {
		go func() { _ = c.Wait(); close(waited) }()
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		if reap {
			<-waited
		} else {
			_ = c.Wait()
		}
	})
	return c
}

func zadvAlive(t *testing.T, pid int) bool {
	t.Helper()
	// A zombie answers kill 0, so ask ps for the state as well.
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	out, err := exec.CommandContext(t.Context(), "/bin/ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output() // #nosec G204 -- literal.
	return err == nil && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// A listing that names a stranger as this process's child, with a header,
// extra columns and junk, must not get the stranger killed: the re-check reads
// the parent from ps again.
func TestZadvW17ListedStrangerIsNotKilled(t *testing.T) {
	stranger := sacrificialProcess(t)
	self := strconv.Itoa(os.Getpid())
	sp := strconv.Itoa(stranger)
	zadvUsePS(t, `case "$*" in
*-p*) exec REAL "$@";;
esac
echo "  PID  PPID  PGID STAT"
echo "abc `+self+` 1 S"
echo "`+sp+` `+self+` `+sp+` S extra columns here"
echo "-5 `+self+` -5 S"
echo "`+sp+` `+self+` 99999999999999999999 S"
echo`)
	if got := endLeftoverChildren(); strings.Contains(got, sp) {
		t.Logf("report: %q", got)
	}
	if !zadvAlive(t, stranger) {
		t.Fatal("a process that was not this one's child was killed because the listing named it")
	}
}

// A ps that never returns must not hold the exit past the bound, and the exit
// must say the table was not read.
func TestZadvW17HungPSIsBoundedAndReported(t *testing.T) {
	zadvUsePS(t, "sleep 60")
	start := time.Now()
	got := endLeftoverChildren()
	if d := time.Since(start); d > childrenReapBound+3*time.Second {
		t.Fatalf("a hung ps held the exit for %v", d)
	}
	if got == "" {
		t.Fatal("a ps that never answered was reported as a clean sweep")
	}
}

// A child that cannot be confirmed as this process's own (the ps for the
// re-check fails, as a fork does under load) must not be skipped silently: the
// sweep either ends it or says it could not look. BREAK on ea35bac: parentOf's
// error is treated as "not ours" and the child is left running with no report.
func TestZadvW17RecheckFailureIsNotAQuietSkip(t *testing.T) {
	zadvUsePS(t, `for a in "$@"; do [ "$a" = "-p" ] && exit 1; done
exec REAL "$@"`)
	c := zadvChild(t, true)
	got := endLeftoverChildren()
	if zadvAlive(t, c.Process.Pid) && got == "" {
		t.Fatal("a child of this process was left running and the sweep reported nothing")
	}
}

// Same, when the re-check is slow rather than failed: it runs out the shared
// bound, and every child after it is skipped. The sweep stays bounded and must
// say so. BREAK on ea35bac for the same reason.
func TestZadvW17SlowRecheckIsNotAQuietSkip(t *testing.T) {
	zadvUsePS(t, `for a in "$@"; do [ "$a" = "-p" ] && sleep 30; done
exec REAL "$@"`)
	c := zadvChild(t, true)
	start := time.Now()
	got := endLeftoverChildren()
	if d := time.Since(start); d > childrenReapBound+3*time.Second {
		t.Fatalf("the sweep took %v", d)
	}
	if zadvAlive(t, c.Process.Pid) && got == "" {
		t.Fatal("a child of this process was left running and the sweep reported nothing")
	}
}

// A child that was killed but that nobody has waited for is a zombie, and is
// as gone as a process gets. The sweep must not report it as "still running",
// nor wait the whole bound for it. BREAK on ea35bac: gone polls kill(2), which
// succeeds on a zombie.
func TestZadvW17KilledUnreapedChildIsNotReportedRunning(t *testing.T) {
	c := zadvChild(t, false)
	start := time.Now()
	got := endLeftoverChildren()
	d := time.Since(start)
	if zadvAlive(t, c.Process.Pid) {
		t.Fatal("the child was not killed")
	}
	if strings.Contains(got, strconv.Itoa(c.Process.Pid)) {
		t.Fatalf("a killed child, awaiting only its parent's wait, was reported as still running after %v: %q", d, got)
	}
}
