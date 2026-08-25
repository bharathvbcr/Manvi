package devcouncil

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"manvi/grants"
	"manvi/policy"
	"manvi/ui"
)

// zfixtoolsApprover clears every escalation and records the question, so a test
// can assert on what the operator was actually shown.
type zfixtoolsApprover struct {
	asked []ui.Request
}

func (a *zfixtoolsApprover) Approve(_ context.Context, req ui.Request) (ui.Decision, error) {
	a.asked = append(a.asked, req)
	return ui.Decision{Allow: true, Reason: "cleared for the test", By: "human"}, nil
}

// zfixtoolsCaptureGrants installs the gate's issue hook so a test can inspect
// the grant an approval actually produced.
func zfixtoolsCaptureGrants(f *fixture, out *[]grants.Grant) {
	f.reg.deps.Gate.OnIssue = func(g grants.Grant) { *out = append(*out, g) }
}

// zfixtoolsExists reports whether a path exists.
func zfixtoolsExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// --- Defect 1: approval blindness -------------------------------------------

// TestZfixtoolsApprovalCardCarriesTheCommandThatWillRun pins the invariant that
// what a human authorises is exactly what executes.
//
// The command gate matches against a normalised form with trailing redirections
// stripped, and the denial it produces carries that stripped string as its
// Target. escalate put Target on the approval card and issued the grant from
// it, so the operator was asked to clear `cat seed.txt` while
// `cat seed.txt > zfixtools-sentinel.txt` was what stood ready to run — and the
// grant, scoped to the same stripped string, went on clearing every redirect
// variant of that command for as long as it lived.
func TestZfixtoolsApprovalCardCarriesTheCommandThatWillRun(t *testing.T) {
	approver := &zfixtoolsApprover{}
	f := newFixture(t)
	f.reg.deps.Approver = approver
	var issued []grants.Grant
	zfixtoolsCaptureGrants(f, &issued)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	// A sentinel, never a destructive payload. The target is src/calc.go —
	// TASK-001's one planned file — so the redirect rung permits the write and
	// the command genuinely runs. Pointing it at an unplanned path instead is
	// the separate guarantee asserted by the test below.
	const command = "cat seed.txt > src/calc.go"
	if err := os.MkdirAll(filepath.Join(f.root, "src"), 0o755); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	result := f.call("devcouncil_exec_command", map[string]string{"command": command})
	if result.IsError {
		t.Fatalf("the approved command was refused: %s", result.Text)
	}

	if len(approver.asked) != 1 {
		t.Fatalf("the approver was asked %d times, want 1", len(approver.asked))
	}
	if asked := approver.asked[0].Path; asked != command {
		t.Fatalf("the approval card said %q; the command that ran was %q — "+
			"the operator cleared something other than what executed", asked, command)
	}

	if len(issued) != 1 {
		t.Fatalf("%d grants issued, want 1", len(issued))
	}
	grant := issued[0]
	if len(grant.Scope.Paths) != 1 {
		t.Fatalf("grant scope = %v, want exactly one pattern", grant.Scope.Paths)
	}

	// The grant must cover the command that was shown...
	shown := policy.Decision{
		Action: policy.Deny, Rule: policy.RuleCommandNotAllowed,
		Target: command, TaskID: grant.Scope.TaskID,
	}
	if !grant.Matches(shown) {
		t.Fatalf("the grant does not cover the command it was issued for: %v", grant.Scope.Paths)
	}
	// ...and nothing else. The stripped form is a different command: it writes
	// nowhere, and clearing it was never what the operator was asked about.
	stripped := policy.NormalizeAllowlistCommand(command)
	if stripped == command {
		t.Fatalf("the test needs a command normalisation actually rewrites; %q was unchanged", command)
	}
	other := shown
	other.Target = stripped
	if grant.Matches(other) {
		t.Fatalf("the grant issued for %q also clears %q — one approval, every redirect variant",
			command, stripped)
	}

	if !zfixtoolsExists(filepath.Join(f.root, "src", "calc.go")) {
		t.Fatal("the redirect never landed; the test is not exercising the command it claims to")
	}
}

