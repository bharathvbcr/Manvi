package devcouncil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"manvi/core/bus"
	"manvi/dc/store"
	"manvi/flags"
	"manvi/gate"
	"manvi/internal/testsupport"
	"manvi/policy"
	"manvi/tools"
)

// fixture builds a real repository, a real store with a real task, and the
// native tool surface over both. Nothing here is mocked: the tools are only
// worth testing against the boundaries they actually cross.
type fixture struct {
	reg  *Registry
	pipe *tools.Registry
	root string
	db   string
	t    *testing.T
}

// newFixture builds the tool surface under the strict posture, which is what
// almost every test here wants: they are testing what the gate does when it is
// enforcing. The shipped default is dev posture, and it has its own test.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureStrict})
}

func newFixtureWith(t *testing.T, settings map[string]string) *fixture {
	t.Helper()
	return newFixtureFull(t, settings, nil)
}

// newFixtureRunner attaches a sub-agent runner, which is the only way
// devcouncil_spawn_subagents can do anything. Without one the tool refuses, and
// that refusal has its own test.
func newFixtureRunner(t *testing.T, runner SubAgentRunner) *fixture {
	t.Helper()
	return newFixtureFull(t, map[string]string{flags.HarnessPosture: flags.PostureStrict}, runner)
}

func newFixtureFull(t *testing.T, settings map[string]string, runner SubAgentRunner) *fixture {
	t.Helper()

	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "h@test"}, {"config", "user.name", "h"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			testsupport.Unavailable(t, "git is required to exercise the diff and verify path: %v %s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "seed"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}

	binary := storeBinary(t)
	db := filepath.Join(t.TempDir(), "state.sqlite")
	client := store.New(binary, db)

	// Create the schema by touching the store, then insert a task directly —
	// planning is DevCouncil's job, not the harness's.
	if _, err := client.ActiveLeases(context.Background()); err != nil {
		t.Fatalf("store: %v", err)
	}
	insertTask(t, binary, db, "TASK-001", `[{"path":"src/calc.go","allowed_change":"modify"}]`)

	regFlags := flags.New()
	if err := flags.DefineHarnessFlags(regFlags); err != nil {
		t.Fatal(err)
	}
	if err := regFlags.LoadConfig(settings); err != nil {
		t.Fatal(err)
	}
	g, err := gate.New(regFlags, root, nil)
	if err != nil {
		t.Fatal(err)
	}

	// A coverage profile covering the planned file's added lines. Supplying one
	// is what lets the loop test assert a genuinely clean pass; without it the
	// verifier correctly reports every changed file as unmeasured.
	coverage := filepath.Join(t.TempDir(), "cover.out")
	if err := os.WriteFile(coverage,
		[]byte("mode: set\nmanvi/src/calc.go:1.1,50.2 40 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, err := New(Deps{
		Store: client, Gate: g, Root: root, LeaseTTL: 10 * time.Minute,
		VerifierBinary: testsupport.DCVerify(t),
		CoverageFile:   coverage,
		SubAgent:       runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	pipe := tools.NewRegistry(bus.New())
	if err := reg.Register(pipe); err != nil {
		t.Fatal(err)
	}
	return &fixture{reg: reg, pipe: pipe, root: root, db: db, t: t}
}

func storeBinary(t *testing.T) string {
	t.Helper()
	return testsupport.DCStore(t)
}

// insertTask writes a task row with sqlite3 if available, else via the store's
// own connection through a tiny helper invocation.
func insertTask(t *testing.T, binary, db, id, plannedJSON string) {
	t.Helper()
	sql := "INSERT INTO tasks (id,title,description,planned_files_json,status) VALUES ('" +
		id + "','t','d','" + strings.ReplaceAll(plannedJSON, "'", "''") + "','ready');"
	cmd := exec.Command(testsupport.Tool(t, "sqlite3"), db, sql)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seeding a task failed: %v %s", err, out)
	}
}

func (f *fixture) call(name string, args any) tools.Result {
	f.t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.pipe.Run(context.Background(), tools.Call{
		ID: "c1", Name: name, Arguments: raw,
	})
}

func (f *fixture) payload(name string, args any) map[string]any {
	f.t.Helper()
	result := f.call(name, args)
	var out map[string]any
	if err := json.Unmarshal([]byte(result.Text), &out); err != nil {
		f.t.Fatalf("%s returned unparseable payload: %v (%q)", name, err, result.Text)
	}
	return out
}

// TestEveryToolIsRegistered pins the surface an agent sees.
func TestEveryToolIsRegistered(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{
		"devcouncil_next_task", "devcouncil_get_task", "devcouncil_checkout_task",
		"devcouncil_renew_lease", "devcouncil_release_task",
		"devcouncil_policy_check_write", "devcouncil_read_file", "devcouncil_write_file",
		"devcouncil_delete_file", "devcouncil_exec_command", "devcouncil_list_dir", "devcouncil_grep",
		"devcouncil_request_override",
		"devcouncil_get_diff", "devcouncil_verify_task",
		"devcouncil_get_gaps", "devcouncil_get_next_actions",
		"devcouncil_git_status", "devcouncil_git_log", "devcouncil_git_branches",
		"devcouncil_git_show", "devcouncil_git_stage", "devcouncil_git_commit",
		"devcouncil_dev_inspect",
	} {
		if !f.pipe.Has(name) {
			t.Errorf("%s is not registered", name)
		}
	}
	// Search agents get the read-only subset without a second list to maintain.
	readOnly := f.pipe.ReadOnlySchemas()
	if len(readOnly) == 0 || len(readOnly) >= len(f.pipe.Schemas()) {
		t.Fatalf("read-only subset = %d of %d", len(readOnly), len(f.pipe.Schemas()))
	}
}

// TestCheckoutImplementVerifyReleaseLoop is the hero loop, natively.
func TestCheckoutImplementVerifyReleaseLoop(t *testing.T) {
	f := newFixture(t)

	next := f.payload("devcouncil_next_task", map[string]any{})
	if next["task_id"] != "TASK-001" {
		t.Fatalf("next_task = %v", next)
	}

	out := f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})
	if out["acquired"] != true {
		t.Fatalf("checkout = %v", out)
	}
	// The lease token is a credential and must not reach the model's context.
	if _, leaked := out["token"]; leaked {
		t.Fatal("checkout leaked the lease token into the tool payload")
	}

	if res := f.call("devcouncil_write_file", map[string]string{
		"path": "src/calc.go", "content": "package calc\n",
	}); res.IsError {
		t.Fatalf("planned write refused: %s", res.Text)
	}

	report := f.payload("devcouncil_verify_task", map[string]any{})
	if report["passed"] != true {
		t.Fatalf("verify = %v", report)
	}
	// Every gate ran and coverage was supplied, so nothing is degraded. This
	// assertion has now been inverted twice, and each inversion is the point:
	// first the gates were unimplemented and a pass had to say so, then they
	// ran but nothing measured coverage and a pass had to say that. Only now
	// can a pass mean the change was checked and exercised.
	if degraded, _ := report["degraded"].([]any); len(degraded) != 0 {
		t.Fatalf("gates were skipped on a pass: %v", degraded)
	}
	gaps, _ := report["gaps"].([]any)
	for _, raw := range gaps {
		gap, _ := raw.(map[string]any)
		if gap["gap_type"] == "diff_coverage" {
			t.Errorf("the added lines were covered, so there is no coverage gap: %v", gap)
		}
	}

	released := f.payload("devcouncil_release_task", map[string]any{})
	if released["released"] != true {
		t.Fatalf("release = %v", released)
	}
}

