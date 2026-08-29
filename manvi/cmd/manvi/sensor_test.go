package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"manvi/agent"
	"manvi/devcouncil"
	"manvi/session"
)

// fakeVerifier stands in for the repository-backed check so these tests can be
// about the sensor's decisions — when to skip, when to bounce, when to escalate,
// what to record — rather than about git and a Rust binary.
type fakeVerifier struct {
	report devcouncil.PathReport
	// queue, when non-empty, is consumed one report per call before report is
	// used.
	queue   []devcouncil.PathReport
	missing []string
	// gotPaths records what the check was actually handed, which is how the
	// filtering assertions are made.
	gotPaths []string
	gotCmd   string
	calls    int
}

func (f *fakeVerifier) VerifyPaths(_ context.Context, paths []string, command string) devcouncil.PathReport {
	f.calls++
	f.gotPaths = append([]string(nil), paths...)
	f.gotCmd = command
	// A queue models a check whose answer changes between looks, which is the
	// ordinary case the bounce exists for: it fails, the model fixes it, it
	// passes. A single fixed report cannot express that.
	if len(f.queue) > 0 {
		next := f.queue[0]
		f.queue = f.queue[1:]
		return next
	}
	return f.report
}

func (f *fakeVerifier) ExistingPaths(paths []string) (present, missing []string) {
	for _, p := range paths {
		if contains(f.missing, p) {
			continue
		}
		present = append(present, p)
	}
	return present, f.missing
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// fakeRunner is a sub-agent runner whose answer the test controls.
type fakeRunner struct {
	result devcouncil.SubAgentResult
	err    error
	calls  int
	gotReq devcouncil.SubAgentRequest
}

func (f *fakeRunner) RunSubAgent(_ context.Context, req devcouncil.SubAgentRequest) (devcouncil.SubAgentResult, error) {
	f.calls++
	f.gotReq = req
	return f.result, f.err
}

func newSensor(t *testing.T, v pathVerifier) (*sensor, *session.Log) {
	t.Helper()
	log := session.NewLog()
	return &sensor{native: v, log: log, flags: nil}, log
}

func reports(t *testing.T, log *session.Log) []session.VerifyReportData {
	t.Helper()
	var out []session.VerifyReportData
	for _, e := range log.Events() {
		if e.Type != session.VerifyReport {
			continue
		}
		var d session.VerifyReportData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatalf("decode verify report: %v", err)
		}
		out = append(out, d)
	}
	return out
}

// A turn that changed nothing is not verified, and says so. The distinction
// this pins is the one the whole design turns on: "not owed" is recorded, so a
// reader can tell it from "did not happen".
func TestSensorSkipsATurnThatChangedNothing(t *testing.T) {
	v := &fakeVerifier{}
	s, log := newSensor(t, v)

	e := &agent.TurnStopping{Turn: 1}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if e.Verdict != agent.SensorSkipped {
		t.Fatalf("verdict = %q, want skipped", e.Verdict)
	}
	if e.Inject != "" {
		t.Fatalf("a read-only turn was bounced: %q", e.Inject)
	}
	if v.calls != 0 {
		t.Fatalf("the verifier ran %d times on a turn that changed nothing", v.calls)
	}
	if got := reports(t, log); len(got) != 1 || got[0].Verdict != "skipped" {
		t.Fatalf("reports = %+v, want one skipped record", got)
	}
}

// The bit that made this worth building. A turn that ran a mutating tool and
// wrote no file — a shell command — must not be reported as verified.
func TestSensorDegradesWhenItCannotSeeWhatChanged(t *testing.T) {
	v := &fakeVerifier{report: devcouncil.PathReport{Verdict: devcouncil.VerdictPassed}}
	s, log := newSensor(t, v)

	e := &agent.TurnStopping{Turn: 1, Mutated: true}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if e.Verdict != agent.SensorDegraded {
		t.Fatalf("verdict = %q, want degraded: nothing named a file, so nothing was examined", e.Verdict)
	}
	if e.Inject != "" {
		t.Fatal("a degraded check bounced the model, which cannot fix a missing file list")
	}
	got := reports(t, log)
	if len(got) != 1 || len(got[0].Degraded) == 0 {
		t.Fatalf("reports = %+v, want a recorded degradation", got)
	}
}

// The same case with an operator command configured: now something did check
// the change, so the pass is real.
func TestSensorUsesTheOperatorCommandWhenNoPathsAreKnown(t *testing.T) {
	v := &fakeVerifier{report: devcouncil.PathReport{
		Verdict: devcouncil.VerdictPassed, Source: "operator verification command",
	}}
	s, _ := newSensor(t, v)
	s.command = "go test ./..."

	e := &agent.TurnStopping{Turn: 1, Mutated: true}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if e.Verdict != agent.SensorPassed {
		t.Fatalf("verdict = %q, want passed", e.Verdict)
	}
	if v.gotCmd != "go test ./..." {
		t.Fatalf("the verifier was handed command %q", v.gotCmd)
	}
}