// Clearing the command is not clearing the write.
//
// The gate evaluates redirect targets only for a command it is about to permit,
// so a command denied at EvaluateCommand had its targets skipped — and an
// escalation issues the grant afterwards. That left the write half judged by
// nobody: a human cleared the command and the redirect landed on a path a
// direct devcouncil_write_file would have refused as scope.unplanned. The
// operator was asked once, about the command.
//
// Found by integrating two independently-correct fixes, neither of which could
// see the seam between them.
func TestZfixtoolsApprovedCommandStillGatesItsRedirectTarget(t *testing.T) {
	approver := &zfixtoolsApprover{}
	f := newFixture(t)
	f.reg.deps.Approver = approver
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	const unplanned = "zfixtools-unplanned.txt"
	result := f.call("devcouncil_exec_command",
		map[string]string{"command": "cat seed.txt > " + unplanned})

	if !result.IsError {
		t.Fatalf("BYPASS: the human cleared the command and the write to an "+
			"unplanned path went unjudged: %s", result.Text)
	}
	if !strings.Contains(result.Text, "scope.unplanned") {
		t.Fatalf("refused, but not by the write gate: %s", result.Text)
	}
	if len(approver.asked) != 1 {
		t.Fatalf("the approver was asked %d times, want exactly 1 (the command)",
			len(approver.asked))
	}
	if zfixtoolsExists(filepath.Join(f.root, unplanned)) {
		t.Fatal("the refused redirect still created its target")
	}
}

// TestZfixtoolsEscalationRefusesAnUnnamedSubject: consent to an operation
// nobody named is not consent.
func TestZfixtoolsEscalationRefusesAnUnnamedSubject(t *testing.T) {
	approver := &zfixtoolsApprover{}
	f := newFixture(t)
	f.reg.deps.Approver = approver

	decision := policy.Decision{
		Action: policy.Deny, Rule: policy.RuleCommandNotAllowed,
		Severity: policy.Soft, Target: "cat seed.txt",
	}
	if _, ok := f.reg.escalate(context.Background(), decision, "   "); ok {
		t.Fatal("an escalation with no concrete subject was treated as cleared")
	}
	if len(approver.asked) != 0 {
		t.Fatalf("a question with nothing to show was still put to the operator: %#v", approver.asked)
	}
}

// --- Defect 2: limitWriter must not kill the child --------------------------

// TestZfixtoolsLimitWriterHonoursTheWriterContract: io.Writer requires a short
// return to carry a non-nil error. limitWriter returned the truncated length
// with a nil error on the write that straddled the cap, and io.Copy — which is
// what os/exec uses to drain a child's stdout — turns that into
// io.ErrShortWrite, closes the pipe, and leaves the child on SIGPIPE.
func TestZfixtoolsLimitWriterHonoursTheWriterContract(t *testing.T) {
	var buf bytes.Buffer
	lw := &limitWriter{w: &buf, limit: 10}

	// Deliberately misaligned: the cap falls in the middle of this write, not
	// on its boundary. A cap that only ever lands on a chunk boundary hides the
	// bug entirely.
	n, err := lw.Write(make([]byte, 16))
	if err != nil {
		t.Fatalf("capping output is not an error: %v", err)
	}
	if n != 16 {
		t.Fatalf("Write returned n=%d for a 16-byte write with a nil error; "+
			"io.Copy reads that as a short write and tears down the pipe", n)
	}
	if buf.Len() != 10 {
		t.Fatalf("forwarded %d bytes, want the 10-byte cap", buf.Len())
	}

	// The same shape as os/exec's drain: bytes.Reader.WriteTo hands the whole
	// slice over in one call and checks the returned length itself.
	var buf2 bytes.Buffer
	lw2 := &limitWriter{w: &buf2, limit: 10}
	if _, err := io.Copy(lw2, bytes.NewReader(make([]byte, 16))); err != nil {
		t.Fatalf("io.Copy over the capped writer failed: %v", err)
	}
	if !lw2.truncated() {
		t.Fatal("the cap discarded bytes and did not record it")
	}
	if note := lw2.truncationNote(); !strings.Contains(note, "discarded") {
		t.Fatalf("a capped capture must say so; note = %q", note)
	}
}

