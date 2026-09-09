package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
	"github.com/bharathvbcr/Manvi/manvi/dc/store"
	"github.com/bharathvbcr/Manvi/manvi/internal/testsupport"
)

type managedTestSession struct {
	ctx            context.Context
	config         codingagent.Effective
	events         chan codingagent.Event
	turns, replies atomic.Int32
	withRequest    bool
	initialized    atomic.Bool
	initialize     func() error
	unreaped       bool
	closed         atomic.Bool
}

func (s *managedTestSession) Initialize(_ context.Context) error {
	s.initialized.Store(true)
	if s.initialize != nil {
		return s.initialize()
	}
	return nil
}

func (s *managedTestSession) PID() int                         { return 123 }
func (s *managedTestSession) Effective() codingagent.Effective { return s.config }
func (s *managedTestSession) Close() (codingagent.Exit, error) {
	s.closed.Store(true)
	return codingagent.Exit{Reaped: !s.unreaped, Code: 0}, nil
}
func (s *managedTestSession) StartTurn(_ context.Context, _ string) error {
	s.turns.Add(1)
	s.events <- codingagent.Event{TurnID: "turn"}
	if s.withRequest {
		payload := `{"command":"git status"}`
		digest := sha256.Sum256([]byte(payload))
		s.events <- codingagent.Event{Request: &codingagent.Request{ID: "n:42", ThreadID: "thread", TurnID: "turn", Kind: "permission", Payload: payload, Digest: hex.EncodeToString(digest[:]), ExpiresAt: time.Now().Add(time.Minute)}}
	} else {
		s.events <- codingagent.Event{Text: "Inspected the fixture", Status: "completed"}
	}
	return nil
}
func (s *managedTestSession) Next(ctx context.Context) (codingagent.Event, error) {
	select {
	case e := <-s.events:
		return e, nil
	case <-ctx.Done():
		return codingagent.Event{}, ctx.Err()
	case <-s.ctx.Done():
		return codingagent.Event{}, s.ctx.Err()
	}
}
func (s *managedTestSession) ValidateResponse(_ codingagent.Request, _ codingagent.Response) error {
	return nil
}
func (s *managedTestSession) Respond(_ context.Context, request codingagent.Request, response codingagent.Response) error {
	s.replies.Add(1)
	s.events <- codingagent.Event{ResolvedID: request.ID}
	s.events <- codingagent.Event{Text: "Fixture result", Status: "completed"}
	return nil
}

