package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mapclient "github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
	"github.com/bharathvbcr/Manvi/manvi/codingagent"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
)

// These tests drive the verification op against a real git repository and the
// real profile store. The pieces that are expensive or non-deterministic — the
// critic's model turn and DevMap's index — are scripted, because what is under
// test is what the op concludes from what they say.

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// scriptedCritic answers from a function and counts its calls.
type scriptedCritic struct {
	calls   atomic.Int32
	answer  func(ctx context.Context, prompt string, criteria int) (string, error)
	lastMu  sync.Mutex
	lastReq string
}

func allPass(_ context.Context, _ string, criteria int) (string, error) {
	var b strings.Builder
	b.WriteString("I read the change.\n")
	for i := 1; i <= criteria; i++ {
		fmt.Fprintf(&b, "MANVI-VERDICT[%d]: PASS\n", i)
	}
	return b.String(), nil
}

func (c *scriptedCritic) Judge(ctx context.Context, prompt string, criteria int) (string, error) {
	c.calls.Add(1)
	c.lastMu.Lock()
	c.lastReq = prompt
	c.lastMu.Unlock()
	if c.answer == nil {
		return allPass(ctx, prompt, criteria)
	}
	return c.answer(ctx, prompt, criteria)
}

// scriptedDevmap is a DevmapClient whose index state and affected-test answer
// a test sets.
type scriptedDevmap struct {
	status    *mapclient.Status
	statusErr error
	tests     string
	err       error
	queries   atomic.Int32
	targets   atomic.Int64
}

const devmapOneTest = `{"items":[{"path":"pkg/a_test.go","depth":1}],"resolution":"Available","shown":1,"total":1,"hidden":0,"truncated":false,"tokens_used":3}`

func freshDevmapStatus() *mapclient.Status {
	return &mapclient.Status{DBPath: "db", HostContractVersion: 1, SchemaVersion: 26, ExpectedSchemaVersion: 26,
		SchemaRelation: "current", ReaderReady: true, QueryReady: true, GenerationID: 7, IsFresh: true,
		Capabilities: map[string]any{"affected": true}}
}

func newScriptedDevmap() *scriptedDevmap {
	return &scriptedDevmap{status: freshDevmapStatus(), tests: devmapOneTest}
}

func (d *scriptedDevmap) Status(context.Context) (*mapclient.Status, error) {
	return d.status, d.statusErr
}

func (d *scriptedDevmap) Advanced(_ context.Context, q mapclient.AdvancedQuery) (mapclient.AdvancedResult, error) {
	d.queries.Add(1)
	d.targets.Store(int64(len(q.Targets)))
	if d.err != nil {
		return mapclient.AdvancedResult{}, d.err
	}
	data := `{"targets":[],"blast_radius":{"layers":{"items":[],"resolution":"Available","shown":0,"total":0,"hidden":0,"truncated":false,"tokens_used":1},"unmatched_targets":[]},"tests":` + d.tests + `}`
	return mapclient.AdvancedResult{Data: json.RawMessage(data), Index: d.status}, nil
}

// fakeChecker is a PathChecker that reports what it is told and remembers what
// it was asked.
type fakeChecker struct {
	report  devcouncil.PathReport
	mu      sync.Mutex
	paths   []string
	command string
	calls   int
}

func (f *fakeChecker) VerifyPaths(_ context.Context, paths []string, command string, _ devcouncil.Baseline) devcouncil.PathReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths, f.command = append([]string(nil), paths...), command
	f.calls++
	report := f.report
	report.Examined = paths
	return report
}

func (f *fakeChecker) ExistingPaths(paths []string) (present, missing []string) { return paths, nil }

func passingChecker() *fakeChecker {
	return &fakeChecker{report: devcouncil.PathReport{Verdict: devcouncil.VerdictPassed, Source: "test"}}
}

type verifyHarness struct {
	t      *testing.T
	client *store.Client
	runner *VerifyRunner
	repo   string
	base   string
	critic *scriptedCritic
	devmap *scriptedDevmap
}

