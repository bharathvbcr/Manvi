//go:build unix

package devcouncil

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/childenv"
)

// writeScript puts an executable shell script at dir/name.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// alive reports whether the pid written to path is still running.
func alive(t *testing.T, path string) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var raw []byte
	for {
		// #nosec G304 -- a path under this test's own t.TempDir.
		data, err := os.ReadFile(path)
		if err == nil && len(strings.TrimSpace(string(data))) > 0 {
			raw = data
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the descendant never wrote its pid to %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	// A reaped group is gone within moments; give the kill time to land.
	for range 50 {
		if syscall.Kill(pid, 0) != nil || exited(pid) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	return true
}

// exited reports a process that has terminated but not been reaped. Signal 0
// still succeeds on a zombie, and a backgrounded descendant is reparented to
// PID 1 when its parent exits — so under an init that does not reap orphans,
// as in some containers, a descendant the harness killed stays a zombie for
// good and reads as alive. Where /proc is absent the state is unknown and the
// signal alone decides.
func exited(pid int) bool {
	// #nosec G304 -- a fixed procfs path built from a pid.
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// The state follows the parenthesised command name, which may itself
	// contain spaces or parentheses, so it is found from the last ')'.
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 || end+2 >= len(stat) {
		return false
	}
	return stat[end+2] == 'Z' || stat[end+2] == 'X'
}

// TestAVerifierCannotLeaveADescendantBehind: a child that exits cleanly after
// backgrounding work must not leave that work running. exec_command has reaped
// its group on every exit path since `sleep 30 &` outlived the tool call that
// started it; the verifier, the git runners and the inspect bridge reaped it
// only on the deadline, so the same shape leaked there.
func TestAVerifierCannotLeaveADescendantBehind(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	bin := writeScript(t, dir, "dcverify",
		"(sleep 60 </dev/null >/dev/null 2>&1 & echo $! > "+pidFile+")\n"+
			`printf '{"ok":true,"verifier":"dc-verify","schema_version":1}'`+"\n")

	if err := (rigorClient{Binary: bin}).available(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
	if alive(t, pidFile) {
		t.Fatal("the verifier's backgrounded descendant outlived the call that started it")
	}
}

// TestAVerifierHealthReplyIsBounded: the health probe read the whole of stdout
// with no cap, so a binary answering with an endless stream was an allocation
// the harness paid for before it looked at a byte.
func TestAVerifierHealthReplyIsBounded(t *testing.T) {
	dir := t.TempDir()
	bin := writeScript(t, dir, "dcverify", "yes '{}' | head -c 50000000\n")
	err := (rigorClient{Binary: bin}).available(context.Background())
	if err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("an oversized health reply returned %v, want a refusal naming the bound", err)
	}
}

// TestGatedGitEmitsRawUTF8Paths: every git child goes through one construction
// site, so the write path reads paths the way the read path does. gatedGit
// built its own command without core.quotePath=false, and a commit summary
// named "é.txt" as "\303\251.txt".
func TestGatedGitEmitsRawUTF8Paths(t *testing.T) {
	f := newFixtureWith(t, map[string]string{
		flags.HarnessPosture:      flags.PostureStrict,
		flags.GrantsAgentCommands: "true",
	})
	if err := os.WriteFile(filepath.Join(f.root, "é.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, f.root, "add", "é.txt")
	checkout(t, f)
	grantCommand(t, f, renderCommandLine([]string{"git", "commit", "-m", "accent"}))

	out := f.payload("devcouncil_git_commit", map[string]any{"message": "accent"})
	if out["exit_code"] != float64(0) {
		t.Fatalf("commit = %v", out)
	}
	text, _ := out["output"].(string)
	if !strings.Contains(text, "é.txt") {
		t.Fatalf("commit output does not name é.txt as UTF-8:\n%s", text)
	}
}

// TestRunProcRefusesAnUnboundedCall: a call with neither its own timeout nor a
// deadline on its context is refused rather than run without a bound.
func TestRunProcRefusesAnUnboundedCall(t *testing.T) {
	res := runProc(context.Background(), procSpec{argv: []string{"true"}, env: childenv.Inherit()})
	if res.err == nil || !strings.Contains(res.err.Error(), "bound") {
		t.Fatalf("unbounded call returned %v", res.err)
	}
}

// TestRunProcKeepsWhatItCapturedOnATimeout: the deadline abandons the run, and
// what had already been captured is still readable — under a lock, because
// the abandoned copy may still be writing.
func TestRunProcKeepsWhatItCapturedOnATimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	res := runProc(ctx, procSpec{
		argv: []string{"sh", "-c", "echo started; sleep 600"}, env: childenv.Inherit(), stdoutLimit: 1024, combined: true,
	})
	if !res.timedOut || res.err != nil {
		t.Fatalf("timedOut=%v err=%v, want a timeout and no error", res.timedOut, res.err)
	}
	if !strings.Contains(string(res.stdout), "started") {
		t.Fatalf("output before the deadline was lost: %q", res.stdout)
	}
}

// TestRunProcReportsCancellationAsAnError: a cancelled call is not a timeout,
// and it is not an exit status either.
func TestRunProcReportsCancellationAsAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	res := runProc(ctx, procSpec{argv: []string{"sleep", "600"}, env: childenv.Inherit(), timeout: time.Minute})
	if res.timedOut || !errors.Is(res.err, context.Canceled) {
		t.Fatalf("timedOut=%v err=%v, want context.Canceled", res.timedOut, res.err)
	}
}

// TestRunProcCapsAndSaysSo: both streams are capped, and a cap that dropped
// something says how much.
func TestRunProcCapsAndSaysSo(t *testing.T) {
	res := runProc(context.Background(), procSpec{
		argv:        []string{"sh", "-c", "head -c 5000 /dev/zero; head -c 5000 /dev/zero >&2; exit 3"},
		env:         childenv.Inherit(),
		timeout:     time.Minute,
		stdoutLimit: 100, stderrLimit: 10,
	})
	if res.err == nil || res.exitCode != 3 {
		t.Fatalf("exitCode=%d err=%v, want exit 3 carried as an ExitError", res.exitCode, res.err)
	}
	if len(res.stdout) != 100 || len(res.stderr) != 10 {
		t.Fatalf("captured %d/%d bytes, want 100/10", len(res.stdout), len(res.stderr))
	}
	if res.stdoutNote == "" || res.stderrNote == "" {
		t.Fatal("a truncated capture did not say it was truncated")
	}
}

const setsidHelperEnv = "MANVI_RUNPROC_SETSID_HELPER"

// TestSetsidHelperProcess is not a test: it is the child that
// TestRunProcLeavesASetsidDescendantRunning re-executes this binary as. "parent"
// starts a long-lived descendant, a "sleeper" in its own session when
// "parent-setsid" and in the parent's group otherwise, prints its pid and exits.
func TestSetsidHelperProcess(t *testing.T) {
	switch os.Getenv(setsidHelperEnv) {
	case "sleeper":
		time.Sleep(2 * time.Minute)
	case "parent-setsid", "parent-samegroup":
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		// #nosec G204 -- re-executes this very test binary.
		cmd := exec.CommandContext(context.Background(), exe, "-test.run=^TestSetsidHelperProcess$")
		cmd.Env = append(os.Environ(), setsidHelperEnv+"=sleeper")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: os.Getenv(setsidHelperEnv) == "parent-setsid"}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_, _ = os.Stdout.WriteString("pid=" + strconv.Itoa(cmd.Process.Pid) + "\n")
	}
}

// startHelperTree runs the helper under runProc and returns the pid of the
// descendant it left behind.
func startHelperTree(t *testing.T, mode string) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	res := runProc(context.Background(), procSpec{
		argv:        []string{exe, "-test.run=^TestSetsidHelperProcess$"},
		env:         append(childenv.Inherit(), setsidHelperEnv+"="+mode),
		timeout:     30 * time.Second,
		stdoutLimit: 4096,
	})
	if res.err != nil || res.timedOut {
		t.Fatalf("helper failed: err=%v timedOut=%v stderr=%s", res.err, res.timedOut, res.stderr)
	}
	for _, line := range strings.Split(string(res.stdout), "\n") {
		if v, ok := strings.CutPrefix(line, "pid="); ok {
			pid, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				t.Fatalf("bad pid line %q", line)
			}
			// Killed by pid, and only this pid: it is the one process this test
			// started that runProc may have left running.
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			return pid
		}
	}
	t.Fatalf("helper printed no pid: %q", res.stdout)
	return 0
}

