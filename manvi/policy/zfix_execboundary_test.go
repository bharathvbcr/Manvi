package policy

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/dc"
)

// The rungs in this file all answer one question: does the ladder's reading of
// a command line still describe what `sh -c` would do with it? Each test pins a
// place where it did not, and each of them fails against the code as it stood
// before the fix beside it.

// A command denied on its own must stay denied however it is nested.
//
// Arithmetic is the case that was wrong. The substitution scanner skipped
// `$(( … ))` spans whole, on the stated ground that arithmetic "expands
// variables but cannot execute commands" — and a command substitution nested
// inside one executes exactly as it would outside. `echo $(( $(touch f)0 ))`
// created the file, with no lease, under hard rules, because the ladder judged
// `echo` and never saw the touch.
func TestDeniedCommandStaysDeniedInsideEveryNesting(t *testing.T) {
	gate := CommandGate{HardRules: true}

	// The payload, judged alone, so the rest of the table means something.
	if d := gate.EvaluateCommand("touch owned", nil); !d.Blocked() {
		t.Fatalf("precondition: `touch owned` must be denied on its own, got %+v", d)
	}

	for _, command := range []string{
		"echo $(touch owned)",
		"echo `touch owned`",
		"echo $(( $(touch owned)0 ))",
		"echo $(( 1 + $(touch owned)0 ))",
		"echo $(( a[$(touch owned)0] ))",
		"echo $(( $(( $(touch owned)0 )) ))",
		`echo "$(( $(touch owned)0 ))"`,
		"echo $( echo $(( $(touch owned)0 )) )",
		"echo $(( $(echo `touch owned`)0 ))",
	} {
		if d := gate.EvaluateCommand(command, nil); !d.Blocked() {
			t.Errorf("%q must be denied — it runs `touch owned`; got %s (%s)",
				command, d.Action, d.Rule)
		}
	}
}

// The counterpart, so the fix above is a fix and not a blanket refusal:
// arithmetic that runs nothing still evaluates, and `>` inside it is a
// comparison operator rather than a redirection.
func TestArithmeticThatRunsNothingIsStillAllowed(t *testing.T) {
	gate := CommandGate{HardRules: true}
	for _, command := range []string{
		"echo $(( 1 + 2 ))",
		"echo $(( 3 > 2 ))",
		"echo $(( (1 + 2) * 3 ))",
	} {
		if d := gate.EvaluateCommand(command, nil); d.Blocked() {
			t.Errorf("%q runs no command and must be allowed, got %s (%s)",
				command, d.Action, d.Rule)
		}
	}

	// `$((3 > 2))` names no file. Reading that `>` as a redirection invents a
	// write to a file called 2 and sends it to the write gate.
	targets, opaque, err := RedirectTargets("echo $((3 > 2))")
	if err != nil || opaque || len(targets) != 0 {
		t.Fatalf("RedirectTargets(arithmetic comparison) = %v opaque=%v err=%v; want no targets",
			targets, opaque, err)
	}
}

// A redirection nested inside arithmetic still writes a file, so the redirect
// enumerator has to find it. This is the other half of the same scan: the two
// halves must describe the same string, or one of them permits a write the
// other never showed to the write gate.
func TestRedirectInsideArithmeticIsEnumerated(t *testing.T) {
	targets, opaque, err := RedirectTargets("echo $(( $(echo hi > owned.txt)0 ))")
	if err != nil {
		t.Fatalf("RedirectTargets: %v", err)
	}
	if opaque {
		t.Fatal("the target is a plain filename; opacity would hide it behind a vaguer refusal")
	}
	found := false
	for _, target := range targets {
		if target == "owned.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("owned.txt is written by this line and was not enumerated: %v", targets)
	}
}

// Test runners are not orientation commands. Running a project's tests executes
// the project's code with this process's authority, which is exactly what a
// lease allocates — so the lease-lifecycle allowlist must be matched below the
// no-lease refusal, not above it.
func TestLeaseOnlyCommandsRequireALease(t *testing.T) {
	gate := CommandGate{HardRules: true}

	for _, command := range []string{
		"pytest tests/",
		"pytest -q",
		"python -m pytest tests/ -q",
		"uv run pytest tests/",
		"dev run-cmd pytest",
		"dev release TASK-001",
		"dev scope update",
	} {
		d := gate.EvaluateCommand(command, nil)
		if !d.Blocked() {
			t.Errorf("%q must require a lease, got %s (%s)", command, d.Action, d.Rule)
			continue
		}
		if d.Rule != RuleCommandNoLease {
			t.Errorf("%q should be refused as %s, got %s", command, RuleCommandNoLease, d.Rule)
		}
	}

	// The same list is still available to a lease holder, which is what makes
	// this a reordering rather than a removal.
	task := &dc.Task{ID: "TASK-001"}
	for _, command := range []string{"pytest tests/", "dev release TASK-001"} {
		if d := gate.EvaluateCommand(command, task); d.Blocked() {
			t.Errorf("%q must still be allowed to a lease holder, got %s (%s)",
				command, d.Action, d.Rule)
		}
	}

	// And the commands the no-lease message tells the reader to use must
	// actually work without one, or the message cannot be followed.
	for _, command := range []string{"dev status", "dev map", "dev map query foo", "dev doctor"} {
		if d := gate.EvaluateCommand(command, nil); d.Blocked() {
			t.Errorf("the no-lease refusal names %q as the remedy, but it is refused: %s (%s)",
				command, d.Action, d.Rule)
		}
	}
}