// newVerifyHarness builds a repository with one committed file, a board item
// with the given criteria, and a runner over a scripted critic and DevMap. The
// checker defaults to a passing fake; set deps.NewVerifier to use the real one.
func newVerifyHarness(t *testing.T, criteria []string, configure func(*VerifyDeps)) *verifyHarness {
	t.Helper()
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q", "-b", "main")
	writeFile(t, repo, "pkg/a.go", "package pkg\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "base")
	base := gitIn(t, repo, "rev-parse", "HEAD")

	client := store.New(testsupport.DCStore(t), filepath.Join(t.TempDir(), "profile.sqlite"))
	t.Cleanup(client.Close)
	enhancementCall(t, client, "repositories.put", `{"request_id":"r","id":"r","expected_revision":0,"name":"repo","identity_key":"r"}`)
	encoded, _ := json.Marshal(criteria)
	h := &verifyHarness{t: t, client: client, repo: repo, base: base, critic: &scriptedCritic{}, devmap: newScriptedDevmap()}
	h.putItem("t", string(encoded))

	checker := passingChecker()
	deps := VerifyDeps{
		Client: client, Critic: h.critic,
		Devmap:      func(string) DevmapClient { return h.devmap },
		NewVerifier: func(string, string, string) (devcouncil.PathChecker, error) { return checker, nil },
		// A binary that exists, so the missing-verifier check is not what a
		// test is accidentally exercising.
		VerifierBinary: "/bin/sh",
	}
	if configure != nil {
		configure(&deps)
	}
	h.runner, err = NewVerifyRunner(deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.runner.Close(); err != nil {
			t.Error(err)
		}
	})
	return h
}

func (h *verifyHarness) putItem(id, criteriaJSON string) {
	h.t.Helper()
	enhancementCall(h.t, h.client, "items.put", fmt.Sprintf(
		`{"request_id":"put-%s","id":%q,"expected_revision":0,"title":"fix E42","description":"d","acceptance_criteria":%s,"repository_ids":["r"],"primary_repository_id":"r"}`,
		id, id, criteriaJSON))
}

