package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// managedLifecycle is a prepared managed run over a real store and a runner
// whose provider is the given fake.
type managedLifecycle struct {
	t       *testing.T
	client  *store.Client
	runner  *ManagedRunner
	session *managedTestSession
	prepare string
}

func newManagedLifecycle(t *testing.T, build func(context.Context, codingagent.Options) *managedTestSession) *managedLifecycle {
	t.Helper()
	client := store.New(testsupport.DCStore(t), filepath.Join(t.TempDir(), "profile.sqlite"))
	t.Cleanup(func() { client.Close() })
	root := t.TempDir()
	common := filepath.Join(root, ".git")
	enhancementCall(t, client, "repositories.put", fmt.Sprintf(`{"id":"r","request_id":"r","expected_revision":0,"name":"Repo","identity_key":%q}`, "local:"+common))
	enhancementCall(t, client, "items.put", `{"id":"t","request_id":"t","expected_revision":0,"title":"Inspect fixture","repository_ids":["r"],"primary_repository_id":"r"}`)
	prepare := fmt.Sprintf(`{"id":"run","request_id":"prep","expected_revision":0,"kind":"managed","task_id":"t","source_revision":1,"repository_id":"r","repository_revision":1,"provider":"claude","permission_mode":"ask","cwd":%q,"git_dir":%q,"git_common_dir":%q,"head_oid":null}`, root, common, common)
	enhancementCall(t, client, "runs.prepare", prepare)
	m := &managedLifecycle{t: t, client: client, prepare: prepare}
	runner, err := NewManagedRunner(client, func(ctx context.Context, options codingagent.Options) (ManagedSession, error) {
		m.session = build(ctx, options)
		return m.session, nil
	}, func(err error) string { return err.Error() })
	if err != nil {
		t.Fatal(err)
	}
	m.runner = runner
	t.Cleanup(func() {
		if err := runner.Close(); err != nil {
			t.Error(err)
		}
	})
	return m
}

func (m *managedLifecycle) fixtureSession(ctx context.Context, options codingagent.Options) *managedTestSession {
	config := codingagent.Effective{Cwd: options.Cwd, ApprovalPolicy: "on-request", ApprovalsReviewer: "user", Sandbox: codingagent.Sandbox{Type: "readOnly"}, Model: "fixture", ModelProvider: "fixture"}
	config.Thread.ID = "thread"
	config.Thread.Cwd = options.Cwd
	config.Thread.Ephemeral = true
	return &managedTestSession{ctx: ctx, config: config, events: make(chan codingagent.Event, 16), withRequest: true}
}

// start prepares (as the given request identity) and activates the run.
func (m *managedLifecycle) start(requestID string) {
	m.t.Helper()
	prepared, err := m.runner.Prepare(m.t.Context(), json.RawMessage(fmt.Sprintf(`{"id":"run","request_id":%q,"expected_revision":1}`, requestID)))
	if err != nil {
		m.t.Fatal(err)
	}
	if _, err := m.runner.Activate(m.t.Context(), json.RawMessage(fmt.Sprintf(`{"id":"run","owner_id":%q,"session_id":%q,"process_id":123,"process_start":"fixture-native-observation"}`, prepared.Owner, prepared.Session))); err != nil {
		m.t.Fatal(err)
	}
}

type decisionRow struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Digest   string `json:"payload_digest"`
	Request  string `json:"protocol_request_id"`
	State    string `json:"state"`
	Reason   string `json:"reason"`
}

func (m *managedLifecycle) decisions() []decisionRow {
	m.t.Helper()
	var page struct {
		Items []decisionRow `json:"items"`
	}
	if err := json.Unmarshal(enhancementCall(m.t, m.client, "decisions.list", `{"run_id":"run"}`), &page); err != nil {
		m.t.Fatal(err)
	}
	return page.Items
}

// awaitDecisions waits until n callbacks are durable and returns them.
func (m *managedLifecycle) awaitDecisions(n int) []decisionRow {
	m.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rows := m.decisions(); len(rows) >= n {
			return rows
		}
		time.Sleep(10 * time.Millisecond)
	}
	m.t.Fatalf("only %d of %d callbacks became durable", len(m.decisions()), n)
	return nil
}

func (m *managedLifecycle) allow(row decisionRow) {
	m.t.Helper()
	enhancementCall(m.t, m.client, "decisions.decide", fmt.Sprintf(`{"id":%q,"request_id":"human-%s","expected_revision":%d,"payload_digest":%q,"decision":"allow_once"}`, row.ID, row.ID[:8], row.Revision, row.Digest))
}

func (m *managedLifecycle) finished() (managedRun, json.RawMessage) {
	m.t.Helper()
	m.runner.mu.Lock()
	job := m.runner.jobs["run"]
	m.runner.mu.Unlock()
	enhancementWait(m.t, job.done)
	run, raw, err := m.runner.run(m.t.Context(), "run")
	if err != nil {
		m.t.Fatal(err)
	}
	return run, raw
}