// A failure bounces once with the findings, and does not reach for a critic on
// the first look: telling the model what is wrong is usually enough, and a
// second opinion nobody needed is a full child turn spent.
func TestSensorBouncesWithFindingsBeforeEscalating(t *testing.T) {
	v := &fakeVerifier{report: devcouncil.PathReport{
		Verdict:  devcouncil.VerdictFailed,
		Findings: []string{"a.go: the error is discarded"},
		Examined: []string{"a.go"},
	}}
	runner := &fakeRunner{}
	s, log := newSensor(t, v)
	s.runner = runner

	e := &agent.TurnStopping{Turn: 1, Mutated: true, Wrote: []string{"a.go"}}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if e.Verdict != agent.SensorFailed {
		t.Fatalf("verdict = %q, want failed", e.Verdict)
	}
	if !strings.Contains(e.Inject, "the error is discarded") {
		t.Fatalf("the inject did not carry the finding: %q", e.Inject)
	}
	if !strings.Contains(e.Inject, "harness") {
		t.Fatal("the inject must say it is the harness speaking, not the operator")
	}
	if runner.calls != 0 {
		t.Fatal("a critic was dispatched on the first failure; the ladder starts cheap")
	}
	if got := reports(t, log); len(got) != 1 || got[0].Verdict != "failed" {
		t.Fatalf("reports = %+v", got)
	}
}

// Second failure on the same turn: now the evidence says telling it is not
// working, and a critic earns its cost.
func TestSensorEscalatesOnTheSecondFailure(t *testing.T) {
	v := &fakeVerifier{report: devcouncil.PathReport{
		Verdict: devcouncil.VerdictFailed, Findings: []string{"still broken"},
	}}
	runner := &fakeRunner{result: devcouncil.SubAgentResult{
		Summary: "reviewed\n" + verdictMarker + " FAIL\n- the lock is never released",
		Verdict: devcouncil.SubAgentVerdict{
			Judged: true, Passed: false, Findings: []string{"the lock is never released"},
		},
	}}
	s, _ := newSensor(t, v)
	s.runner = runner

	e := &agent.TurnStopping{Turn: 1, Mutated: true, Wrote: []string{"a.go"}, Bounce: 1}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 1 {
		t.Fatalf("critic dispatched %d times, want 1", runner.calls)
	}
	if runner.gotReq.Verdict != verdictMarker {
		t.Fatalf("the critic was not asked for a structured verdict: %+v", runner.gotReq)
	}
	if !strings.Contains(e.Inject, "the lock is never released") {
		t.Fatalf("the critic's finding did not reach the model: %q", e.Inject)
	}
}

// Circling is the loop's own evidence that a turn is stuck, and it escalates on
// the first look rather than waiting for a second.
func TestSensorEscalatesOnCirclingWithoutWaiting(t *testing.T) {
	v := &fakeVerifier{report: devcouncil.PathReport{
		Verdict: devcouncil.VerdictFailed, Findings: []string{"broken"},
	}}
	runner := &fakeRunner{result: devcouncil.SubAgentResult{Summary: "notes"}}
	s, _ := newSensor(t, v)
	s.runner = runner

	e := &agent.TurnStopping{
		Turn: 1, Mutated: true, Wrote: []string{"a.go"},
		Circling: agent.NoProgressLimit,
	}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 1 {
		t.Fatalf("critic dispatched %d times on a circling turn, want 1", runner.calls)
	}
}