// TestWriteWithoutALeaseIsRefused is the reason the lease exists.
func TestWriteWithoutALeaseIsRefused(t *testing.T) {
	f := newFixture(t)
	res := f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": "x"})
	if !res.IsError {
		t.Fatal("a write with no lease must be refused")
	}
	if !strings.Contains(res.Text, "checkout") {
		t.Fatalf("the refusal must name the recovery, got %q", res.Text)
	}
}

// TestUnplannedWriteIsBlockedAndTheAgentCanClearIt is the goal's flexibility
// requirement, end to end through the tool surface.
//
// The path is in a directory no planned file is in. That is load-bearing:
// TASK-001 plans src/calc.go, and a write to src/helper.go is *not* blocked any
// more — the same-directory rung allows it when no repo map is available. This
// test is about the path that is still outside, and about what the agent can do
// with it.
func TestUnplannedWriteIsBlockedAndTheAgentCanClearIt(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	blocked := f.call("devcouncil_write_file", map[string]string{
		"path": "internal/helper.go", "content": "package calc\n",
	})
	if !blocked.IsError {
		t.Fatal("an unplanned write outside every planned directory must be blocked")
	}
	var decision map[string]any
	if err := json.Unmarshal([]byte(blocked.Text), &decision); err != nil {
		t.Fatalf("the refusal must be routable JSON, got %q", blocked.Text)
	}
	if decision["overridable"] != true || decision["suggested_tool"] != "devcouncil_request_override" {
		t.Fatalf("a soft block must tell the agent how to clear it: %v", decision)
	}

	granted := f.payload("devcouncil_request_override", map[string]string{
		"path": "internal/helper.go", "rule": "scope.unplanned",
		"reason": "the fix needs a helper the plan did not enumerate",
	})
	if granted["granted"] != true {
		t.Fatalf("an agent may clear its own unplanned-scope block: %v", granted)
	}
	if granted["scope_persisted"] != true {
		t.Fatalf("the argument must outlive the grant that recorded it: %v", granted)
	}

	allowed := f.call("devcouncil_write_file", map[string]string{
		"path": "internal/helper.go", "content": "package calc\n",
	})
	if allowed.IsError {
		t.Fatalf("the granted write should succeed: %s", allowed.Text)
	}
	// The write is authorised by the scope the override wrote into the task, so
	// it reports as widened rather than as grant-cleared. Either way it must
	// never read as a clean write against the plan.
	if allowed.Widened == "" {
		t.Fatalf("a widened write must say so, not read as a clean write: %+v", allowed)
	}
	if !allowed.Qualified() {
		t.Fatal("a write allowed by runtime-appended scope is not an unqualified pass")
	}
}

// TestSecretPathIsRefusedToTheAgentEntirely: hard rules are not negotiable
// through the tool surface either.
func TestSecretPathIsRefusedToTheAgentEntirely(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	blocked := f.call("devcouncil_write_file", map[string]string{"path": ".env", "content": "K=v"})
	if !blocked.IsError {
		t.Fatal(".env must be refused")
	}

	refusal := f.payload("devcouncil_request_override", map[string]string{
		"path": ".env", "rule": "path.secret", "reason": "I need it",
	})
	if refusal["granted"] != false {
		t.Fatalf("a hard rule must never be granted: %v", refusal)
	}
	if !strings.Contains(refusal["suggested_action"].(string), "never grantable") {
		t.Fatalf("the refusal must say the rule is not negotiable: %v", refusal)
	}
}

// TestOverrideRequiresAReason: an override nobody can review is not worth
// recording.
func TestOverrideRequiresAReason(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})
	res := f.call("devcouncil_request_override", map[string]string{
		"path": "internal/helper.go", "rule": "scope.unplanned", "reason": "   ",
	})
	if !res.IsError {
		t.Fatal("an override with no reason must be refused")
	}
}

