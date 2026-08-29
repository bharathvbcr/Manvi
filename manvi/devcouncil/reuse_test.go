package devcouncil

import (
	"strings"
	"testing"

	"manvi/flags"
)

// The reuse check is advisory, so the way it fails is by being noisy or by
// being silently absent. These tests hold both ends.

// The search stem has to discriminate. A name that matches half a repository
// produces a note listing half a repository, which is a note nobody reads
// twice — and a check nobody reads is a check that is not running.
func TestReuseStemRefusesNamesThatCannotDiscriminate(t *testing.T) {
	for _, path := range []string{
		"cmd/manvi/main.go",
		"internal/util.go",
		"pkg/utils.go",
		"a/index.ts",
		"x/init.py",
		"deep/nested/types.go",
		"pkg/common.go",
		"lib/helper.go",
		"a/b.go", // too short to mean anything
		"",
	} {
		if got := reuseStem(path); got != "" {
			t.Errorf("reuseStem(%q) = %q, want no search: this name matches everything", path, got)
		}
	}
}

func TestReuseStemUsesTheBaseNameWithoutItsExtension(t *testing.T) {
	cases := map[string]string{
		"devcouncil/verify_paths.go":   "verify_paths",
		"src/PaymentProcessor.ts":      "PaymentProcessor",
		"a/b/retry_policy.test.ts":     "retry_policy",
		"nested/dir/scheduler.go":      "scheduler",
		"./leading/dot/normalizer.rs":  "normalizer",
		"weird/name.with.dots.and.ext": "name",
	}
	for path, want := range cases {
		if got := reuseStem(path); got != want {
			t.Errorf("reuseStem(%q) = %q, want %q", path, got, want)
		}
	}
}

// A clean check says nothing. Announcing "no duplicate found" on every new file
// trains the reader to skip the line, and the line that matters is the one that
// is not usually there.
func TestReuseNoteIsSilentWhenNothingWasFound(t *testing.T) {
	if got := (reuseReport{Path: "a.go"}).Note(); got != "" {
		t.Fatalf("a clean check said %q", got)
	}
}

// A check that could not run must not read like one that ran and found nothing.
// This is the whole distinction, in the one place the model actually reads.
func TestReuseNoteSaysWhenItDidNotLook(t *testing.T) {
	got := reuseReport{
		Path:     "a.go",
		Degraded: []string{"no code index is configured"},
	}.Note()
	if !strings.Contains(got, "did not run") {
		t.Fatalf("note = %q, want it to say the check did not run", got)
	}
	if !strings.Contains(got, "not a duplicate") {
		t.Fatalf("note = %q, want it to say what its silence does not prove", got)
	}
}

// The list is a sample and has to say so. A capped list presented as the whole
// answer is how "these three exist" gets read as "only these three exist".
func TestReuseNoteReportsWhatItLeftOut(t *testing.T) {
	got := reuseReport{
		Path:       "retry.go",
		Area:       "net",
		Candidates: []string{"a.go:Retry", "b.go:Retry"},
		More:       9,
	}.Note()
	if !strings.Contains(got, "and 9 more") {
		t.Fatalf("note = %q, want the omitted count", got)
	}
	if !strings.Contains(got, "in net") {
		t.Fatalf("note = %q, want the area named", got)
	}
	// Advisory, not a verdict: the note must leave the decision with the model.
	if !strings.Contains(got, "If it is not, carry on") {
		t.Fatalf("note = %q, want it to permit proceeding", got)
	}
}

// A degraded report never also lists candidates as though it had searched:
// whichever branch runs, the reader gets one unambiguous story.
func TestReuseNoteDegradationWinsOverCandidates(t *testing.T) {
	got := reuseReport{
		Candidates: []string{"a.go"},
		Degraded:   []string{"the code index is unavailable"},
	}.Note()
	if strings.Contains(got, "a.go") {
		t.Fatalf("note = %q: a report that could not run listed findings anyway", got)
	}
}