// processAlive reports a process that is running. kill(pid, 0) succeeds on a
// zombie, a killed process whose parent has not reaped it yet, so where /proc
// exists its state is read and Z counts as dead.
func processAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	// The state is the field after the command, which is parenthesised and may
	// itself contain spaces or parentheses, so it is found from the last ")".
	text := string(stat)
	if i := strings.LastIndexByte(text, ')'); i >= 0 && i+2 < len(text) {
		return text[i+2] != 'Z'
	}
	return true
}

// TestRunProcLeavesASetsidDescendantRunning pins what runProc's comment says it
// bounds. A descendant in the child's group is killed with it; one that called
// setsid is in another session and group and survives the group kill. The
// comment used to say "nothing the child started survives the call", which this
// test observes to be false for the second case.
func TestRunProcLeavesASetsidDescendantRunning(t *testing.T) {
	grouped := startHelperTree(t, "parent-samegroup")
	escaped := startHelperTree(t, "parent-setsid")

	deadline := time.Now().Add(5 * time.Second)
	for processAlive(grouped) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processAlive(grouped) {
		t.Errorf("descendant %d in the child's group survived the group kill", grouped)
	}
	if !processAlive(escaped) {
		t.Errorf("setsid descendant %d did not survive: the documented limit no longer holds, update runProc's comment", escaped)
	}
}