// TestOrphanDiffProducesARoutableNextAction: the verifier's finding has to be
// actionable without parsing prose.
func TestOrphanDiffProducesARoutableNextAction(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	// Write outside scope directly, bypassing the gate, to simulate a change
	// that arrived some other way — which is exactly what orphan detection is
	// for.
	if err := os.WriteFile(filepath.Join(f.root, "unplanned.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	report := f.payload("devcouncil_verify_task", map[string]any{})
	if report["passed"] != false {
		t.Fatalf("an orphan file must fail verification: %v", report)
	}
	actions, _ := report["next_actions"].([]any)
	if len(actions) == 0 {
		t.Fatal("a blocking gap must come with a next action")
	}
	first := actions[0].(map[string]any)
	if first["category"] != "scope" || first["suggested_tool"] != "devcouncil_request_override" {
		t.Fatalf("next action = %v; it must be routable on category", first)
	}
}

// TestEmptyDiffIsNotAPass: a task that changed nothing has not been done.
func TestEmptyDiffIsNotAPass(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})
	report := f.payload("devcouncil_verify_task", map[string]any{})
	if report["passed"] != false {
		t.Fatalf("an empty working tree must not verify: %v", report)
	}
}

// TestUnreachableStoreNeverReportsSuccess is the fail-closed rule at the tool
// boundary.
func TestUnreachableStoreNeverReportsSuccess(t *testing.T) {
	f := newFixture(t)
	f.reg.deps.Store = store.New(filepath.Join(t.TempDir(), "absent"), f.db)

	res := f.call("devcouncil_next_task", map[string]any{})
	if !res.IsError {
		t.Fatal("an unreachable store must produce an error result")
	}
	if !strings.Contains(res.Text, "did not run") {
		t.Fatalf("the result must distinguish 'could not check' from 'nothing found': %q", res.Text)
	}
}

// TestDevPostureLetsAnUnplannedWriteThroughAndSaysSo is the shipped default at
// the tool surface. The write must land — a harness under construction cannot
// stop on every scope disagreement — and the result must state that a rule
// fired, so nobody reads the success as scope approval.
func TestDevPostureLetsAnUnplannedWriteThroughAndSaysSo(t *testing.T) {
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	res := f.call("devcouncil_write_file", map[string]string{
		"path": "internal/helper.go", "content": "package calc\n",
	})
	if res.IsError {
		t.Fatalf("dev posture must not block an unplanned write: %s", res.Text)
	}
	if _, err := os.Stat(filepath.Join(f.root, "internal/helper.go")); err != nil {
		t.Fatalf("the write did not reach the filesystem: %v", err)
	}
	if res.Rule != string(policy.RuleUnplannedScope) {
		t.Fatalf("result rule = %q, want the rule that would have blocked recorded on the success", res.Rule)
	}

	// And the hard rules are untouched by the posture.
	secret := f.call("devcouncil_write_file", map[string]string{
		"path": ".env", "content": "TOKEN=abc\n",
	})
	if !secret.IsError {
		t.Fatal("dev posture must not open a credential path")
	}
	if _, err := os.Stat(filepath.Join(f.root, ".env")); err == nil {
		t.Fatal("a refused write reached the filesystem")
	}
}

// TestTheRigorGatesActuallyRun is the test that makes `passed: true` mean
// something. The gates used to be listed as unimplemented, and a report that
// names its own blind spots is honest but not useful — this asserts the content
// gates reach the diff and produce routable findings.
func TestTheRigorGatesActuallyRun(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	// A planned file, so scope is clean and the only findings can come from the
	// content gates.
	if res := f.call("devcouncil_write_file", map[string]string{
		"path": "src/calc.go",
		"content": "package calc\n\n" +
			"// TODO: rotate this before shipping\n" +
			"const key = \"sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAA\"\n",
	}); res.IsError {
		t.Fatalf("planned write refused: %s", res.Text)
	}

	report := f.payload("devcouncil_verify_task", map[string]any{})
	if report["passed"] == true {
		t.Fatalf("a diff containing a credential passed verification: %v", report)
	}

	gaps, _ := report["gaps"].([]any)
	kinds := map[string]bool{}
	var secretDetail string
	for _, raw := range gaps {
		gap, _ := raw.(map[string]any)
		kind, _ := gap["gap_type"].(string)
		kinds[kind] = true
		if kind == "secret_scan" {
			secretDetail, _ = gap["description"].(string)
			if gap["blocking"] != true {
				t.Errorf("a credential finding must block: %v", gap)
			}
		}
	}
	for _, want := range []string{"secret_scan", "stub_detection"} {
		if !kinds[want] {
			t.Errorf("the %s gate produced no finding; gaps were %v", want, kinds)
		}
	}
	// diff_coverage produces no finding here, and that is the correct outcome
	// rather than a missing gate: the fixture supplies a profile covering this
	// file. What must hold is that it *ran* — which is what an empty degraded
	// list below asserts.

	// The finding must identify the credential without reproducing it — this
	// report is written to the evidence trail and the session log.
	if secretDetail == "" {
		t.Fatal("no secret finding detail")
	}
	if strings.Contains(secretDetail, "AAAAAAAAAAAAAAAAAAAAAAAAAAAA") {
		t.Fatalf("the gap description reproduced the credential: %q", secretDetail)
	}
	if !strings.Contains(secretDetail, "sk-ant-") {
		t.Fatalf("the gap description does not identify the shape: %q", secretDetail)
	}

	// And the degraded list is now empty for these gates, because they ran.
	degraded, _ := report["degraded"].([]any)
	for _, raw := range degraded {
		if entry, _ := raw.(string); strings.Contains(entry, "secret_scan") {
			t.Errorf("secret_scan is still reported as degraded: %q", entry)
		}
	}

	// Every finding routes: an agent must be able to act on it without parsing prose.
	actions, _ := report["next_actions"].([]any)
	if len(actions) < len(gaps) {
		t.Fatalf("%d gaps produced only %d next actions", len(gaps), len(actions))
	}
}