func (h *verifyHarness) params(item, request string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"request_id":%q,"item_id":%q,"worktree":%q,"base":%q,"head":"HEAD"}`, request, item, h.repo, h.base))
}

// change commits nothing: it leaves work in the tree, which is what an agent's
// finished task looks like to the verifier.
func (h *verifyHarness) change() {
	writeFile(h.t, h.repo, "pkg/a.go", "package pkg\n\nfunc A() int { return 1 }\n")
}

func (h *verifyHarness) start(item, request string) VerifyResult {
	h.t.Helper()
	result, err := h.runner.Start(context.Background(), h.params(item, request))
	if err != nil {
		h.t.Fatalf("Start: %v", err)
	}
	return result
}

// settled starts a verification and waits for its verdict.
func (h *verifyHarness) settled(item, request string) VerifyRecord {
	h.t.Helper()
	h.start(item, request)
	return h.wait(item, request)
}

func (h *verifyHarness) wait(item, request string) VerifyRecord {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		got, err := h.runner.Get(context.Background(), json.RawMessage(fmt.Sprintf(`{"item_id":%q,"request_id":%q}`, item, request)))
		if err != nil {
			h.t.Fatalf("Get: %v", err)
		}
		if got.Record.State == verifySettled {
			return got.Record
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatal("verification did not settle")
	return VerifyRecord{}
}

func requireDegraded(t *testing.T, record VerifyRecord, want string) {
	t.Helper()
	if record.Outcome != VerifyDegraded {
		t.Fatalf("outcome = %q, want degraded (record %+v)", record.Outcome, record)
	}
	all := strings.Join(record.Degraded, "\n")
	if rigor := record.Rigor; rigor != nil {
		all += "\n" + strings.Join(rigor.Degraded, "\n") + "\n" + strings.Join(rigor.Omitted, "\n")
	}
	if !strings.Contains(all, want) {
		t.Fatalf("degraded reasons do not name %q:\n%s", want, all)
	}
}

func TestVerifyPassesOnlyWhenEveryCheckRanAndEveryCriterionIsMet(t *testing.T) {
	checker := passingChecker()
	h := newVerifyHarness(t, []string{"adds A", "A returns 1"}, func(d *VerifyDeps) {
		d.NewVerifier = func(string, string, string) (devcouncil.PathChecker, error) { return checker, nil }
		d.Command = "operator-verify-command"
	})
	h.change()
	record := h.settled("t", "req-1")
	if record.Outcome != VerifyPassed || len(record.Degraded) != 0 {
		t.Fatalf("outcome = %q degraded = %v, want a clean pass", record.Outcome, record.Degraded)
	}
	if len(record.Criteria) != 2 || record.Criteria[0].Status != devcouncil.CriterionMet || record.Criteria[1].Status != devcouncil.CriterionMet {
		t.Fatalf("criteria = %+v", record.Criteria)
	}
	if got := strings.Join(checker.paths, ","); got != "pkg/a.go" {
		t.Fatalf("verifier saw paths %q, want only the changed file", got)
	}
	if checker.command != "operator-verify-command" {
		t.Fatalf("verifier ran %q, want the operator's command", checker.command)
	}
	if record.Tests == nil || !record.Tests.Queried || len(record.Tests.Candidates) != 1 || record.Tests.Candidates[0] != "pkg/a_test.go" {
		t.Fatalf("tests = %+v", record.Tests)
	}
}

func TestVerifyRecordIsPersistedInTheStoreAsEvidence(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.change()
	record := h.settled("t", "req-1")
	rows, truncated, err := h.client.EvidenceList(context.Background(), "t")
	if err != nil || truncated {
		t.Fatalf("EvidenceList: %v truncated=%v", err, truncated)
	}
	var found VerifyRecord
	for _, row := range rows {
		if row.Type == verifyEvidenceType {
			if err := json.Unmarshal(row.DataJSON, &found); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if found.RequestID != "req-1" || found.State != verifySettled || found.Outcome != record.Outcome {
		t.Fatalf("stored record = %+v, want the settled verdict", found)
	}
}

func TestVerifyUntrackedFileIsInTheChangedSetAndFailsARigorCheck(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds B"}, func(d *VerifyDeps) {
		d.VerifierBinary = testsupport.DCVerify(t)
		d.NewVerifier = nil // the real path verifier and the real dcverify
	})
	// Never added to git: the agent's most ordinary act, and the one `git diff`
	// alone cannot see.
	writeFile(t, h.repo, "pkg/b.go", "package pkg\n\n// B is not written yet: unimplemented!()\nfunc B() int { panic(\"TODO\") }\n")
	record := h.settled("t", "req-1")
	if record.Outcome != VerifyFailed {
		t.Fatalf("outcome = %q, want failed by the stub in the untracked file (record %+v)", record.Outcome, record)
	}
	if record.Changes.Untracked != 1 || !strings.Contains(strings.Join(record.Changes.Paths, ","), "pkg/b.go") {
		t.Fatalf("changes = %+v, want the untracked file counted and listed", record.Changes)
	}
	if record.Rigor == nil || !strings.Contains(strings.Join(record.Rigor.Findings, "\n"), "pkg/b.go") {
		t.Fatalf("rigor = %+v, want a finding about pkg/b.go", record.Rigor)
	}
}

func TestVerifyMissingDcverifyIsDegradedNotPassed(t *testing.T) {
	for name, binary := range map[string]string{"unset": "", "not on disk": "/nonexistent/dcverify"} {
		t.Run(name, func(t *testing.T) {
			h := newVerifyHarness(t, []string{"adds A"}, func(d *VerifyDeps) {
				d.VerifierBinary = binary
				d.NewVerifier = nil
			})
			h.change()
			requireDegraded(t, h.settled("t", "req-1"), "dcverify")
		})
	}
}

func TestVerifyStaleDevmapIndexIsDegraded(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.devmap.status.IsFresh = false
	reason := "source tree differs from the indexed generation"
	h.devmap.status.DegradedReason = &reason
	h.change()
	requireDegraded(t, h.settled("t", "req-1"), "stale")
}

func TestVerifyUnbuiltDevmapIndexIsDegraded(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.devmap.statusErr = errors.New("no devmap database")
	h.change()
	requireDegraded(t, h.settled("t", "req-1"), "devmap")
}

func TestVerifyAffectedTestsWalkIncompleteIsDegraded(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.devmap.tests = `{"items":[{"path":"pkg/a_test.go","depth":1}],"resolution":"Available","shown":1,"total":1,"hidden":0,"truncated":false,"tokens_used":3,"walk_incomplete":"the walk did not complete: stopped at depth 3"}`
	h.change()
	record := h.settled("t", "req-1")
	requireDegraded(t, record, "walk_incomplete")
	if record.Tests.WalkIncomplete == "" {
		t.Fatalf("the incompleteness was not recorded: %+v", record.Tests)
	}
}

func TestVerifyAffectedTestsTruncatedIsDegraded(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.devmap.tests = `{"items":[{"path":"pkg/a_test.go","depth":1}],"resolution":"Available","shown":1,"total":3,"hidden":2,"truncated":true,"tokens_used":3}`
	h.change()
	record := h.settled("t", "req-1")
	requireDegraded(t, record, "truncated")
	if !record.Tests.Truncated || record.Tests.Total != 3 {
		t.Fatalf("tests = %+v, want truncated with both counts carried", record.Tests)
	}
}

func TestVerifyEmptyChangeSetIsDegraded(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	record := h.settled("t", "req-1") // nothing changed since base
	requireDegraded(t, record, "empty")
	if h.critic.calls.Load() != 0 {
		t.Fatal("a critic was asked to judge a change that does not exist")
	}
}

func TestVerifyCriticWithNoVerdictLineIsDegraded(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.critic.answer = func(context.Context, string, int) (string, error) {
		return "Looks fine to me, nothing wrong.", nil
	}
	h.change()
	record := h.settled("t", "req-1")
	requireDegraded(t, record, "no verdict")
	if record.Criteria[0].Status == devcouncil.CriterionMet {
		t.Fatal("a criterion nobody judged was reported met")
	}
}

func TestVerifyCriticWithConflictingVerdictLinesIsDegraded(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.critic.answer = func(context.Context, string, int) (string, error) {
		return "MANVI-VERDICT[1]: PASS\nMANVI-VERDICT[1]: FAIL\n", nil
	}
	h.change()
	record := h.settled("t", "req-1")
	requireDegraded(t, record, "conflict")
	if record.Criteria[0].Status != devcouncil.CriterionConflict {
		t.Fatalf("criterion = %+v", record.Criteria[0])
	}
}

func TestVerifyCriticFailureIsDegraded(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.critic.answer = func(context.Context, string, int) (string, error) { return "", errors.New("provider down") }
	h.change()
	requireDegraded(t, h.settled("t", "req-1"), "critic")
}

func TestVerifyChangedPathListCappedAt128IsDegraded(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds files"}, func(d *VerifyDeps) {
		d.VerifierBinary = testsupport.DCVerify(t)
		d.NewVerifier = nil
	})
	for i := range 130 {
		writeFile(t, h.repo, fmt.Sprintf("gen/f%03d.go", i), "package gen\n")
	}
	record := h.settled("t", "req-1")
	requireDegraded(t, record, "more than 128 paths")
	if record.Changes.Total != 130 {
		t.Fatalf("total = %d, want the true count 130", record.Changes.Total)
	}
}

func TestVerifyCriterionTheCriticDidNotAddressIsUnmet(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A", "handles errors"}, nil)
	h.critic.answer = func(context.Context, string, int) (string, error) {
		return "Only the first one:\nMANVI-VERDICT[1]: PASS\n", nil
	}
	h.change()
	record := h.settled("t", "req-1")
	if record.Outcome != VerifyFailed {
		t.Fatalf("outcome = %q, want failed: criterion 2 was never addressed", record.Outcome)
	}
	if record.Criteria[1].Status != devcouncil.CriterionUnaddressed {
		t.Fatalf("criterion 2 = %+v, want unaddressed", record.Criteria[1])
	}
}

func TestVerifyFailedCriterionFailsWithTheCriticsObjection(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.critic.answer = func(context.Context, string, int) (string, error) {
		return "MANVI-VERDICT[1]: FAIL\n- A never checks its input\n", nil
	}
	h.change()
	record := h.settled("t", "req-1")
	if record.Outcome != VerifyFailed || len(record.Criteria[0].Findings) != 1 {
		t.Fatalf("record = %+v", record)
	}
}

func TestVerifyTaskWithNoCriteriaIsDegraded(t *testing.T) {
	h := newVerifyHarness(t, nil, nil)
	h.change()
	requireDegraded(t, h.settled("t", "req-1"), "no acceptance criteria")
}

func TestVerifyOperatorCommandFailureFailsTheTask(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, func(d *VerifyDeps) {
		d.VerifierBinary = testsupport.DCVerify(t)
		d.NewVerifier = nil
		d.Command = "exit 3"
	})
	h.change()
	record := h.settled("t", "req-1")
	if record.Outcome != VerifyFailed || record.Rigor == nil || !strings.Contains(strings.Join(record.Rigor.Findings, "\n"), "exited 3") {
		t.Fatalf("record = %+v", record)
	}
}

func TestVerifyRequestCannotCarryACommandOrPaths(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	for _, field := range []string{`"command":"rm -rf /"`, `"paths":["a.go"]`, `"criteria":["x"]`} {
		raw := fmt.Sprintf(`{"request_id":"r","item_id":"t","worktree":%q,"base":%q,"head":"HEAD",%s}`, h.repo, h.base, field)
		if _, err := h.runner.Start(context.Background(), json.RawMessage(raw)); err == nil {
			t.Fatalf("a request carrying %s was accepted", field)
		}
	}
}

func TestVerifyRefusesBadInputBeforeClaiming(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	cases := map[string]string{
		"relative worktree": fmt.Sprintf(`{"request_id":"r","item_id":"t","worktree":"repo","base":%q,"head":"HEAD"}`, h.base),
		"option as base":    fmt.Sprintf(`{"request_id":"r","item_id":"t","worktree":%q,"base":"--output=/tmp/x","head":"HEAD"}`, h.repo),
		"unknown revision":  fmt.Sprintf(`{"request_id":"r","item_id":"t","worktree":%q,"base":"nope","head":"HEAD"}`, h.repo),
		"unknown item":      fmt.Sprintf(`{"request_id":"r","item_id":"missing","worktree":%q,"base":%q,"head":"HEAD"}`, h.repo, h.base),
		"not a worktree":    fmt.Sprintf(`{"request_id":"r","item_id":"t","worktree":%q,"base":%q,"head":"HEAD"}`, t.TempDir(), h.base),
		"head is not HEAD":  fmt.Sprintf(`{"request_id":"r","item_id":"t","worktree":%q,"base":%q,"head":%q}`, h.repo, h.base, h.base),
	}
	gitIn(t, h.repo, "commit", "-q", "--allow-empty", "-m", "second")
	for name, raw := range cases {
		if _, err := h.runner.Start(context.Background(), json.RawMessage(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if rows, _, _ := h.client.EvidenceList(context.Background(), "t"); len(rows) != 0 {
		t.Fatalf("a refused request left %d evidence rows behind", len(rows))
	}
}

func TestVerifyRetryReturnsTheSameRecordAndDoesNotRerunTheCritic(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.change()
	first := h.settled("t", "req-1")
	again := h.start("t", "req-1")
	if !again.Replayed || again.Record.State != verifySettled || again.Record.RunID != first.RunID || again.Record.Outcome != first.Outcome {
		t.Fatalf("retry = %+v, want the settled record replayed", again)
	}
	if h.critic.calls.Load() != 1 {
		t.Fatalf("critic ran %d times, want 1", h.critic.calls.Load())
	}
}

func TestVerifyRetryWhileRunningReplaysTheClaim(t *testing.T) {
	release := make(chan struct{})
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.critic.answer = func(ctx context.Context, p string, n int) (string, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return allPass(ctx, p, n)
	}
	h.change()
	h.start("t", "req-1")
	again := h.start("t", "req-1")
	if !again.Replayed || again.Record.State != verifyRunning {
		t.Fatalf("retry = %+v, want the running claim", again)
	}
	close(release)
	h.wait("t", "req-1")
	if h.critic.calls.Load() != 1 {
		t.Fatalf("critic ran %d times, want 1", h.critic.calls.Load())
	}
}

func TestVerifyRequestIDCannotBeReusedForAnotherRange(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.change()
	h.settled("t", "req-1")
	raw := fmt.Sprintf(`{"request_id":"req-1","item_id":"t","worktree":%q,"base":"HEAD","head":"HEAD"}`, h.repo)
	if _, err := h.runner.Start(context.Background(), json.RawMessage(raw)); err == nil || !strings.Contains(err.Error(), "request_conflict") {
		t.Fatalf("err = %v, want request_conflict", err)
	}
}

func TestVerifyOrphanedRunningClaimSettlesDegradedWithoutRerunning(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	orphan := VerifyRecord{RequestID: "req-1", ItemID: "t", RunID: "verify-t-req-1", State: verifyRunning,
		Worktree: h.repo, Base: h.base, Head: "HEAD", StartedAt: "2026-01-01T00:00:00Z"}
	if err := h.runner.persist(context.Background(), orphan); err != nil {
		t.Fatal(err)
	}
	got := h.start("t", "req-1")
	if got.Record.State != verifySettled || got.Record.Outcome != VerifyDegraded {
		t.Fatalf("record = %+v, want an orphaned claim settled as degraded", got.Record)
	}
	if h.critic.calls.Load() != 0 {
		t.Fatal("an orphaned claim re-ran the critic")
	}
}

func TestVerifyJobDeadlineDegradesASlowCriticAndLeavesTheServerResponsive(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, func(d *VerifyDeps) { d.Timeout = 300 * time.Millisecond })
	started := make(chan struct{})
	h.critic.answer = func(ctx context.Context, _ string, _ int) (string, error) {
		close(started)
		<-ctx.Done() // a critic that never answers
		return "", ctx.Err()
	}
	h.change()
	managed, err := NewManagedRunner(h.client, func(context.Context, codingagent.Options) (ManagedSession, error) {
		return nil, errors.New("no session")
	}, func(err error) string { return err.Error() })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = managed.Close() })

	begun := time.Now()
	responses := roundTrip(t, Options{Modules: []Module{WorkbenchModule{Client: h.client, Managed: managed, Verify: h.runner}}},
		Request{ID: "verify", Op: OpVerifyTask, Params: h.params("t", "req-1")},
		Request{ID: "stop", Op: opManagedStop, Params: json.RawMessage(`{"id":"none"}`)},
		Request{ID: "hello", Op: OpHello})
	if len(responses) != 3 {
		t.Fatalf("got %d responses, want 3", len(responses))
	}
	for _, response := range byID(t, responses, "verify") {
		if !response.OK {
			t.Fatalf("verify was refused: %+v", response.Error)
		}
		var result VerifyResult
		if err := json.Unmarshal(response.Result, &result); err != nil || result.Record.State != verifyRunning {
			t.Fatalf("verify answered %s, want the running claim", response.Result)
		}
	}
	if time.Since(begun) > 5*time.Second {
		t.Fatalf("answering took %s; the op waited for the critic", time.Since(begun))
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the critic never started")
	}
	record := h.wait("t", "req-1")
	requireDegraded(t, record, "timed out")
}

func TestVerifyCloseCancelsAJobAndRecordsIt(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	started := make(chan struct{})
	h.critic.answer = func(ctx context.Context, _ string, _ int) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	h.change()
	h.start("t", "req-1")
	<-started
	if err := h.runner.Close(); err != nil {
		t.Fatal(err)
	}
	// Read through a fresh runner: the process that cancelled it is gone.
	reader, err := NewVerifyRunner(VerifyDeps{Client: h.client, Critic: h.critic, Devmap: func(string) DevmapClient { return h.devmap }})
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.Get(context.Background(), json.RawMessage(`{"item_id":"t","request_id":"req-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	requireDegraded(t, got.Record, "cancel")
}