func TestManagedInitializationFailurePreservesBirthAndOnlyReapedAttemptsReleaseCapacity(t *testing.T) {
	for _, unreaped := range []bool{false, true} {
		t.Run(fmt.Sprint(unreaped), func(t *testing.T) {
			client := store.New(testsupport.DCStore(t), filepath.Join(t.TempDir(), "profile.sqlite"))
			defer client.Close()
			root := t.TempDir()
			common := filepath.Join(root, ".git")
			enhancementCall(t, client, "repositories.put", fmt.Sprintf(`{"id":"r","request_id":"r","expected_revision":0,"name":"Repo","identity_key":%q}`, "local:"+common))
			enhancementCall(t, client, "items.put", `{"id":"t","request_id":"t","expected_revision":0,"title":"Inspect fixture","repository_ids":["r"],"primary_repository_id":"r"}`)
			prepare := fmt.Sprintf(`{"id":"run","request_id":"prep","expected_revision":0,"kind":"managed","task_id":"t","source_revision":1,"repository_id":"r","repository_revision":1,"provider":"codex","permission_mode":"ask","cwd":%q,"git_dir":%q,"git_common_dir":%q,"head_oid":null}`, root, common, common)
			enhancementCall(t, client, "runs.prepare", prepare)
			var sawBirth atomic.Bool
			session := &managedTestSession{unreaped: unreaped, initialize: func() error {
				raw, err := client.Workbench(context.Background(), "runs.get", json.RawMessage(`{"id":"run"}`))
				if err != nil {
					return err
				}
				var row struct {
					Item struct {
						State string `json:"state"`
						PID   int    `json:"process_id"`
						Birth string `json:"process_start"`
					} `json:"item"`
				}
				if err := json.Unmarshal(raw, &row); err != nil {
					return err
				}
				sawBirth.Store(row.Item.State == "running" && row.Item.PID == 123 && row.Item.Birth == "fixture-birth")
				return errors.New("provider handshake rejected")
			}}
			runner, err := NewManagedRunner(client, func(context.Context, codingagent.Options) (ManagedSession, error) { return session, nil }, func(err error) string { return err.Error() })
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := runner.Close(); err != nil {
					t.Error(err)
				}
			}()
			prepared, err := runner.Prepare(t.Context(), json.RawMessage(`{"id":"run","request_id":"launch","expected_revision":1}`))
			if err != nil {
				t.Fatal(err)
			}
			if prepared.ProtocolVersion != 2 || prepared.Phase != "awaiting_activation" || session.initialized.Load() {
				t.Fatalf("unexpected preparation: %+v", prepared)
			}
			_, err = runner.Activate(t.Context(), json.RawMessage(fmt.Sprintf(`{"id":"run","owner_id":%q,"session_id":%q,"process_id":123,"process_start":"fixture-birth"}`, prepared.Owner, prepared.Session)))
			if err != nil {
				t.Fatal(err)
			}
			runner.mu.Lock()
			job := runner.jobs["run"]
			runner.mu.Unlock()
			enhancementWait(t, job.done)
			run, raw, err := runner.run(t.Context(), "run")
			if err != nil {
				t.Fatal(err)
			}
			want := "exited"
			if unreaped {
				want = "unresolved"
			}
			if run.State != want || run.ProviderState != "failed" || !sawBirth.Load() || !session.closed.Load() || session.turns.Load() != 0 {
				t.Fatalf("unsafe initialization outcome: %s", raw)
			}
			var row struct {
				Item struct {
					Reason string  `json:"reason"`
					Thread *string `json:"provider_thread_id"`
				} `json:"item"`
			}
			if json.Unmarshal(raw, &row) != nil || row.Item.Thread != nil || row.Item.Reason != "provider handshake rejected" {
				t.Fatalf("failure evidence lost or fabricated: %s", raw)
			}
			next := strings.ReplaceAll(strings.ReplaceAll(prepare, `"run"`, `"retry"`), `"prep"`, `"retry-prep"`)
			_, err = client.Workbench(t.Context(), "runs.prepare", json.RawMessage(next))
			if (err != nil) != unreaped {
				t.Fatalf("repository reservation disagrees with reaping evidence: %v", err)
			}
			var task struct {
				Item struct {
					Revision int    `json:"revision"`
					Status   string `json:"status"`
				} `json:"item"`
			}
			if json.Unmarshal(enhancementCall(t, client, "items.get", `{"id":"t"}`), &task) != nil || task.Item.Revision != 1 || task.Item.Status != "inbox" {
				t.Fatal("startup failure changed the task")
			}
		})
	}
}

