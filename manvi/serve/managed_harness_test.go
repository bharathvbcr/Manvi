package serve

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
	"github.com/bharathvbcr/Manvi/manvi/harness"
)

// These tests run a managed attempt over a real task store (the store-backed
// ones; see managed_lifecycle_test.go) in a real git work tree, with a harness.

func harnessLifecycle(t *testing.T, root, provider string, reads *atomic.Value, starts *atomic.Int32) *managedLifecycle {
	t.Helper()
	instruction, _ := harnessInstructionFile(provider)
	var m *managedLifecycle
	m = newManagedLifecycleAt(t, root, provider, func(ctx context.Context, o codingagent.Options) *managedTestSession {
		starts.Add(1)
		// What the CLI would find when it starts: read here, before the session
		// exists, and judged by the test afterwards.
		b, _ := os.ReadFile(filepath.Join(root, instruction)) // #nosec G304 -- a file under this test's own t.TempDir
		reads.Store(string(b))
		s := m.fixtureSession(ctx, o)
		s.withRequest = false
		return s
	})
	resolver := newTestResolver(t)
	bad := harness.Spec{Name: "bad", Sections: []harness.Section{{Name: "policy", Order: 70, Text: "takes an essential name"}}}
	if err := resolver.Catalogue.Register(bad); err != nil {
		t.Fatal(err)
	}
	if err := m.runner.SetHarness(resolver); err != nil {
		t.Fatal(err)
	}
	return m
}

func preparing(requestID, harnessMember string) json.RawMessage {
	member := ""
	if harnessMember != "" {
		member = `,"harness":` + harnessMember
	}
	return json.RawMessage(`{"id":"run","request_id":"` + requestID + `","expected_revision":1` + member + `}`)
}

func activate(t *testing.T, m *managedLifecycle, p ManagedPrepared) {
	t.Helper()
	raw := `{"id":"run","owner_id":"` + p.Owner + `","session_id":"` + p.Session + `","process_id":123,"process_start":"fixture-native-observation"}`
	if _, err := m.runner.Activate(t.Context(), json.RawMessage(raw)); err != nil {
		t.Fatal(err)
	}
}

func TestManagedHarnessIsWrittenBeforeTheProcessAndRestoredWhenTheRunEnds(t *testing.T) {
	root := scanRepo(t, map[string]string{"CLAUDE.md": "project rules\n"})
	var atStart atomic.Value
	var starts atomic.Int32
	m := harnessLifecycle(t, root, "claude", &atStart, &starts)

	prepared, err := m.runner.Prepare(t.Context(), preparing("launch", `{"kind":"named","name":"registered"}`))
	if err != nil {
		t.Fatal(err)
	}
	spec, _, err := harness.Resolve("registered", m.runner.harness.catalogue())
	if err != nil {
		t.Fatal(err)
	}
	if prepared.HarnessDigest != spec.Digest() {
		t.Fatalf("prepared digest = %q, want the spec's %q", prepared.HarnessDigest, spec.Digest())
	}
	if started, _ := atStart.Load().(string); !strings.Contains(started, harnessBeginMarker) || !strings.HasPrefix(started, "project rules\n") {
		t.Fatalf("the harness was not in the instruction file when the process started:\n%s", started)
	}
	activate(t, m, prepared)
	run, raw := m.finished()
	if run.State != "exited" {
		t.Fatalf("the run ended %s: %s", run.State, raw)
	}

	if got := readFile(t, root, "CLAUDE.md"); got != "project rules\n" {
		t.Fatalf("the checkout was not given back as it was:\n%s", got)
	}
	if status := gitIn(t, root, "status", "--porcelain"); status != "" {
		t.Fatalf("the run's diff carries the harness: %q", status)
	}
	var configuration map[string]any
	if err := json.Unmarshal([]byte(run.Configuration), &configuration); err != nil {
		t.Fatalf("effective_configuration is no longer a JSON object: %v", err)
	}
	if configuration["harness_digest"] != spec.Digest() {
		t.Fatalf("effective configuration carries %v, want the digest %s", configuration["harness_digest"], spec.Digest())
	}
	for _, key := range []string{"cwd", "sandbox", "model"} {
		if _, ok := configuration[key]; !ok {
			t.Errorf("the adapter's own description lost %q", key)
		}
	}
	turn, _ := m.session.sent.Load().(string)
	if !strings.Contains(turn, "CLAUDE.md") || !strings.Contains(turn, "BEGIN MANVI HARNESS") {
		t.Fatalf("the turn does not tell the agent where the harness is:\n%s", turn)
	}
}

// An attempt with no harness is what it always was: nothing written, nothing in
// the configuration, nothing added to the turn.
func TestManagedAttemptWithoutAHarnessIsUnchanged(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	var atStart atomic.Value
	var starts atomic.Int32
	m := harnessLifecycle(t, root, "codex", &atStart, &starts)
	prepared, err := m.runner.Prepare(t.Context(), preparing("launch", ""))
	if err != nil {
		t.Fatal(err)
	}
	if prepared.HarnessDigest != "" {
		t.Fatalf("a digest appeared with no harness: %q", prepared.HarnessDigest)
	}
	activate(t, m, prepared)
	run, _ := m.finished()
	if strings.Contains(run.Configuration, "harness_digest") {
		t.Fatalf("the configuration names a harness there was none of: %s", run.Configuration)
	}
	if turn, _ := m.session.sent.Load().(string); strings.Contains(turn, "MANVI HARNESS") {
		t.Fatalf("the turn mentions a harness there was none of:\n%s", turn)
	}
	if got := readFile(t, root, "AGENTS.md"); got != "rules\n" {
		t.Fatalf("an attempt with no harness changed the checkout: %q", got)
	}
}