// TestAnUnreachableVerifierIsDegradedNotAPass: the failure that matters is the
// one where the gates silently stop running.
func TestAnUnreachableVerifierIsDegradedNotAPass(t *testing.T) {
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureStrict})
	f.reg.deps.VerifierBinary = filepath.Join(t.TempDir(), "no-such-verifier")
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})
	if res := f.call("devcouncil_write_file", map[string]string{
		"path": "src/calc.go", "content": "package calc\n",
	}); res.IsError {
		t.Fatalf("planned write refused: %s", res.Text)
	}

	report := f.payload("devcouncil_verify_task", map[string]any{})
	degraded, _ := report["degraded"].([]any)
	if len(degraded) == 0 {
		t.Fatal("an unreachable verifier produced no degradation; its silence read as a pass")
	}
	joined := fmt.Sprint(degraded...)
	for _, gate := range []string{"secret_scan", "stub_detection", "diff_coverage"} {
		if !strings.Contains(joined, gate) {
			t.Errorf("the degradation does not name %s: %v", gate, degraded)
		}
	}
}

// TestNoCoverageFileIsReportedNotAssumed: without measurements every changed
// file is "unmeasured", and a report that did not say why would look like a
// finding about the code rather than about the pipeline.
func TestNoCoverageFileIsReportedNotAssumed(t *testing.T) {
	f := newFixture(t)
	f.reg.deps.CoverageFile = ""
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})
	if res := f.call("devcouncil_write_file", map[string]string{
		"path": "src/calc.go", "content": "package calc\n\nfunc Add(a, b int) int { return a + b }\n",
	}); res.IsError {
		t.Fatalf("planned write refused: %s", res.Text)
	}

	report := f.payload("devcouncil_verify_task", map[string]any{})
	degraded, _ := report["degraded"].([]any)
	if len(degraded) == 0 {
		t.Fatal("a run with no coverage measurements reported none of that")
	}
	if !strings.Contains(fmt.Sprint(degraded...), "no coverage file") {
		t.Fatalf("degraded = %v, want it to name the missing measurements", degraded)
	}

	unmeasured := false
	gaps, _ := report["gaps"].([]any)
	for _, raw := range gaps {
		gap, _ := raw.(map[string]any)
		if gap["gap_type"] == "diff_coverage" {
			unmeasured = true
			if gap["blocking"] == true {
				t.Errorf("unmeasured coverage must not block: %v", gap)
			}
		}
	}
	if !unmeasured {
		t.Fatal("the changed file must be reported as unmeasured")
	}
}

// TestBrokenCoverageFileDoesNotBecomeZeroCoverage: an unreadable measurement
// file must degrade the gate, not report every added line as unexercised.
func TestBrokenCoverageFileDoesNotBecomeZeroCoverage(t *testing.T) {
	f := newFixture(t)
	broken := filepath.Join(t.TempDir(), "broken.out")
	if err := os.WriteFile(broken, []byte("{\"coverage\": 91.2}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.reg.deps.CoverageFile = broken
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})
	if res := f.call("devcouncil_write_file", map[string]string{
		"path": "src/calc.go", "content": "package calc\n",
	}); res.IsError {
		t.Fatalf("planned write refused: %s", res.Text)
	}

	report := f.payload("devcouncil_verify_task", map[string]any{})
	degraded, _ := report["degraded"].([]any)
	if len(degraded) == 0 {
		t.Fatal("an unreadable coverage file produced no degradation")
	}
	// And no gate produced findings from it, because none of them ran.
	if !strings.Contains(fmt.Sprint(degraded...), "did not run") {
		t.Fatalf("degraded = %v, want the gates reported as not having run", degraded)
	}
}

func TestExecCommandLifecycle(t *testing.T) {
	f := newFixture(t)

	// Command without a task checkout: orientation command passes
	res := f.call("devcouncil_exec_command", map[string]string{"command": "git status"})
	if res.IsError {
		t.Fatalf("git status without lease should be allowed, got error: %s", res.Text)
	}
	if !strings.Contains(res.Text, "exit code 0") {
		t.Fatalf("expected exit code 0, got: %s", res.Text)
	}

	// Arbitrary command without lease: refused
	resBlocked := f.call("devcouncil_exec_command", map[string]string{"command": "ls -la"})
	if !resBlocked.IsError {
		t.Fatalf("arbitrary command without lease must be refused, got success: %s", resBlocked.Text)
	}

	// Checkout task
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	// Allowlisted task command
	resAllowed := f.call("devcouncil_exec_command", map[string]string{"command": "git status"})
	if resAllowed.IsError {
		t.Fatalf("allowlisted command with lease should succeed: %s", resAllowed.Text)
	}

	// Dangerous command: force push blocked by git safety
	resForce := f.call("devcouncil_exec_command", map[string]string{"command": "git push -f origin main"})
	if !resForce.IsError {
		t.Fatalf("force push must be blocked by git safety, got: %s", resForce.Text)
	}
}