// TestZfixtoolsCompletedCommandIsNotReportedAsFailed is the end-to-end half.
//
// The cap is 1 MiB and os/exec drains the pipe in 32 KiB reads, so 1 MiB of
// output lands exactly on a chunk boundary and the bug never fires — a naive
// repro passes by luck. The ten-byte preamble below misaligns it on purpose.
// The command runs to completion either way; before the fix runShell reported
// `short write` for it and exec_command rendered that as
// "failed to execute command".
func TestZfixtoolsCompletedCommandIsNotReportedAsFailed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the repro needs a POSIX shell")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// printf offsets the stream by ten bytes so the 1 MiB cap falls inside a
	// 32 KiB pipe read rather than on its edge; the sentinel proves the shell
	// reached the end of the command.
	command := "printf '0123456789'; head -c 3000000 /dev/zero | tr '\\0' 'a'; touch zfixtools-DONE"
	out, code, timedOut, err := runShell(ctx, dir, command)

	if err != nil {
		t.Fatalf("a command that ran to completion was reported as an execution failure: %v", err)
	}
	if timedOut {
		t.Fatal("the command did not time out")
	}
	if code != 0 {
		t.Fatalf("exit code %d for a command that completed (141 is SIGPIPE from the torn-down pipe)", code)
	}
	if !zfixtoolsExists(filepath.Join(dir, "zfixtools-DONE")) {
		t.Fatal("the sentinel is missing; the command did not actually complete")
	}
	if len(out) > 2*1024*1024 {
		t.Fatalf("captured %d bytes; the cap did not hold", len(out))
	}
	if !strings.Contains(out, "output truncated") {
		t.Fatalf("output was capped and the result did not say so: %q", out[max(0, len(out)-200):])
	}
}

// --- Defects 3 and 4: one containment rule, one reader ----------------------

