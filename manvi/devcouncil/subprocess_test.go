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
	"sync"
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

const (
	setsidHelperEnv = "MANVI_RUNPROC_SETSID_HELPER"
	// setsidOwnerEnv names the process whose liveness bounds a sleeper. See
	// TestSetsidHelperProcess.
	setsidOwnerEnv = "MANVI_RUNPROC_SETSID_OWNER"
)

// TestSetsidHelperProcess is not a test: it is the child that
// TestRunProcLeavesASetsidDescendantRunning re-executes this binary as. "parent"
// starts a long-lived descendant, a "sleeper" in its own session when
// "parent-setsid" and in the parent's group otherwise, prints its pid and exits.
func TestSetsidHelperProcess(t *testing.T) {
	switch os.Getenv(setsidHelperEnv) {
	case "sleeper":
		// Lives exactly as long as its owner, the test process that started
		// the tree. While that test is running the sleeper's liveness changes
		// only through a kill, so a failed group kill is still seen (a timed
		// sleep would let it exit on its own and pass). When the owner is gone
		// (the binary panicking at its timeout, SIGINT, a cancelled job, a
		// fatal before its cleanup was registered) nothing is left to kill it,
		// and it would live under init forever, so it exits. With no owner
		// named there is nothing to bound it by, and it exits at once.
		owner, err := strconv.Atoi(os.Getenv(setsidOwnerEnv))
		if err != nil || owner <= 0 {
			return
		}
		for processAlive(owner) {
			time.Sleep(100 * time.Millisecond)
		}
	case "parent-setsid", "parent-samegroup", "owner-exits":
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		// #nosec G204 -- re-executes this very test binary.
		cmd := exec.CommandContext(context.Background(), exe, "-test.run=^TestSetsidHelperProcess$")
		cmd.Env = append(os.Environ(), setsidHelperEnv+"=sleeper")
		if os.Getenv(setsidHelperEnv) == "owner-exits" {
			// This process is the owner and exits right after printing the
			// pid, which is what TestSetsidSleeperExitsWhenItsOwnerIsGone
			// needs: a sleeper whose owner has gone away.
			cmd.Env = append(cmd.Env, setsidOwnerEnv+"="+strconv.Itoa(os.Getpid()))
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: os.Getenv(setsidHelperEnv) != "parent-samegroup"}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_, _ = os.Stdout.WriteString("pid=" + strconv.Itoa(cmd.Process.Pid) + "\n")
	}
}

// helperHangGuard is how long one helper test may spend waiting in total. Every
// wait below ends the moment its event happens, so the guard only matters when
// something is wrong; it is one deadline for the whole test, so it does not
// multiply across rounds, and it sits far below the package -timeout so a real
// hang fails as an assertion that prints the bound rather than as a package
// timeout.
const helperHangGuard = 3 * time.Minute

type helperGuard struct {
	deadline time.Time
	bound    time.Duration
}

func newHelperGuard(t *testing.T) helperGuard {
	t.Helper()
	bound := helperHangGuard
	if d, ok := t.Deadline(); ok {
		bound = min(bound, time.Until(d)/2)
	}
	return helperGuard{deadline: time.Now().Add(bound), bound: bound}
}

// remaining is the time left on the guard, never less than a second so a call
// made just at the deadline still gets a bounded, non-zero timeout.
func (g helperGuard) remaining() time.Duration {
	return max(time.Until(g.deadline), time.Second)
}

// until polls cond every 10 ms and reports whether it became true before the
// guard's deadline.
func (g helperGuard) until(cond func() bool) bool {
	for !cond() {
		if !time.Now().Before(g.deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// confirmedGone holds pids a test has seen exit. Their cleanup must not signal
// them: a pid that is gone may by then belong to an unrelated process.
var confirmedGone sync.Map

// startHelperTree runs the helper under runProc and returns the pid of the
// descendant it left behind.
func startHelperTree(t *testing.T, g helperGuard, mode string) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	res := runProc(context.Background(), procSpec{
		argv:        []string{exe, "-test.run=^TestSetsidHelperProcess$"},
		env:         append(childenv.Inherit(), setsidHelperEnv+"="+mode, setsidOwnerEnv+"="+strconv.Itoa(os.Getpid())),
		timeout:     g.remaining(),
		stdoutLimit: 4096,
	})
	// The pid is read and its kill registered before any check can fail the
	// test: a parent that set a sleeper going and then timed out or errored
	// has still left it running, and runProc's group kill does not reach one
	// that called setsid.
	pid := 0
	for _, line := range strings.Split(string(res.stdout), "\n") {
		if v, ok := strings.CutPrefix(line, "pid="); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
				pid = n
				// Killed by pid, and only this pid: it is the one process
				// this test started that runProc may have left running.
				t.Cleanup(func() {
					if _, gone := confirmedGone.Load(n); !gone {
						_ = syscall.Kill(n, syscall.SIGKILL)
					}
				})
			}
		}
	}
	if res.timedOut {
		t.Fatalf("helper did not finish within the %s hang guard (the guard ends %s from now): stderr=%s", g.bound, time.Until(g.deadline).Round(time.Millisecond), res.stderr)
	}
	if res.err != nil {
		t.Fatalf("helper failed: err=%v stderr=%s", res.err, res.stderr)
	}
	if pid == 0 {
		t.Fatalf("helper printed no valid pid: %q", res.stdout)
	}
	return pid
}

// processAlive reports a process that is running. kill(pid, 0) succeeds on a
// zombie, a killed process whose parent has not reaped it yet, so where /proc
// exists its state is read and Z counts as dead.
func processAlive(pid int) bool {
	// kill(0, 0) signals the caller's own process group and succeeds, so a
	// zero or negative pid would read as alive.
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
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
	g := newHelperGuard(t)
	grouped := startHelperTree(t, g, "parent-samegroup")
	escaped := startHelperTree(t, g, "parent-setsid")

	if !g.until(func() bool { return !processAlive(grouped) }) {
		t.Errorf("descendant %d in the child's group still alive %s after the group kill", grouped, g.bound)
	}
	if !processAlive(escaped) {
		t.Errorf("setsid descendant %d did not survive: the documented limit no longer holds, update runProc's comment", escaped)
	}
}

// TestSetsidSleeperExitsWhenItsOwnerIsGone proves the sleeper's failsafe: it
// blocks while its owner lives, and an owner that went away (the test binary
// panicking at its timeout, SIGINT, a cancelled CI job) must not leave it
// running under init forever. The owner here is a helper process that starts a
// setsid sleeper and exits.
func TestSetsidSleeperExitsWhenItsOwnerIsGone(t *testing.T) {
	g := newHelperGuard(t)
	pid := startHelperTree(t, g, "owner-exits")
	if !g.until(func() bool { return !processAlive(pid) }) {
		t.Fatalf("sleeper %d outlived its owner by %s", pid, g.bound)
	}
	confirmedGone.Store(pid, true)
}