// A brand-new file in a repository with no code index gets the degradation, not
// silence — and the write still succeeds, because this never refuses anything.
func TestReuseCheckOnACreateIsAdvisoryAndVisible(t *testing.T) {
	// A posture that lets a write land without a task lease, because what is
	// under test is the note attached to the write and not the gate that
	// authorises it.
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureYolo})
	f.reg.deps.Map = nil

	res := f.call("devcouncil_write_file", map[string]any{
		"path": "src/scheduler.go", "content": "package src\n",
	})
	if res.IsError {
		t.Fatalf("the write was refused: %s", res.Text)
	}
	if len(res.Wrote) != 1 {
		t.Fatalf("wrote = %v, want the created path", res.Wrote)
	}
	if !strings.Contains(res.Text, "reuse check") {
		t.Fatalf("no reuse note on a newly created file:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "did not run") {
		t.Fatalf("the missing index was not reported:\n%s", res.Text)
	}
}

// Editing an existing file cannot be duplicating it, so the check does not run
// and the model is not charged for the note.
func TestReuseCheckDoesNotRunOnAnEdit(t *testing.T) {
	// A posture that lets a write land without a task lease, because what is
	// under test is the note attached to the write and not the gate that
	// authorises it.
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureYolo})
	f.reg.deps.Map = nil

	args := map[string]any{"path": "src/scheduler.go", "content": "package src\n"}
	if res := f.call("devcouncil_write_file", args); res.IsError {
		t.Fatalf("first write: %s", res.Text)
	}
	args["content"] = "package src\n\nfunc S() {}\n"
	res := f.call("devcouncil_write_file", args)
	if res.IsError {
		t.Fatalf("second write: %s", res.Text)
	}
	if strings.Contains(res.Text, "reuse check") {
		t.Fatalf("the reuse check ran on an overwrite:\n%s", res.Text)
	}
}

// A name that cannot discriminate produces no note at all — neither candidates
// nor a degradation, because no question was asked.
func TestReuseCheckSaysNothingForAGenericName(t *testing.T) {
	// A posture that lets a write land without a task lease, because what is
	// under test is the note attached to the write and not the gate that
	// authorises it.
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureYolo})
	f.reg.deps.Map = nil

	res := f.call("devcouncil_write_file", map[string]any{
		"path": "src/util.go", "content": "package src\n",
	})
	if res.IsError {
		t.Fatalf("write: %s", res.Text)
	}
	if strings.Contains(res.Text, "reuse check") {
		t.Fatalf("a generic name produced a note:\n%s", res.Text)
	}
}

// A role's named write exception must never override a caller that explicitly
// asked for a non-mutating child. Both facts arrive at the runner as one bool,
// so the distinction has to be made at the dispatch site — and if it is ever
// lost, `read_only: true` silently stops meaning read-only.
func TestCallerReadOnlyBeatsARolesWriteException(t *testing.T) {
	runner := &recordingRunner{}
	f := newFixtureRunner(t, runner)

	res := f.call("devcouncil_spawn_subagents", map[string]any{
		"tasks": []map[string]any{
			{"label": "p1", "prompt": "plan it", "type": "planner"},
			{"label": "p2", "prompt": "plan it", "type": "planner", "read_only": true},
		},
	})
	if res.IsError {
		t.Fatalf("spawn failed: %s", res.Text)
	}

	byLabel := map[string]SubAgentRequest{}
	for _, req := range runner.seen {
		byLabel[req.Label] = req
	}
	if len(byLabel) != 2 {
		t.Fatalf("dispatched %d children, want 2", len(byLabel))
	}
	if !byLabel["p1"].Surface.PermitsWrite("devcouncil_create_artifact") {
		t.Error("the planner lost the artifact exception its own prompt depends on")
	}
	if byLabel["p2"].Surface.PermitsWrite("devcouncil_create_artifact") {
		t.Error("a caller asked for a read-only child and the role's write exception " +
			"overrode it; read_only would no longer mean read-only")
	}
}