// TestZfixtoolsGrepDoesNotReadThroughASymlinkOutOfTheRepository.
//
// grep walked with WalkDir, which does not follow symlinks, and then read each
// hit with os.ReadFile, which does — so a link inside the repository pointing
// at a file outside it was read and reported under its in-repo name. The 2 MiB
// guard was an lstat on the link, which measured the link.
func TestZfixtoolsGrepDoesNotReadThroughASymlinkOutOfTheRepository(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not dependable here")
	}
	f := newFixture(t)

	outside := filepath.Join(t.TempDir(), "outside-secret.txt")
	const canary = "sk-ant-FAKE-TESTVALUE"
	if err := os.WriteFile(outside, []byte("key="+canary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.root, "notes.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got := f.payload("devcouncil_grep", map[string]any{"pattern": canary})
	if count, _ := got["count"].(float64); count != 0 {
		t.Fatalf("grep read a file outside the repository through a symlink: %v", got)
	}
	// The echoed pattern is expected; a match is not. Only the matches are
	// checked, or the assertion would fire on grep quoting its own argument.
	if strings.Contains(fmt.Sprint(got["matches"]), canary) {
		t.Fatalf("the secret reached the result: %v", got["matches"])
	}

	// The other reader has always refused this. That both now refuse is the
	// point: one containment rule, not one per reader.
	read := f.call("devcouncil_read_file", map[string]string{"path": "notes.txt"})
	if !read.IsError {
		t.Fatalf("read_file followed the link out of the repository: %q", read.Text)
	}
}

// TestZfixtoolsGrepStillReadsOrdinaryFiles guards against fixing containment by
// breaking the tool: a refusal that refuses everything is not containment.
func TestZfixtoolsGrepStillReadsOrdinaryFiles(t *testing.T) {
	f := newFixture(t)
	nested := filepath.Join(f.root, "pkg", "inner")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "found.go"),
		[]byte("package inner\n// zfixtoolsNeedle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := f.payload("devcouncil_grep", map[string]any{"pattern": "zfixtoolsNeedle"})
	if count, _ := got["count"].(float64); count != 1 {
		t.Fatalf("grep lost an ordinary in-repo match: %v", got)
	}
}

// TestZfixtoolsReadFileRefusesANonRegularFileInsideTheRepository.
//
// read_file was os.ReadFile with no regular-file check, no size cap, and a ctx
// it accepted and never used. A FIFO planted inside the repository held the
// call open indefinitely — well past the tool's own deadline — wedging the turn.
func TestZfixtoolsReadFileRefusesANonRegularFileInsideTheRepository(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs here")
	}
	f := newFixture(t)
	fifo := filepath.Join(f.root, "zfixtools-pipe")
	if err := runMkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	// The pre-fix failure mode is an unbounded block, so the assertion is a
	// deadline: the call has to come back at all.
	done := make(chan bool, 1)
	go func() {
		done <- f.call("devcouncil_read_file", map[string]string{"path": "zfixtools-pipe"}).IsError
	}()
	select {
	case isErr := <-done:
		if !isErr {
			t.Fatal("read_file returned success for a FIFO")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("read_file on a FIFO never returned; the tool is wedged past its own deadline")
	}
}

// TestZfixtoolsReadFileIsBounded: an unbounded read is a way to spend the whole
// context window, and the pinned reader refuses loudly rather than truncating
// silently.
func TestZfixtoolsReadFileIsBounded(t *testing.T) {
	f := newFixture(t)
	big := filepath.Join(f.root, "zfixtools-big.bin")
	if err := os.WriteFile(big, bytes.Repeat([]byte("a"), maxToolReadBytes+1024), 0o644); err != nil {
		t.Fatal(err)
	}
	res := f.call("devcouncil_read_file", map[string]string{"path": "zfixtools-big.bin"})
	if !res.IsError {
		t.Fatalf("read_file returned %d bytes with no ceiling", len(res.Text))
	}
	if !strings.Contains(res.Text, "limit") {
		t.Fatalf("the refusal must name the limit it enforced: %q", res.Text)
	}

	// A file under the ceiling is still read whole.
	small := filepath.Join(f.root, "zfixtools-small.txt")
	if err := os.WriteFile(small, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	okRes := f.call("devcouncil_read_file", map[string]string{"path": "zfixtools-small.txt"})
	if okRes.IsError || okRes.Text != "hello\n" {
		t.Fatalf("an ordinary read regressed: err=%v text=%q", okRes.IsError, okRes.Text)
	}
}

// TestZfixtoolsReadContainedHonoursACancelledContext: the ctx read_file is
// handed now bounds the read instead of being ignored.
func TestZfixtoolsReadContainedHonoursACancelledContext(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readContained(ctx, f.root, "seed.txt", maxToolReadBytes); err == nil {
		t.Fatal("a cancelled context did not stop the read")
	}
}

// --- Defect 5: the child's environment --------------------------------------

// TestZfixtoolsShellChildDoesNotInheritTheOperatorEnvironment.
//
// The child inherited everything, so an agent-authored command could print the
// operator's provider keys straight back into the transcript. The replacement
// is an allowlist: a name not on it does not reach the child at all.
func TestZfixtoolsShellChildDoesNotInheritTheOperatorEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the repro needs a POSIX shell")
	}
	const canary = "sk-ant-FAKE-TESTVALUE"
	t.Setenv("ZFIXTOOLS_PROVIDER_KEY", canary)
	t.Setenv("LD_PRELOAD", "/zfixtools/nonexistent.so")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, code, timedOut, err := runShell(ctx, t.TempDir(), "env")
	if err != nil || timedOut || code != 0 {
		t.Fatalf("env failed: err=%v timedOut=%v code=%d out=%q", err, timedOut, code, out)
	}

	if strings.Contains(out, canary) {
		t.Fatal("the operator's secret reached the child environment")
	}
	if strings.Contains(out, "ZFIXTOOLS_PROVIDER_KEY") {
		t.Fatalf("an unlisted variable was passed through:\n%s", out)
	}
	if strings.Contains(out, "LD_PRELOAD") {
		t.Fatalf("an interpreter preload variable reached the child:\n%s", out)
	}
	// The allowlist has to leave a usable shell behind, or every command breaks.
	if !strings.Contains(out, "PATH=") {
		t.Fatalf("PATH did not survive sanitisation; nothing would be runnable:\n%s", out)
	}
}