func TestManagedHostPersistsCallbacksAndRequiresNativeActivationBeforeOneTurn(t *testing.T) {
	for _, withRequest := range []bool{false, true} {
		t.Run(fmt.Sprint(withRequest), func(t *testing.T) {
			client := store.New(testsupport.DCStore(t), filepath.Join(t.TempDir(), "profile.sqlite"))
			defer client.Close()
			root := t.TempDir()
			common := filepath.Join(root, ".git")
			enhancementCall(t, client, "repositories.put", fmt.Sprintf(`{"id":"r","request_id":"r","expected_revision":0,"name":"Repo","identity_key":%q}`, "local:"+common))
			enhancementCall(t, client, "items.put", `{"id":"t","request_id":"t","expected_revision":0,"title":"Inspect fixture","repository_ids":["r"],"primary_repository_id":"r"}`)
			enhancementCall(t, client, "runs.prepare", fmt.Sprintf(`{"id":"run","request_id":"prep","expected_revision":0,"kind":"managed","task_id":"t","source_revision":1,"repository_id":"r","repository_revision":1,"provider":"codex","permission_mode":"ask","cwd":%q,"git_dir":%q,"git_common_dir":%q,"head_oid":null}`, root, common, common))
			var session *managedTestSession
			runner, err := NewManagedRunner(client, func(ctx context.Context, options codingagent.Options) (ManagedSession, error) {
				config := codingagent.Effective{Cwd: options.Cwd, ApprovalPolicy: "on-request", ApprovalsReviewer: "user", Sandbox: codingagent.Sandbox{Type: "readOnly"}, Model: "fixture", ModelProvider: "fixture"}
				config.Thread.ID = "thread"
				config.Thread.Cwd = options.Cwd
				config.Thread.Ephemeral = true
				session = &managedTestSession{ctx: ctx, config: config, events: make(chan codingagent.Event, 8), withRequest: withRequest}
				return session, nil
			}, func(err error) string { return err.Error() })
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := runner.Close(); err != nil {
					t.Error(err)
				}
			}()
			request := json.RawMessage(`{"id":"run","request_id":"launch","expected_revision":1}`)
			prepared, err := runner.Prepare(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			again, err := runner.Prepare(t.Context(), request)
			if err != nil || again != prepared || session.turns.Load() != 0 {
				t.Fatalf("preparation started or repeated a turn: %+v %v", again, err)
			}
			if session.initialized.Load() {
				t.Fatal("provider initialized before native process identity was persisted")
			}
			activation := func(pid int) json.RawMessage {
				return json.RawMessage(fmt.Sprintf(`{"id":"run","owner_id":%q,"session_id":%q,"process_id":%d,"process_start":"fixture-native-observation"}`, prepared.Owner, prepared.Session, pid))
			}
			if _, err := runner.Activate(t.Context(), activation(456)); err == nil {
				t.Fatal("foreign process activated")
			}
			first, err := runner.Activate(t.Context(), activation(123))
			if err != nil {
				t.Fatal(err)
			}
			second, err := runner.Activate(t.Context(), activation(123))
			if err != nil || string(second) != string(first) {
				t.Fatal("activation replay lost its original receipt", err)
			}
			if withRequest {
				deadline := time.Now().Add(5 * time.Second)
				var item struct {
					ID       string `json:"id"`
					Revision int64  `json:"revision"`
					Digest   string `json:"payload_digest"`
				}
				for item.ID == "" && time.Now().Before(deadline) {
					var page struct {
						Items []json.RawMessage `json:"items"`
					}
					raw := enhancementCall(t, client, "decisions.list", `{"run_id":"run"}`)
					if json.Unmarshal(raw, &page) != nil {
						t.Fatal("bad callback page")
					}
					if len(page.Items) > 0 {
						if err := json.Unmarshal(page.Items[0], &item); err != nil {
							t.Fatal(err)
						}
					} else {
						time.Sleep(10 * time.Millisecond)
					}
				}
				if item.ID == "" {
					t.Fatal("live callback not persisted")
				}
				enhancementCall(t, client, "decisions.decide", fmt.Sprintf(`{"id":%q,"request_id":"human","expected_revision":%d,"payload_digest":%q,"decision":"allow_once"}`, item.ID, item.Revision, item.Digest))
			}
			runner.mu.Lock()
			job := runner.jobs["run"]
			runner.mu.Unlock()
			enhancementWait(t, job.done)
			run, _, err := runner.run(t.Context(), "run")
			if err != nil {
				t.Fatal(err)
			}
			if run.State != "exited" || run.ProviderState != "completed" || session.turns.Load() != 1 {
				t.Fatalf("unexpected run %+v, turns %d, error %v", run, session.turns.Load(), job.err)
			}
			if !session.initialized.Load() {
				t.Fatal("managed turn skipped provider initialization")
			}
			if withRequest && session.replies.Load() != 1 {
				t.Fatal("human response was lost or duplicated")
			}
			var task struct {
				Item struct {
					Revision int    `json:"revision"`
					Status   string `json:"status"`
				} `json:"item"`
			}
			if err := json.Unmarshal(enhancementCall(t, client, "items.get", `{"id":"t"}`), &task); err != nil {
				t.Fatal(err)
			}
			if task.Item.Revision != 1 || task.Item.Status != "inbox" {
				t.Fatal("managed completion accepted the task")
			}
		})
	}
}