func TestVerifyTwoItemsVerifyConcurrentlyWithoutCrossTalk(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.putItem("u", `["adds B"]`)
	var arrived sync.WaitGroup
	arrived.Add(2)
	barrier := make(chan struct{})
	go func() { arrived.Wait(); close(barrier) }()
	h.critic.answer = func(ctx context.Context, p string, n int) (string, error) {
		arrived.Done()
		select {
		case <-barrier: // both critics are inside Judge at once, or this never opens
		case <-time.After(10 * time.Second):
			return "", errors.New("the two verifications never overlapped")
		}
		if strings.Contains(p, "adds B") {
			return "MANVI-VERDICT[1]: FAIL\n- B is missing\n", nil
		}
		return allPass(ctx, p, n)
	}
	h.change()
	h.start("t", "req-t")
	h.start("u", "req-u")
	a, b := h.wait("t", "req-t"), h.wait("u", "req-u")
	if a.Outcome != VerifyPassed || b.Outcome != VerifyFailed {
		t.Fatalf("outcomes = %q / %q, want passed / failed; the two verdicts crossed or the run was not concurrent", a.Outcome, b.Outcome)
	}
	if a.ItemID != "t" || b.ItemID != "u" || a.RunID == b.RunID {
		t.Fatalf("records crossed: %+v / %+v", a, b)
	}
}