// Git's safety rules are written against `git <subcommand>` and were matched by
// adjacency, so anything git accepts in between hid the subcommand from them.
func TestGitSafetySeesPastGlobalOptions(t *testing.T) {
	for _, command := range []string{
		"git push --force origin main",
		"git -C . push --force origin main",
		"git -c core.pager=cat push --force origin main",
		"git --no-pager push --force origin main",
		"git -c a=b -c c=d push --force origin main",
		"git --git-dir=.git push --force origin main",
		"git -C . -c core.pager=cat --no-pager push --force origin main",
		"git push -f origin main",
		"git -C . push -f origin main",
		"/usr/bin/git -C . push --force origin main",
	} {
		if d := GitSafety(command); !d.Blocked() {
			t.Errorf("%q is a force push and must be denied, got %s (%s)",
				command, d.Action, d.Rule)
		}
	}

	// The refspec spelling carries no --force flag at all.
	for _, command := range []string{
		"git push origin +main",
		"git -C . push origin +main",
	} {
		if d := GitSafety(command); !d.Blocked() {
			t.Errorf("%q forces a non-fast-forward update and must be denied, got %s (%s)",
				command, d.Action, d.Rule)
		}
	}

	// An ordinary push is still an ordinary push.
	if d := GitSafety("git -C . push origin feature-branch"); d.Blocked() {
		t.Errorf("a plain push must not be denied, got %s (%s)", d.Action, d.Rule)
	}
}

// The gate resolves a relative path against the working tree it judges for; the
// shell resolves it against wherever the shell is standing. `cd` is what makes
// those two different, so it is refused rather than waved through.
func TestDirectoryChangesAreRefused(t *testing.T) {
	gate := CommandGate{HardRules: true, Root: "/tmp/repo"}
	task := &dc.Task{ID: "TASK-001", AllowedCommands: []string{"echo *", "go test *"}}

	for _, command := range []string{
		"cd ../outside && echo AUDIT > allowed.txt",
		"cd /etc && echo AUDIT > allowed.txt",
		"cd sub && echo AUDIT > allowed.txt",
		"cd repo && go test ./...",
		"cd",
		"pushd /tmp",
		"popd",
		"echo hi && cd ../outside",
	} {
		d := gate.EvaluateCommand(command, task)
		if !d.Blocked() {
			t.Errorf("%q changes the working directory and must be denied, got %s (%s)",
				command, d.Action, d.Rule)
			continue
		}
		if d.Rule != RuleCommandDirectoryChange {
			t.Errorf("%q should be refused as %s, got %s", command, RuleCommandDirectoryChange, d.Rule)
		}
	}

	// git's own spelling of the same move.
	for _, command := range []string{
		"git -C ../elsewhere status",
		"git -C /tmp diff",
		"git --git-dir=/tmp/.git status",
		"git --work-tree=/tmp status",
	} {
		d := gate.EvaluateCommand(command, task)
		if !d.Blocked() || d.Rule != RuleCommandDirectoryChange {
			t.Errorf("%q runs against another working tree and must be refused as %s, got %s (%s)",
				command, RuleCommandDirectoryChange, d.Action, d.Rule)
		}
	}

	// A path that merely begins with the letters is not a directory change.
	for _, command := range []string{"cdate", "echo cd"} {
		if d := gate.EvaluateCommand(command, task); d.Blocked() && d.Rule == RuleCommandDirectoryChange {
			t.Errorf("%q is not a directory change, got %s", command, d.Rule)
		}
	}

	// The refusal has to say what to do instead, or it is a wall.
	d := gate.EvaluateCommand("cd sub && echo x > f.txt", task)
	if !strings.Contains(d.Reason, "repository root") {
		t.Errorf("the refusal should name the remedy, got %q", d.Reason)
	}
}