// --- Defect 6: orphaned descendants -----------------------------------------

// TestZfixtoolsNoDescendantSurvivesRunShell asserts on both exit paths.
//
// The group kill hung off ctx.Done() alone, so a command that exited zero
// closed the watcher and left its descendants running, attached to nothing that
// would ever reap them. The descendants here redirect their own stdio to
// /dev/null so they do not hold the capture pipe, which is a separate concern
// with its own test; what is under test is whether they are still alive.
func TestZfixtoolsNoDescendantSurvivesRunShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no process groups here")
	}

	// The success path: sh exits 0 and nothing else signals the group.
	t.Run("success", func(t *testing.T) {
		dir := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, code, timedOut, err := runShell(ctx, dir,
			"( sleep 2; touch zfixtools-orphan ) >/dev/null 2>&1 & echo started")
		if err != nil || timedOut || code != 0 {
			t.Fatalf("the foreground command succeeded: err=%v timedOut=%v code=%d", err, timedOut, code)
		}
		zfixtoolsAssertNoOrphan(t, filepath.Join(dir, "zfixtools-orphan"))
	})

	// The timeout path: the deadline fires while the group is still working.
	t.Run("timeout", func(t *testing.T) {
		dir := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		_, _, timedOut, err := runShell(ctx, dir,
			"( sleep 2; touch zfixtools-orphan ) >/dev/null 2>&1 & sleep 600")
		if err != nil {
			t.Fatalf("a timeout is not an execution error: %v", err)
		}
		if !timedOut {
			t.Fatal("expected the deadline to be reported")
		}
		zfixtoolsAssertNoOrphan(t, filepath.Join(dir, "zfixtools-orphan"))
	})
}

// zfixtoolsAssertNoOrphan waits past the descendant's own sleep and requires
// that it never got to run. A surviving descendant is the whole defect, so the
// wait is generous rather than tight.
func zfixtoolsAssertNoOrphan(t *testing.T, sentinel string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if zfixtoolsExists(sentinel) {
			t.Fatalf("a descendant outlived the tool call and wrote %s", filepath.Base(sentinel))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// --- Defect 7: the flat listing had no ceiling ------------------------------

// TestZfixtoolsListDirCapsAndSaysSo: the recursive branch capped at 500 and
// reported it; the flat branch had no cap at all, so a large directory was
// rendered whole into one result. A capped sample must never be handed back
// looking like complete coverage.
func TestZfixtoolsListDirCapsAndSaysSo(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.root, "many")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 620; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d.txt", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := f.payload("devcouncil_list_dir", map[string]any{"path": "many"})
	count, _ := got["count"].(float64)
	if count != 500 {
		t.Fatalf("flat listing returned %v entries; the cap is 500", count)
	}
	if got["truncated"] != true {
		t.Fatalf("a capped listing was presented as the whole directory: %v", got["truncated"])
	}
	if limit, _ := got["limit"].(float64); limit != 500 {
		t.Fatalf("the result must name the ceiling it applied, got %v", got["limit"])
	}

	// A directory under the ceiling is still listed whole and says nothing
	// about truncation.
	small := f.payload("devcouncil_list_dir", map[string]any{"path": "."})
	if _, claimed := small["truncated"]; claimed {
		t.Fatalf("a complete listing claimed truncation: %v", small)
	}
}
