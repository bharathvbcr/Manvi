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

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
	"github.com/bharathvbcr/Manvi/manvi/codingagent"
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
	sent           atomic.Value // the turn text StartTurn received
	// claudeLike models the Claude adapter: Respond emits no ResolvedID, and the
	// turn finishes finishAfter later rather than as part of the reply.
	claudeLike  bool
	finishAfter time.Duration
	// requestIDs, when set, are the callbacks StartTurn raises (instead of the
	// single "n:42"), each expiring after expires (default one minute).
	requestIDs []string
	expires    time.Duration
	// withdraw has StartTurn withdraw each request it raised, as Claude does on
	// control_cancel_request; validateErr is what ValidateResponse answers; and
	// completeAfter, when set, finishes the turn that long after StartTurn.
	withdraw      bool
	validateErr   error
	completeAfter time.Duration
	// prelude is emitted after the turn id and before the completion, and
	// leaveOpen withholds that completion so a test can observe the checkpoint.
	prelude   []codingagent.Tool
	leaveOpen bool
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
func (s *managedTestSession) StartTurn(_ context.Context, turn string) error {
	s.sent.Store(turn)
	s.turns.Add(1)
	s.events <- codingagent.Event{TurnID: "turn"}
	if len(s.prelude) > 0 {
		s.events <- codingagent.Event{Tools: s.prelude}
	}
	if s.withRequest {
		ids, expires := s.requestIDs, s.expires
		if len(ids) == 0 {
			ids = []string{"n:42"}
		}
		if expires == 0 {
			expires = time.Minute
		}
		for _, id := range ids {
			payload := `{"command":"git status"}`
			digest := sha256.Sum256([]byte(payload))
			s.events <- codingagent.Event{Request: &codingagent.Request{ID: id, ThreadID: "thread", TurnID: "turn", Kind: "permission", Payload: payload, Digest: hex.EncodeToString(digest[:]), ExpiresAt: time.Now().Add(expires)}}
			if s.withdraw {
				s.events <- codingagent.Event{CancelledID: id}
			}
		}
		if s.completeAfter > 0 {
			time.AfterFunc(s.completeAfter, func() {
				s.events <- codingagent.Event{Text: "Fixture result", Status: "completed"}
			})
		}
	} else if !s.leaveOpen {
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
	return s.validateErr
}
func (s *managedTestSession) Respond(_ context.Context, request codingagent.Request, response codingagent.Response) error {
	s.replies.Add(1)
	if s.claudeLike {
		time.AfterFunc(s.finishAfter, func() {
			s.events <- codingagent.Event{Text: "Fixture result", Status: "completed"}
		})
		return nil
	}
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
				if err := runner.Wake(nil); err != nil {
					t.Fatal(err)
				}
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
			// The one turn carries the launcher's scope for this mode, then the
			// store's brief verbatim, which itself opens with the agent guidance.
			turn, _ := session.sent.Load().(string)
			preface, brief, found := strings.Cut(turn, "\n\n# Task brief v1\n\n")
			if !found || !strings.HasPrefix(brief, "## Agent guidance\n") || !strings.Contains(brief, "gitpulse_insights") || !strings.Contains(brief, "## Title\nInspect fixture\n") {
				t.Fatalf("the managed turn did not deliver the recorded brief with its guidance:\n%s", turn)
			}
			if !strings.Contains(preface, "gitpulse_complete_task") || !strings.Contains(preface, "Do not publish changes") {
				t.Fatalf("an ask-mode turn lost its completion or publishing rule:\n%s", preface)
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

// TestManagedTurnScopesTheAttemptAndKeepsTheBriefVerbatim: only an attempt
// that may edit is told how to finish the task, every attempt is told not to
// publish, only a Claude session (which loads user settings alone) is told the
// repository's own instructions were not loaded, and the recorded brief rides
// along byte for byte at the end.
func TestManagedTurnScopesTheAttemptAndKeepsTheBriefVerbatim(t *testing.T) {
	brief := "# Task brief v1\n\n## Agent guidance\nRead before you write.\n\n## Title\nKeep E42 `$(touch nope)` 🧪\n"
	for _, provider := range []string{"claude", "codex"} {
		for _, mode := range []string{"inspect", "ask", "edit", "auto_review", "preapproved", "bypass"} {
			turn := managedTurn(provider, mode, brief)
			preface, found := strings.CutSuffix(turn, "\n\n"+brief)
			if !found {
				t.Fatalf("%s/%s: the brief is not carried verbatim at the end:\n%s", provider, mode, turn)
			}
			if strings.Contains(preface, "gitpulse_complete_task") == (mode == "inspect") {
				t.Fatalf("%s/%s: completion rule is wrong for the mode:\n%s", provider, mode, preface)
			}
			if mode == "inspect" && !strings.Contains(preface, "do not modify files or change the task's status") {
				t.Fatalf("%s/inspect: not told to leave files and status alone:\n%s", provider, preface)
			}
			if !strings.Contains(preface, "Do not publish changes (push, merge, release)") {
				t.Fatalf("%s/%s: publishing rule missing:\n%s", provider, mode, preface)
			}
			if strings.Contains(preface, "read the repository's AGENTS.md and CLAUDE.md") != (provider == "claude") {
				t.Fatalf("%s/%s: project-instructions notice is wrong for the provider:\n%s", provider, mode, preface)
			}
			if !strings.Contains(preface, "It opens with the agent guidance") {
				t.Fatalf("%s/%s: a brief with guidance was not described as having it:\n%s", provider, mode, preface)
			}
		}
	}
	// A store that predates the guidance section ships separately from this
	// binary; its brief must not be described as carrying guidance it lacks.
	old := "# Task brief v1\n\n## Title\nKeep E42\n"
	turn := managedTurn("codex", "edit", old)
	if strings.Contains(turn, "It opens with the agent guidance") || !strings.Contains(turn, "read the repository's own instructions") || !strings.HasSuffix(turn, "\n\n"+old) {
		t.Fatalf("a brief without guidance was misdescribed:\n%s", turn)
	}
}

// TestManagedPreparationBoundsTheTurnNotJustTheBrief: the adapters refuse a
// turn over 128 KiB in StartTurn, after the attempt is claimed and the process
// spawned. A brief that fits on its own but not with the preface must be
// refused at preparation, before anything is claimed or started.
func TestManagedPreparationBoundsTheTurnNotJustTheBrief(t *testing.T) {
	client := store.New(testsupport.DCStore(t), filepath.Join(t.TempDir(), "profile.sqlite"))
	defer client.Close()
	root := t.TempDir()
	common := filepath.Join(root, ".git")
	enhancementCall(t, client, "repositories.put", fmt.Sprintf(`{"id":"r","request_id":"r","expected_revision":0,"name":"Repo","identity_key":%q}`, "local:"+common))
	put := func(revision, logs int) {
		enhancementCall(t, client, "items.put", fmt.Sprintf(`{"id":"t","request_id":"t%d","expected_revision":%d,"title":"Inspect fixture","logs":%q,"repository_ids":["r"],"primary_repository_id":"r"}`, revision, revision, strings.Repeat("x", logs)))
	}
	markdown := func(revision int) string {
		var brief struct {
			Item struct {
				Markdown string `json:"markdown"`
			} `json:"item"`
		}
		if err := json.Unmarshal(enhancementCall(t, client, "items.brief.get", fmt.Sprintf(`{"id":"t","expected_revision":%d}`, revision)), &brief); err != nil {
			t.Fatal(err)
		}
		return brief.Item.Markdown
	}
	const probe = 100_000
	put(0, probe)
	target := 128*1024 - 64 // fits alone; the preface is far longer than 64 bytes
	put(1, probe+target-len(markdown(1)))
	if got := len(markdown(2)); got != target {
		t.Fatalf("fixture brief is %d bytes, want %d", got, target)
	}
	if len(managedTurn("codex", "ask", markdown(2))) <= 128*1024 {
		t.Fatal("fixture does not exercise the preface pushing the turn over the bound")
	}
	enhancementCall(t, client, "runs.prepare", fmt.Sprintf(`{"id":"run","request_id":"prep","expected_revision":0,"kind":"managed","task_id":"t","source_revision":2,"repository_id":"r","repository_revision":1,"provider":"codex","permission_mode":"ask","cwd":%q,"git_dir":%q,"git_common_dir":%q,"head_oid":null}`, root, common, common))
	var started atomic.Int32
	runner, err := NewManagedRunner(client, func(context.Context, codingagent.Options) (ManagedSession, error) {
		started.Add(1)
		return nil, errors.New("must not start")
	}, func(err error) string { return err.Error() })
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := runner.Close(); err != nil {
			t.Error(err)
		}
	}()
	_, err = runner.Prepare(t.Context(), json.RawMessage(`{"id":"run","request_id":"launch","expected_revision":1}`))
	if err == nil || !strings.Contains(err.Error(), "128 KiB") {
		t.Fatalf("an over-long turn was not refused at preparation: %v", err)
	}
	if started.Load() != 0 {
		t.Fatal("a provider was started for a turn the adapter would refuse")
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
		_, err := codingagent.StartCodex(ctx, options)
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

// The provider gate is what stops one provider's saved attempt being handed to
// another provider's protocol reader. It is derived from the adapters that
// exist, so adding an adapter opens the gate and nothing else does.
func TestManagedRunnerAdmitsEveryProviderWithAnAdapterAndNoOthers(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "grok", "agy", "shell"} {
		t.Run(provider, func(t *testing.T) {
			client := store.New(testsupport.DCStore(t), filepath.Join(t.TempDir(), "profile.sqlite"))
			defer client.Close()
			root := t.TempDir()
			common := filepath.Join(root, ".git")
			enhancementCall(t, client, "repositories.put", fmt.Sprintf(`{"id":"r","request_id":"r","expected_revision":0,"name":"Repo","identity_key":%q}`, "local:"+common))
			enhancementCall(t, client, "items.put", `{"id":"t","request_id":"t","expected_revision":0,"title":"Inspect fixture","repository_ids":["r"],"primary_repository_id":"r"}`)
			prepared := enhancementCallErr(client, "runs.prepare", fmt.Sprintf(`{"id":"run","request_id":"prep","expected_revision":0,"kind":"managed","task_id":"t","source_revision":1,"repository_id":"r","repository_revision":1,"provider":%q,"permission_mode":"ask","cwd":%q,"git_dir":%q,"git_common_dir":%q,"head_oid":null}`, provider, root, common, common))
			if prepared != nil {
				// The store refuses this provider for a managed run before the
				// runner is reached, which is itself a closed gate.
				if codingagent.IsManagedProvider(provider) {
					t.Fatalf("a provider with an adapter could not be prepared: %v", prepared)
				}
				return
			}
			var routed atomic.Value
			session := &managedTestSession{}
			runner, err := NewManagedRunner(client, func(_ context.Context, options codingagent.Options) (ManagedSession, error) {
				routed.Store(options.Provider)
				return session, nil
			}, func(err error) string { return err.Error() })
			if err != nil {
				t.Fatal(err)
			}
			defer runner.Close()
			_, err = runner.Prepare(t.Context(), json.RawMessage(`{"id":"run","request_id":"launch","expected_revision":1}`))
			admitted := err == nil
			if admitted != codingagent.IsManagedProvider(provider) {
				t.Fatalf("provider %q admitted=%v but IsManagedProvider=%v (%v)", provider, admitted, codingagent.IsManagedProvider(provider), err)
			}
			if !admitted {
				if got := routed.Load(); got != nil {
					t.Fatalf("a provider without an adapter still reached the factory as %v", got)
				}
				return
			}
			// The factory must be told which provider to start; without it,
			// every run would be handed to whichever adapter is written first.
			if got, _ := routed.Load().(string); got != provider {
				t.Fatalf("factory was asked for %q on a %q run", got, provider)
			}
		})
	}
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

func TestSavedOutputKeepsAFittingSnapshotAndDropsOneThatDoesNot(t *testing.T) {
	job := &managedJob{}
	job.output.WriteString("hello")
	job.tools = []codingagent.Tool{{ID: "toolu_1", Name: "Bash", Status: "started"}}
	out, truncated := savedOutput(job)
	if truncated || !strings.Contains(out, "hello\n[tools]\n") || !strings.Contains(out, `"id":"toolu_1"`) {
		t.Fatalf("snapshot = %q truncated %v", out, truncated)
	}
	if job.output.String() != "hello" {
		t.Fatal("savedOutput mutated the turn text")
	}
	job.output.Reset()
	job.output.WriteString(strings.Repeat("x", maxManagedOutput-8))
	out, truncated = savedOutput(job)
	if !truncated || strings.Contains(out, "[tools]") {
		t.Fatal("a snapshot that does not fit was written or not marked truncated")
	}
	job = &managedJob{}
	noteTools(job, []codingagent.Tool{{ID: "toolu_1", Name: "Bash", Status: "started", Input: "ls"}})
	noteTools(job, []codingagent.Tool{{ID: "toolu_1", Status: "completed", Output: "ok"}})
	if len(job.tools) != 1 || job.tools[0].Name != "Bash" || job.tools[0].Input != "ls" || job.tools[0].Output != "ok" || job.tools[0].Status != "completed" {
		t.Fatalf("merge = %+v", job.tools)
	}
	for i := 0; i < maxManagedTools+1; i++ {
		noteTools(job, []codingagent.Tool{{ID: fmt.Sprintf("extra-%d", i), Name: "Bash", Status: "started"}})
	}
	if len(job.tools) != maxManagedTools || !job.truncated {
		t.Fatalf("cap = %d truncated %v", len(job.tools), job.truncated)
	}
}

func TestManagedToolSnapshotIsCheckpointedWhileTheTurnRuns(t *testing.T) {
	previous := outputCheckpoint
	outputCheckpoint = 0
	t.Cleanup(func() { outputCheckpoint = previous })
	var session *managedTestSession
	m := newManagedLifecycle(t, func(ctx context.Context, options codingagent.Options) *managedTestSession {
		config := codingagent.Effective{Cwd: options.Cwd, ApprovalPolicy: "on-request", ApprovalsReviewer: "user", Sandbox: codingagent.Sandbox{Type: "readOnly"}, Model: "fixture", ModelProvider: "fixture"}
		config.Thread.ID = "thread"
		config.Thread.Cwd = options.Cwd
		session = &managedTestSession{
			ctx: ctx, config: config, events: make(chan codingagent.Event, 8),
			leaveOpen: true,
			prelude: []codingagent.Tool{{
				ID: "toolu_1", Name: "Bash", Status: "started", Input: `{"command":"ls"}`,
			}},
		}
		return session
	})
	m.start("launch")
	deadline := time.Now().Add(5 * time.Second)
	var output string
	for time.Now().Before(deadline) {
		_, raw, err := m.runner.run(m.t.Context(), "run")
		if err != nil {
			t.Fatal(err)
		}
		var row struct {
			Item struct {
				Output        string `json:"output"`
				ProviderState string `json:"provider_state"`
			} `json:"item"`
		}
		if json.Unmarshal(raw, &row) != nil {
			t.Fatalf("bad run: %s", raw)
		}
		output = row.Item.Output
		if strings.Contains(output, "\n[tools]\n") && strings.Contains(output, `"id":"toolu_1"`) && row.Item.ProviderState == "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(output, "\n[tools]\n") || !strings.Contains(output, `"id":"toolu_1"`) {
		t.Fatalf("tool snapshot was not checkpointed: %s", output)
	}
	session.events <- codingagent.Event{Text: "done\n", Status: "completed"}
	run, raw := m.finished()
	if run.ProviderState != "completed" {
		t.Fatalf("turn did not finish: %s %v", raw, run)
	}
	var row struct {
		Item struct {
			Output string `json:"output"`
		} `json:"item"`
	}
	if json.Unmarshal(raw, &row) != nil || !strings.Contains(row.Item.Output, "done\n") || !strings.Contains(row.Item.Output, `"id":"toolu_1"`) {
		t.Fatalf("final output lost the snapshot: %s", raw)
	}
}