func TestDeleteFileLifecycle(t *testing.T) {
	f := newFixture(t)

	// Delete without a lease: refused
	resNoLease := f.call("devcouncil_delete_file", map[string]string{"path": "seed.txt"})
	if !resNoLease.IsError {
		t.Fatalf("delete without lease must be refused")
	}

	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	// Create an extra file on disk first
	if err := os.MkdirAll(filepath.Join(f.root, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "internal", "extra.go"), []byte("package internal\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Deleting an unplanned file is blocked by scope.unplanned
	resDel := f.call("devcouncil_delete_file", map[string]string{"path": "internal/extra.go"})
	if !resDel.IsError {
		t.Fatalf("deleting an unplanned file without grant should be blocked")
	}

	// Grant override for unplanned file deletion
	resReq := f.payload("devcouncil_request_override", map[string]string{
		"path": "internal/extra.go", "rule": "scope.unplanned", "reason": "delete temporary file",
	})
	if resReq["granted"] != true {
		t.Fatalf("granting override failed: %v", resReq)
	}

	// Delete now succeeds
	resDel2 := f.call("devcouncil_delete_file", map[string]string{"path": "internal/extra.go"})
	if resDel2.IsError {
		t.Fatalf("delete after grant failed: %s", resDel2.Text)
	}
	if _, err := os.Stat(filepath.Join(f.root, "internal/extra.go")); !os.IsNotExist(err) {
		t.Fatalf("file still exists on disk after deletion")
	}

	// Deleting secret path is hard blocked
	resSecret := f.call("devcouncil_delete_file", map[string]string{"path": ".env"})
	if !resSecret.IsError || resSecret.Severity != "hard" {
		t.Fatalf("deleting secret path must be hard blocked: %+v", resSecret)
	}
}

func TestListDirAndGrep(t *testing.T) {
	f := newFixture(t)

	// Create some files
	subDir := filepath.Join(f.root, "pkg", "math")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "calc.go"), []byte("package math\nconst Pi = 3.14159\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// listDir non-recursive
	listRes := f.payload("devcouncil_list_dir", map[string]any{"path": "pkg/math"})
	count, _ := listRes["count"].(float64)
	if count != 1 {
		t.Fatalf("list_dir count = %v, want 1", count)
	}

	// listDir recursive
	listRec := f.payload("devcouncil_list_dir", map[string]any{"recursive": true})
	recCount, _ := listRec["count"].(float64)
	if recCount < 2 {
		t.Fatalf("recursive count = %v, want >= 2", recCount)
	}

	// grepSearch finding Pi
	grepRes := f.payload("devcouncil_grep", map[string]any{"pattern": "Pi = 3.14159"})
	matchCount, _ := grepRes["count"].(float64)
	if matchCount != 1 {
		t.Fatalf("grep match count = %v, want 1", matchCount)
	}

	// grepSearch non-existent pattern
	grepNone := f.payload("devcouncil_grep", map[string]any{"pattern": "NON_EXISTENT_PATTERN_XYZ"})
	if grepNone["count"].(float64) != 0 {
		t.Fatalf("expected 0 matches, got %v", grepNone["count"])
	}
}

// TestYoloDoesNotShutTheGatedWritePathWhileLeavingTheShellOpen is the
// regression test for an inversion that made --yolo actively less safe than
// the posture it relaxes.
//
// Before the fix, writeFile and deleteFile refused a missing lease themselves,
// up front, with severity "hard" — a check the posture engine never saw. The
// command tool did not: it passed a nil task to the gate, which produced a
// soft rule that yolo demotes. So in the posture whose whole meaning is "the
// gate is not containing the agent", `devcouncil_write_file` was refused while
// `devcouncil_exec_command` would run `printf ... > file` quite happily.
//
// The consequence was not merely inconsistent, it was backwards: a model that
// hit the refusal would route around it through the shell, which is the one
// path that records no policy decision, produces no tracked diff, and cannot
// be verified afterwards. The harness was steering work off the audited path
// onto the unaudited one.
//
// The assertion is therefore about symmetry, not about either tool alone: the
// two must agree, in both postures.
func TestYoloDoesNotShutTheGatedWritePathWhileLeavingTheShellOpen(t *testing.T) {
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureYolo})

	write := f.call("devcouncil_write_file", map[string]string{
		"path": "unplanned.txt", "content": "written under yolo\n",
	})
	if write.IsError {
		t.Fatalf("yolo must not refuse a gated write it would allow through the shell: %s", write.Text)
	}

	exec := f.call("devcouncil_exec_command", map[string]string{
		"command": "printf 'written by shell\n' > shell.txt",
	})
	if exec.IsError {
		t.Fatalf("the shell path was already open under yolo; it must stay open: %s", exec.Text)
	}
}

// TestStrictRefusesTheWriteAndTheShellAlike is the other half of the same
// invariant. Relaxing yolo must not have relaxed strict: with no lease, both
// mutating paths are still refused, and the file on disk is untouched.
func TestStrictRefusesTheWriteAndTheShellAlike(t *testing.T) {
	f := newFixture(t) // strict

	write := f.call("devcouncil_write_file", map[string]string{
		"path": "unplanned.txt", "content": "should not land\n",
	})
	if !write.IsError {
		t.Fatal("strict must still refuse a write with no lease")
	}
	if _, err := os.Stat(filepath.Join(f.root, "unplanned.txt")); !os.IsNotExist(err) {
		t.Fatal("a refused write must not have touched the filesystem")
	}

	exec := f.call("devcouncil_exec_command", map[string]string{
		"command": "printf 'should not land\n' > shell.txt",
	})
	if !exec.IsError {
		t.Fatal("strict must still refuse a command with no lease")
	}
	if _, err := os.Stat(filepath.Join(f.root, "shell.txt")); !os.IsNotExist(err) {
		t.Fatal("a refused command must not have run")
	}
}