// One critic per turn. A second would be asked the same question about the same
// tree and would cost another child turn to produce the answer already in hand.
func TestSensorDispatchesOneCriticPerTurn(t *testing.T) {
	v := &fakeVerifier{report: devcouncil.PathReport{
		Verdict: devcouncil.VerdictFailed, Findings: []string{"broken"},
	}}
	runner := &fakeRunner{result: devcouncil.SubAgentResult{Summary: "notes"}}
	s, _ := newSensor(t, v)
	s.runner = runner

	for bounce := 1; bounce <= 2; bounce++ {
		e := &agent.TurnStopping{
			Turn: 1, Mutated: true, Wrote: []string{"a.go"}, Bounce: bounce,
		}
		if err := s.check(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if runner.calls != 1 {
		t.Fatalf("critic dispatched %d times within one turn, want 1", runner.calls)
	}

	// A new turn gets a fresh budget: the counter belongs to the turn, not to
	// however long whoever owns the bus decides to keep this listener.
	e := &agent.TurnStopping{Turn: 2, Mutated: true, Wrote: []string{"a.go"}, Bounce: 1}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 2 {
		t.Fatalf("critic dispatched %d times across two turns, want 2 — the counter did not reset",
			runner.calls)
	}
}

// A critic that could not run is a check that did not happen. The model is told
// so rather than left to read the silence as approval.
func TestSensorNamesACriticThatCouldNotRun(t *testing.T) {
	v := &fakeVerifier{report: devcouncil.PathReport{
		Verdict: devcouncil.VerdictFailed, Findings: []string{"broken"},
	}}
	runner := &fakeRunner{err: errors.New("no model attached")}
	s, _ := newSensor(t, v)
	s.runner = runner

	e := &agent.TurnStopping{Turn: 1, Mutated: true, Wrote: []string{"a.go"}, Bounce: 1}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.Inject, "could not complete") {
		t.Fatalf("a failed critic dispatch was not reported to the model: %q", e.Inject)
	}
}

// A critic that finishes without reaching a verdict has not approved anything,
// and the inject must not read as though it had.
func TestSensorTreatsAnUnjudgedCriticAsNoApproval(t *testing.T) {
	v := &fakeVerifier{report: devcouncil.PathReport{
		Verdict: devcouncil.VerdictFailed, Findings: []string{"broken"},
	}}
	runner := &fakeRunner{result: devcouncil.SubAgentResult{
		Summary: "I looked at it and it seems fine to me.",
	}}
	s, _ := newSensor(t, v)
	s.runner = runner

	e := &agent.TurnStopping{Turn: 1, Mutated: true, Wrote: []string{"a.go"}, Bounce: 1}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.Inject, "did not reach a verdict") {
		t.Fatalf("an unjudged critic was not reported as such: %q", e.Inject)
	}
	if strings.Contains(e.Inject, "found nothing blocking") {
		t.Fatal("prose was read as approval")
	}
}

// A truncated answer is not a finished one, whether or not anything was written.
func TestSensorBouncesATruncatedAnswer(t *testing.T) {
	v := &fakeVerifier{}
	s, log := newSensor(t, v)

	e := &agent.TurnStopping{Turn: 1, Truncated: true}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if e.Verdict != agent.SensorFailed {
		t.Fatalf("verdict = %q, want failed", e.Verdict)
	}
	if e.Inject == "" {
		t.Fatal("a cut-off answer closed the turn without being asked to finish")
	}

	// And only once. The second look does not re-bounce on the same fact — the
	// model has already been told, and the cap is what stops a loop.
	e2 := &agent.TurnStopping{Turn: 1, Truncated: true, Bounce: 1}
	if err := s.check(context.Background(), e2); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e2.Inject, "output limit") {
		t.Fatal("the truncation bounce repeated itself")
	}
	if got := reports(t, log); len(got) == 0 {
		t.Fatal("nothing was recorded")
	}
}

// A deleted path cannot be read by a content gate. It must be excluded from the
// examined set and named as an exclusion.
func TestSensorSeparatesDeletedPaths(t *testing.T) {
	v := &fakeVerifier{
		report:  devcouncil.PathReport{Verdict: devcouncil.VerdictPassed},
		missing: []string{"gone.go"},
	}
	s, _ := newSensor(t, v)

	e := &agent.TurnStopping{
		Turn: 1, Mutated: true, Wrote: []string{"kept.go", "gone.go"},
	}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if len(v.gotPaths) != 1 || v.gotPaths[0] != "kept.go" {
		t.Fatalf("the verifier was handed %v, want only the paths still on disk", v.gotPaths)
	}
}

// An incomplete path list is a check with a hole in it, and the hole travels
// into the record whatever the verdict was.
func TestSensorRecordsATruncatedPathList(t *testing.T) {
	v := &fakeVerifier{report: devcouncil.PathReport{Verdict: devcouncil.VerdictPassed}}
	s, log := newSensor(t, v)

	e := &agent.TurnStopping{
		Turn: 1, Mutated: true, Wrote: []string{"a.go"}, WroteTruncated: true,
	}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	got := reports(t, log)
	if len(got) != 1 {
		t.Fatalf("reports = %+v", got)
	}
	var found bool
	for _, d := range got[0].Degraded {
		if strings.Contains(d, "incomplete") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a truncated path list left no trace: %+v", got[0])
	}
}

// A verdict this build does not recognise is not evidence that anything passed.
func TestSensorTreatsAnUnknownVerdictAsDegraded(t *testing.T) {
	v := &fakeVerifier{report: devcouncil.PathReport{Verdict: "something-new"}}
	s, _ := newSensor(t, v)

	e := &agent.TurnStopping{Turn: 1, Mutated: true, Wrote: []string{"a.go"}}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if e.Verdict != agent.SensorDegraded {
		t.Fatalf("verdict = %q, want degraded for an unrecognised result", e.Verdict)
	}
}

// With no verifier attached at all, the answer is degraded — never a pass.
func TestSensorWithNoVerifierIsDegraded(t *testing.T) {
	s := &sensor{log: session.NewLog()}
	e := &agent.TurnStopping{Turn: 1, Mutated: true, Wrote: []string{"a.go"}}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if e.Verdict != agent.SensorDegraded {
		t.Fatalf("verdict = %q with no verifier attached, want degraded", e.Verdict)
	}
}