func TestVerifyTenThousandFileChangeIsBoundedAndDegraded(t *testing.T) {
	checker := passingChecker()
	h := newVerifyHarness(t, []string{"generates files"}, func(d *VerifyDeps) {
		d.NewVerifier = func(string, string, string) (devcouncil.PathChecker, error) { return checker, nil }
	})
	for i := range 10000 {
		writeFile(t, h.repo, fmt.Sprintf("gen/d%02d/f%05d.go", i%100, i), "package gen\n")
	}
	begun := time.Now()
	record := h.settled("t", "req-1")
	requireDegraded(t, record, "capped")
	if record.Changes.Total != 10000 || !record.Changes.Capped {
		t.Fatalf("changes total=%d capped=%v, want the true count and the cap reported", record.Changes.Total, record.Changes.Capped)
	}
	if len(record.Changes.Paths) > 128 {
		t.Fatalf("record lists %d paths, want a bounded sample", len(record.Changes.Paths))
	}
	if len(checker.paths) > maxVerifyChanged {
		t.Fatalf("verifier was handed %d paths, want at most %d", len(checker.paths), maxVerifyChanged)
	}
	if int(h.devmap.targets.Load()) > 128 {
		t.Fatalf("devmap was asked about %d targets, want at most 128", h.devmap.targets.Load())
	}
	if encoded, _ := json.Marshal(record); len(encoded) > maxVerifyRecordBytes {
		t.Fatalf("record is %d bytes, want at most %d", len(encoded), maxVerifyRecordBytes)
	}
	if time.Since(begun) > 60*time.Second {
		t.Fatalf("took %s", time.Since(begun))
	}
}

func TestVerifyOpsAreRegisteredOnlyWithARunner(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	with := opsOf(t, Options{Modules: []Module{WorkbenchModule{Client: h.client, Verify: h.runner}}})
	without := opsOf(t, Options{Modules: []Module{WorkbenchModule{Client: h.client}}})
	for _, op := range []string{OpVerifyTask, OpVerifyGet} {
		if !slices.Contains(with, op) || slices.Contains(without, op) {
			t.Fatalf("%s advertised with=%v without=%v", op, slices.Contains(with, op), slices.Contains(without, op))
		}
	}
}

func TestVerifyAPanickingCheckSettlesDegradedInsteadOfEndingTheHost(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.critic.answer = func(context.Context, string, int) (string, error) { panic("critic exploded") }
	h.change()
	requireDegraded(t, h.settled("t", "req-1"), "crashed")
}