// TestLeaselessRefusalRoutesToCheckoutNotOverride pins the recovery an agent is
// told to take. Both are soft blocks, so severity cannot distinguish them: an
// agent that holds a task and strayed out of scope should argue for the
// exception, while one holding no task at all should take one. Naming
// request_override for a missing checkout sends the model to negotiate about
// work it never claimed, which is a turn spent going the wrong way.
func TestLeaselessRefusalRoutesToCheckoutNotOverride(t *testing.T) {
	f := newFixture(t) // strict

	res := f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": "x"})
	if !res.IsError {
		t.Fatal("a write with no lease must be refused under strict")
	}
	var decision map[string]any
	if err := json.Unmarshal([]byte(res.Text), &decision); err != nil {
		t.Fatalf("the refusal must be routable JSON, got %q", res.Text)
	}
	if decision["rule"] != "task.absent" {
		t.Fatalf("the gate, not the tool, must own this refusal: %v", decision)
	}
	if decision["suggested_tool"] != "devcouncil_checkout_task" {
		t.Fatalf("a missing lease must route to checkout, got %v", decision["suggested_tool"])
	}
}

// TestAnExpiredLeaseStaysHardInEveryPosture guards the line the fix must not
// cross. An absent lease is a scope question the posture may relax; an expired
// one is not. It is a lease another agent may already hold, and writing under
// it is how two builders come to believe they own the same file. No posture,
// including yolo, softens that.
func TestAnExpiredLeaseStaysHardInEveryPosture(t *testing.T) {
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureYolo})
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	// Steal the lease out from under the session, which is what an expiry or a
	// forced hand-off looks like from inside the tool: the token it holds is
	// still well-formed, and is no longer the one the store recognises.
	client := store.New(storeBinary(t), f.db)
	if _, err := client.Acquire(context.Background(), store.AcquireRequest{
		TaskID: "TASK-001", Owner: "someone-else", TTL: 10 * time.Minute, Force: true,
	}); err != nil {
		t.Fatalf("stealing the lease: %v", err)
	}

	res := f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": "x"})
	if !res.IsError {
		t.Fatal("a write under a lease another owner now holds must be refused even under yolo")
	}
	if res.Rule != "lease.invalid" {
		t.Fatalf("the refusal must name the invalid lease, got rule %q: %s", res.Rule, res.Text)
	}
}

func TestPatchFileExactReplacement(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	// Write initial content
	f.call("devcouncil_write_file", map[string]string{
		"path":    "src/calc.go",
		"content": "package calc\n\nfunc Add(a, b int) int {\n\treturn a - b // bug\n}\n",
	})

	// Patch the bug
	res := f.call("devcouncil_patch_file", map[string]any{
		"path":                "src/calc.go",
		"target_content":      "return a - b // bug",
		"replacement_content": "return a + b",
	})
	if res.IsError {
		t.Fatalf("patch should succeed, got error: %s", res.Text)
	}

	readRes := f.call("devcouncil_read_file", map[string]string{"path": "src/calc.go"})
	if !strings.Contains(readRes.Text, "return a + b") {
		t.Fatalf("expected patched content, got: %s", readRes.Text)
	}
}

func TestPatchFileLineBoundsAndUniqueness(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	// File with duplicate tokens on different lines
	f.call("devcouncil_write_file", map[string]string{
		"path":    "src/calc.go",
		"content": "line1 foo\nline2 bar\nline3 foo\nline4 baz\n",
	})

	// Multiple matches without allow_multiple should error
	resDup := f.call("devcouncil_patch_file", map[string]any{
		"path":                "src/calc.go",
		"target_content":      "foo",
		"replacement_content": "qux",
	})
	if !resDup.IsError {
		t.Fatal("expected error on multiple occurrences without allow_multiple")
	}

	// Targeted replacement using start_line and end_line
	resBounded := f.call("devcouncil_patch_file", map[string]any{
		"path":                "src/calc.go",
		"target_content":      "foo",
		"replacement_content": "qux",
		"start_line":          1,
		"end_line":            2,
	})
	if resBounded.IsError {
		t.Fatalf("bounded patch should succeed, got error: %s", resBounded.Text)
	}

	readRes := f.call("devcouncil_read_file", map[string]string{"path": "src/calc.go"})
	expected := "line1 qux\nline2 bar\nline3 foo\nline4 baz\n"
	if readRes.Text != expected {
		t.Fatalf("expected %q, got %q", expected, readRes.Text)
	}
}

func TestFindFilesGlobMatching(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	// Create some files
	f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": "package calc"})

	res := f.call("devcouncil_find_files", map[string]any{
		"pattern": "*.go",
	})
	if res.IsError {
		t.Fatalf("find_files failed: %s", res.Text)
	}

	var data struct {
		Count int      `json:"count"`
		Files []string `json:"files"`
	}
	if err := json.Unmarshal([]byte(res.Text), &data); err != nil {
		t.Fatalf("unmarshal find_files result: %v", err)
	}
	if data.Count < 1 || len(data.Files) < 1 {
		t.Fatalf("expected at least 1 match for *.go, got %d", data.Count)
	}
}

// recordingRunner stands in for a real sub-agent: it records what it was asked
// to do, so a test can tell delegation from the appearance of delegation.
type recordingRunner struct {
	mu       sync.Mutex
	seen     []SubAgentRequest
	fail     error
	emptyOut bool
}

func (r *recordingRunner) RunSubAgent(ctx context.Context, req SubAgentRequest) (SubAgentResult, error) {
	r.mu.Lock()
	r.seen = append(r.seen, req)
	r.mu.Unlock()
	if r.fail != nil {
		return SubAgentResult{}, r.fail
	}
	if r.emptyOut {
		return SubAgentResult{}, nil
	}
	return SubAgentResult{Summary: "findings for " + req.Label, Steps: 3}, nil
}