// A Claude approval is answered in seconds and the turn keeps working past the
// callback's expiry (the store's own deadline is whole seconds, so the fixture
// uses several). Claude's Respond emits no ResolvedID, so nothing but the
// delivery itself says the callback is settled; the run used to be failed at
// expiry anyway and the delivered approval recorded as cancelled.
func TestManagedClaudeAnsweredApprovalSurvivesItsExpiry(t *testing.T) {
	var m *managedLifecycle
	m = newManagedLifecycle(t, func(ctx context.Context, o codingagent.Options) *managedTestSession {
		s := m.fixtureSession(ctx, o)
		s.claudeLike, s.expires, s.finishAfter = true, 2*time.Second, 2500*time.Millisecond
		return s
	})
	m.start("launch")
	rows := m.awaitDecisions(1)
	m.allow(rows[0])
	run, raw := m.finished()
	if run.State != "exited" || run.ProviderState != "completed" || m.session.replies.Load() != 1 {
		t.Fatalf("an answered approval failed the run at its expiry: state=%s provider_state=%s (%.200s)", run.State, run.ProviderState, raw[strings.Index(string(raw), `"reason"`):])
	}
	got := m.decisions()
	if len(got) != 1 || got[0].State != "resolved" || !strings.Contains(got[0].Reason, "delivered") {
		t.Fatalf("a delivered approval was recorded as %+v, want resolved and delivered", got)
	}
}

// Expiry is for callbacks still waiting on a human. With one answered and one
// not, only the unanswered one may end the run, and the answered one stays
// recorded as delivered.
func TestManagedExpiryAppliesOnlyToUnansweredCallbacks(t *testing.T) {
	var m *managedLifecycle
	m = newManagedLifecycle(t, func(ctx context.Context, o codingagent.Options) *managedTestSession {
		s := m.fixtureSession(ctx, o)
		s.claudeLike, s.expires, s.finishAfter = true, 2*time.Second, time.Hour
		s.requestIDs = []string{"c:1", "c:2"}
		return s
	})
	m.start("launch")
	rows := m.awaitDecisions(2)
	for _, r := range rows {
		if r.Request == "c:1" {
			m.allow(r)
		}
	}
	run, raw := m.finished()
	if run.ProviderState != "failed" || !strings.Contains(string(raw), "expired without confirmation") {
		t.Fatalf("the unanswered callback did not end the run: provider_state=%s (%.300s)", run.ProviderState, raw[strings.Index(string(raw), `"reason"`):])
	}
	for _, r := range m.decisions() {
		switch r.Request {
		case "c:1":
			if r.State != "resolved" || !strings.Contains(r.Reason, "delivered") {
				t.Fatalf("answered callback recorded as %+v", r)
			}
		case "c:2":
			if r.State != "cancelled" {
				t.Fatalf("unanswered callback recorded as %+v", r)
			}
		}
	}
}

// A Prepare refused before anything was claimed must not wedge the run id: the
// store still says prepared, so a corrected retry has to be accepted.
func TestManagedPrepareFailureBeforeClaimDoesNotWedgeTheRun(t *testing.T) {
	var m *managedLifecycle
	m = newManagedLifecycle(t, func(ctx context.Context, o codingagent.Options) *managedTestSession {
		s := m.fixtureSession(ctx, o)
		s.withRequest = false
		return s
	})
	if _, err := m.runner.Prepare(t.Context(), json.RawMessage(`{"id":"run","request_id":"stale","expected_revision":7}`)); err == nil {
		t.Fatal("a stale revision was accepted")
	}
	prepared, err := m.runner.Prepare(t.Context(), json.RawMessage(`{"id":"run","request_id":"corrected","expected_revision":1}`))
	if err != nil || prepared.Phase != "awaiting_activation" {
		t.Fatalf("the corrected retry was refused: %+v %v", prepared, err)
	}
}

// A callback the provider withdrew is no longer awaiting anyone: its row ends
// cancelled, it stops counting toward expiry, and the run carries on to the
// provider's own status.
func TestManagedWithdrawnCallbackIsCancelledAndNoLongerExpires(t *testing.T) {
	var m *managedLifecycle
	m = newManagedLifecycle(t, func(ctx context.Context, o codingagent.Options) *managedTestSession {
		s := m.fixtureSession(ctx, o)
		s.expires, s.withdraw, s.completeAfter = 2*time.Second, true, 3500*time.Millisecond
		return s
	})
	m.start("launch")
	run, raw := m.finished()
	if run.ProviderState != "completed" {
		t.Fatalf("a withdrawn callback still ended the run: provider_state=%s (%.300s)", run.ProviderState, raw[strings.Index(string(raw), `"reason"`):])
	}
	rows := m.decisions()
	if len(rows) != 1 || rows[0].State != "cancelled" || !strings.Contains(rows[0].Reason, "withdrew") {
		t.Fatalf("withdrawn callback recorded as %+v", rows)
	}
}

// The withdrawal can land between the human's answer and its delivery. That is
// a callback gone, not a failed run.
func TestManagedAnswerToAWithdrawnCallbackDoesNotFailTheRun(t *testing.T) {
	var m *managedLifecycle
	m = newManagedLifecycle(t, func(ctx context.Context, o codingagent.Options) *managedTestSession {
		s := m.fixtureSession(ctx, o)
		s.expires, s.completeAfter = time.Minute, 1500*time.Millisecond
		s.validateErr = codingagent.ErrCallbackWithdrawn
		return s
	})
	m.start("launch")
	m.allow(m.awaitDecisions(1)[0])
	run, raw := m.finished()
	if run.ProviderState != "completed" || m.session.replies.Load() != 0 {
		t.Fatalf("an answer to a withdrawn callback failed the run or was delivered: provider_state=%s replies=%d (%.300s)", run.ProviderState, m.session.replies.Load(), raw[strings.Index(string(raw), `"reason"`):])
	}
	if rows := m.decisions(); len(rows) != 1 || rows[0].State != "cancelled" {
		t.Fatalf("withdrawn callback recorded as %+v", rows)
	}
}