// A harness that cannot be written aborts the run: no process starts, the
// failure is typed, the run is recorded failed, and the file is as it was.
func TestManagedFailedMaterialisationAbortsTheRunWithoutAProcess(t *testing.T) {
	root := scanRepo(t, map[string]string{"README.md": "x"})
	outside := filepath.Join(t.TempDir(), "elsewhere.md")
	if err := os.WriteFile(outside, []byte("not yours\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	var atStart atomic.Value
	var starts atomic.Int32
	m := harnessLifecycle(t, root, "claude", &atStart, &starts)

	_, err := m.runner.Prepare(t.Context(), preparing("launch", `{"kind":"named","name":"registered"}`))
	if err == nil {
		t.Fatal("a run whose harness could not be written was prepared")
	}
	var typed *harnessMaterialiseError
	if !errors.As(err, &typed) {
		t.Fatalf("the failure is not the typed materialisation error: %T %v", err, err)
	}
	if got := workbenchHostError("managed preparation", err); got.Code != ErrHarnessMaterialise {
		t.Fatalf("the host sees code %s, want %s", got.Code, ErrHarnessMaterialise)
	}
	if starts.Load() != 0 {
		t.Fatal("a process was started for a run that could not carry its harness")
	}
	run, raw := m.finished()
	if run.State != "failed" {
		t.Fatalf("the aborted run is recorded %s, want failed: %s", run.State, raw)
	}
	// #nosec G304 -- a file under this test's own t.TempDir.
	if got, _ := os.ReadFile(outside); string(got) != "not yours\n" {
		t.Fatalf("the link's target was written: %q", got)
	}
}

// A refused harness is refused before anything is claimed: the saved attempt
// stays prepared and the host can send it again, corrected.
func TestManagedRefusedHarnessLeavesTheAttemptPrepared(t *testing.T) {
	root := scanRepo(t, map[string]string{"CLAUDE.md": "rules\n"})
	var atStart atomic.Value
	var starts atomic.Int32
	m := harnessLifecycle(t, root, "claude", &atStart, &starts)

	for name, member := range map[string]string{
		"reserved section name": `{"kind":"named","name":"bad"}`,
		"unknown spec":          `{"kind":"named","name":"nonesuch"}`,
		"inline":                `{"kind":"inline","name":"x"}`,
	} {
		_, err := m.runner.Prepare(t.Context(), preparing("launch-"+name, member))
		var typed *Error
		if !errors.As(err, &typed) || (typed.Code != ErrHarnessRefused && typed.Code != ErrHarnessNotFound) {
			t.Fatalf("%s: err = %v, want a typed harness refusal", name, err)
		}
	}
	if starts.Load() != 0 {
		t.Fatal("a process was started for a refused harness")
	}
	if got := readFile(t, root, "CLAUDE.md"); got != "rules\n" {
		t.Fatalf("a refused harness changed the checkout: %q", got)
	}
	m.runner.mu.Lock()
	job := m.runner.jobs["run"]
	m.runner.mu.Unlock()
	if job != nil {
		enhancementWait(t, job.done)
	}
	if run, _, err := m.runner.run(t.Context(), "run"); err != nil || run.State != "prepared" {
		t.Fatalf("a refused harness consumed the attempt: state=%s err=%v", run.State, err)
	}
	prepared, err := m.runner.Prepare(t.Context(), preparing("corrected", ""))
	if err != nil {
		t.Fatalf("the corrected attempt was not admitted: %v", err)
	}
	activate(t, m, prepared)
	m.finished()
}

// However the run ends, the block is taken out before it is recorded finished;
// a stop is the end that gives no chance to a clean exit path.
func TestManagedStopRestoresTheHarness(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	var m *managedLifecycle
	var atStart atomic.Value
	var starts atomic.Int32
	m = harnessLifecycle(t, root, "codex", &atStart, &starts)
	// A session that is waiting on a human never finishes by itself.
	m.runner.factory = func(ctx context.Context, o codingagent.Options) (ManagedSession, error) {
		s := m.fixtureSession(ctx, o)
		m.session = s
		return s, nil
	}
	prepared, err := m.runner.Prepare(t.Context(), preparing("launch", `{"kind":"named","name":"registered"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, root, "AGENTS.md"); !strings.Contains(got, harnessBeginMarker) {
		t.Fatal("the harness was not written for the live run")
	}
	activate(t, m, prepared)
	m.awaitDecisions(1)
	if err := m.runner.Stop(stopRequest()); err != nil {
		t.Fatal(err)
	}
	m.finished()
	if got := readFile(t, root, "AGENTS.md"); got != "rules\n" {
		t.Fatalf("a stopped run left the harness in the checkout:\n%s", got)
	}
	if status := gitIn(t, root, "status", "--porcelain"); status != "" {
		t.Fatalf("the stopped run's diff carries the harness: %q", status)
	}
}