func TestManagedMissingExecutableEndsTheAttemptAndReleasesItsRepository(t *testing.T) {
	client := store.New(testsupport.DCStore(t), filepath.Join(t.TempDir(), "profile.sqlite"))
	defer client.Close()
	root := t.TempDir()
	common := filepath.Join(root, ".git")
	enhancementCall(t, client, "repositories.put", fmt.Sprintf(`{"id":"r","request_id":"r","expected_revision":0,"name":"Repo","identity_key":%q}`, "local:"+common))
	enhancementCall(t, client, "items.put", `{"id":"t","request_id":"t","expected_revision":0,"title":"Inspect fixture","repository_ids":["r"],"primary_repository_id":"r"}`)
	prepare := func(id string) {
		enhancementCall(t, client, "runs.prepare", fmt.Sprintf(`{"id":%q,"request_id":%q,"expected_revision":0,"kind":"managed","task_id":"t","source_revision":1,"repository_id":"r","repository_revision":1,"provider":"codex","permission_mode":"ask","cwd":%q,"git_dir":%q,"git_common_dir":%q,"head_oid":null}`, id, "prep-"+id, root, common, common))
	}
	prepare("run")
	runner, err := NewManagedRunner(client, func(ctx context.Context, options codingagent.Options) (ManagedSession, error) {
		options.Program = filepath.Join(root, "missing-codex")
		_, err := codingagent.OpenCodex(ctx, options)
		return nil, err
	}, func(err error) string { return err.Error() })
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	if _, err := runner.Prepare(t.Context(), json.RawMessage(`{"id":"run","request_id":"launch","expected_revision":1}`)); err == nil {
		t.Fatal("missing executable started")
	}
	run, _, err := runner.run(t.Context(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "failed" {
		t.Fatalf("known unstarted provider retained repository capacity: %s", run.State)
	}
	prepare("retry")
}

func TestManagedUncertainFactoryFailureRetainsItsClaimWithoutCallingANilSession(t *testing.T) {
	client := store.New(testsupport.DCStore(t), filepath.Join(t.TempDir(), "profile.sqlite"))
	defer client.Close()
	root := t.TempDir()
	common := filepath.Join(root, ".git")
	enhancementCall(t, client, "repositories.put", fmt.Sprintf(`{"id":"r","request_id":"r","expected_revision":0,"name":"Repo","identity_key":%q}`, "local:"+common))
	enhancementCall(t, client, "items.put", `{"id":"t","request_id":"t","expected_revision":0,"title":"Inspect fixture","repository_ids":["r"],"primary_repository_id":"r"}`)
	enhancementCall(t, client, "runs.prepare", fmt.Sprintf(`{"id":"run","request_id":"prep","expected_revision":0,"kind":"managed","task_id":"t","source_revision":1,"repository_id":"r","repository_revision":1,"provider":"codex","permission_mode":"ask","cwd":%q,"git_dir":%q,"git_common_dir":%q,"head_oid":null}`, root, common, common))
	runner, err := NewManagedRunner(client, func(_ context.Context, _ codingagent.Options) (ManagedSession, error) {
		return (*codingagent.Codex)(nil), errors.New("unknown startup outcome")
	}, func(err error) string { return err.Error() })
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	if _, err := runner.Prepare(t.Context(), json.RawMessage(`{"id":"run","request_id":"launch","expected_revision":1}`)); err == nil {
		t.Fatal("uncertain start accepted")
	}
	run, _, err := runner.run(t.Context(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "unresolved" {
		t.Fatalf("uncertain start released its claim: %s", run.State)
	}
}