// TestSpawnSubagentsDeliversThePromptAndReportsWhatCameBack replaces an
// assertion that pinned the stub as correct.
//
// The test that stood here checked Children==2 and Failed==0 against a handler
// that never read `prompt` and never ran anything, so it passed precisely
// because the tool fabricated success — it encoded the defect as the
// expectation. Counting children proves only that the pool ran; what has to be
// proved is that each child received the instruction and that its output is
// what the caller reads back.
func TestSpawnSubagentsDeliversThePromptAndReportsWhatCameBack(t *testing.T) {
	runner := &recordingRunner{}
	f := newFixtureRunner(t, runner)

	res := f.call("devcouncil_spawn_subagents", map[string]any{
		"tasks": []map[string]any{
			{"label": "worker-1", "prompt": "analyze structure", "read_only": true},
			{"label": "worker-2", "prompt": "check dependencies"},
		},
	})
	if res.IsError {
		t.Fatalf("spawn_subagents failed: %s", res.Text)
	}

	// The prompts must have arrived. This is the assertion the old handler
	// could never have passed: it decoded prompt and dropped it on the floor.
	delivered := map[string]SubAgentRequest{}
	for _, req := range runner.seen {
		delivered[req.Label] = req
	}
	if len(delivered) != 2 {
		t.Fatalf("the runner was asked for %d children, want 2: %+v", len(delivered), runner.seen)
	}
	if got := delivered["worker-1"].Prompt; got != "analyze structure" {
		t.Errorf("worker-1 prompt = %q, want the prompt the caller sent", got)
	}
	if !delivered["worker-1"].ReadOnly {
		t.Error("read_only was decoded and then not passed to the runner")
	}
	if got := delivered["worker-2"].Prompt; got != "check dependencies" {
		t.Errorf("worker-2 prompt = %q, want the prompt the caller sent", got)
	}

	// And what each child produced must reach the dispatching agent. The old
	// payload carried none of it: agents.Result tags Value `json:"-"`, so the
	// whole fan-out arrived as a count of children that had not failed.
	var data struct {
		Report struct {
			Children int `json:"children"`
			Failed   int `json:"failed"`
		} `json:"report"`
		Results []struct {
			Label   string `json:"label"`
			Status  string `json:"status"`
			Summary string `json:"summary"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Text), &data); err != nil {
		t.Fatalf("unmarshal spawn_subagents: %v", err)
	}
	if data.Report.Children != 2 || data.Report.Failed != 0 {
		t.Fatalf("expected 2 successful children, got %+v", data.Report)
	}
	for _, out := range data.Results {
		if out.Status != "completed" {
			t.Errorf("%s: status = %q", out.Label, out.Status)
		}
		if out.Summary != "findings for "+out.Label {
			t.Errorf("%s: summary = %q, want the runner's own output", out.Label, out.Summary)
		}
	}
}

// TestSpawnSubagentsReportsAChildThatFailedAsFailed: the counterpart. A runner
// that errors, or one that comes back with nothing to say, must not be folded
// into the completions.
func TestSpawnSubagentsReportsAChildThatFailedAsFailed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runner *recordingRunner
	}{
		{"the runner errored", &recordingRunner{fail: errors.New("provider refused")}},
		{"the runner produced nothing", &recordingRunner{emptyOut: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixtureRunner(t, tc.runner)
			payload := f.payload("devcouncil_spawn_subagents", map[string]any{
				"tasks": []map[string]any{{"label": "worker-1", "prompt": "analyze structure"}},
			})
			report, _ := payload["report"].(map[string]any)
			if failed, _ := report["failed"].(float64); failed != 1 {
				t.Fatalf("a child that produced no work must be reported failed: %v", payload)
			}
			if clean, _ := payload["clean"].(bool); clean {
				t.Fatalf("a fan-out with a failed child is not clean: %v", payload)
			}
		})
	}
}

// TestSpawnSubagentsRefusesAnEmptyPromptBeforeTakingALease: the schema marks
// prompt required, but a schema is a request to the model rather than a
// guarantee about what arrives, and a child dispatched with nothing to do can
// only report that it finished.
func TestSpawnSubagentsRefusesAnEmptyPromptBeforeTakingALease(t *testing.T) {
	runner := &recordingRunner{}
	f := newFixtureRunner(t, runner)

	res := f.call("devcouncil_spawn_subagents", map[string]any{
		"tasks": []map[string]any{
			{"label": "worker-1", "prompt": "real work", "task_id": "TASK-001"},
			{"label": "worker-2", "prompt": "   "},
		},
	})
	if !res.IsError {
		t.Fatalf("an empty prompt must be refused: %q", res.Text)
	}
	if len(runner.seen) != 0 {
		t.Fatalf("the refusal ran %d children first: %+v", len(runner.seen), runner.seen)
	}
	// Validation happens before acquisition, so the well-formed sibling's task
	// is not left locked against other builders on behalf of a refused call.
	leases, err := f.reg.deps.Store.ActiveLeases(context.Background())
	if err != nil {
		t.Fatalf("reading leases: %v", err)
	}
	if len(leases) != 0 {
		t.Fatalf("a refused fan-out stranded %d lease(s): %+v", len(leases), leases)
	}
}

// TestGrepUnderstandsARegularExpression is the regression test for a wrong
// answer that read exactly like a right one.
//
// grep matched with strings.Contains while calling its argument a pattern, so
// every alternation returned {"count":0} — indistinguishable from a genuine
// absence. A local model asked to remove unused imports was told by this tool
// that `sys|time` appeared nowhere, and then that `subprocess|re|os` appeared
// nowhere either, in a file that plainly imported all five. It spent its whole
// step budget trying to reconcile the two, and the file was never edited.
func TestGrepUnderstandsARegularExpression(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.root, "mod.py"),
		[]byte("import subprocess\nimport re\nimport os\n\nvalue = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hit := f.payload("devcouncil_grep", map[string]any{"pattern": "subprocess|re|os", "path": "mod.py"})
	count, _ := hit["count"].(float64)
	if count == 0 {
		t.Fatalf("an alternation over three imports the file has must match: %v", hit)
	}

	// And the negative must still be a real negative, or the fix would have
	// replaced one useless answer with another.
	miss := f.payload("devcouncil_grep", map[string]any{"pattern": "sys|time", "path": "mod.py"})
	if missed, _ := miss["count"].(float64); missed != 0 {
		t.Fatalf("neither sys nor time appears in the file: %v", miss)
	}
}

// TestGrepRefusesAnUnparseablePatternRatherThanReportingNoMatches is the
// invariant the bug violated: a search that could not run must never return the
// same answer as a search that ran and found nothing.
func TestGrepRefusesAnUnparseablePatternRatherThanReportingNoMatches(t *testing.T) {
	f := newFixture(t)
	res := f.call("devcouncil_grep", map[string]any{"pattern": "unclosed(group"})
	if !res.IsError {
		t.Fatalf("an invalid pattern must be an error, not an empty result: %q", res.Text)
	}
	if !strings.Contains(res.Text, "not a negative result") {
		t.Fatalf("the error must say no search ran, got %q", res.Text)
	}
}

// TestSpawnSubagentsNeverReportsWorkItDidNotDo is the fabricated-success
// regression.
//
// devcouncil_spawn_subagents decoded `prompt`, which its schema marks required,
// and then never read it. Its worker took a lease and returned
// {"status":"completed","summary":"sub-agent X processed prompt"} without a
// model ever having been asked anything. An agent that fanned four analyses out
// received four completions and reported the work as done — the exact failure
// this package's doc-comment claims cannot happen, since a tool that could not
// run had reported success.
//
// With no sub-agent runner configured there is nothing to delegate to, and the
// only honest answer is an error.
func TestSpawnSubagentsNeverReportsWorkItDidNotDo(t *testing.T) {
	f := newFixture(t)

	res := f.call("devcouncil_spawn_subagents", map[string]any{
		"tasks": []map[string]any{
			{"label": "worker-1", "prompt": "analyze structure"},
		},
	})
	if !res.IsError {
		t.Fatalf("with no runner attached, dispatch must fail loudly: %q", res.Text)
	}
	if strings.Contains(res.Text, "completed") {
		t.Fatalf("a dispatch that ran no model must not report completion: %q", res.Text)
	}
}

// devcouncil_invoke_subagent must not report completed work either.
//
// The fix that stopped devcouncil_spawn_subagents fabricating success left an
// identical branch standing in the other dispatcher: with no runner attached it
// set the instance to StateCompleted and returned status "completed" with a
// summary it composed from the prompt — "subagent %s finished executing task:
// %s" — for a child that had never existed. Two dispatchers, one defect; a fix
// applied to one of them is not a fix.
func TestInvokeSubagentDoesNotReportWorkItDidNotDo(t *testing.T) {
	f := newFixture(t) // no runner attached

	if res := f.call("devcouncil_define_subagent", map[string]any{
		"name":          "analyst",
		"description":   "reads code",
		"system_prompt": "you analyse",
	}); res.IsError {
		t.Fatalf("defining a subagent type failed: %s", res.Text)
	}

	res := f.call("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{{
			"type_name": "analyst",
			"role":      "parser-review",
			"prompt":    "review the parser and report what you find",
		}},
	})

	// However this surfaces — an error result, or a payload reporting the
	// failure — it must not claim the work completed.
	if strings.Contains(res.Text, `"status": "completed"`) ||
		strings.Contains(res.Text, `"status":"completed"`) {
		t.Errorf("reported completed work with no runner attached: %s", res.Text)
	}
	if strings.Contains(res.Text, "finished executing task") {
		t.Errorf("fabricated a summary for a child that never ran: %s", res.Text)
	}
	if !res.IsError && !strings.Contains(res.Text, "failed") && !strings.Contains(res.Text, "no model") {
		t.Errorf("a dispatch with no runner did not report a failure: %s", res.Text)
	}
}

// TestAWhollyFailedFanOutIsAnErrorResult.
//
// This package has one rule for a check that did not succeed — "a failed check
// is an error result, never an empty success" (see unavailable) — and the
// fan-out returned ok() regardless of how its children fared. Two consequences:
// the model read a total failure as a completed delegation, and because a
// non-error result from a mutating tool is credited as progress, the
// no-progress detector reset its streak, so a model delegating into repeated
// failures was never interrupted.
func TestAWhollyFailedFanOutIsAnErrorResult(t *testing.T) {
	f := newFixtureRunner(t, &recordingRunner{fail: errors.New("the child could not start")})

	res := f.call("devcouncil_spawn_subagents", map[string]any{
		"tasks": []map[string]any{
			{"label": "worker-1", "prompt": "do a thing"},
			{"label": "worker-2", "prompt": "do another thing"},
		},
	})
	if !res.IsError {
		t.Fatalf("a fan-out in which every child failed reported success: %s", res.Text)
	}
	// Still parseable: Result.Text on these tools is JSON the model reads, so
	// the reason goes inside the payload rather than in front of it.
	var payload map[string]any
	if err := json.Unmarshal([]byte(res.Text), &payload); err != nil {
		t.Fatalf("the failure payload is not valid JSON, so the model cannot read it: %v (%s)", err, res.Text)
	}
	if payload["clean"] != false {
		t.Errorf("clean = %v, want false", payload["clean"])
	}
	if _, ok := payload["error"]; !ok {
		t.Error("the payload carries no error key saying why the dispatch failed")
	}
}
