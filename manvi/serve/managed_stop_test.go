package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// ctxOnlySession is a ManagedSession that is not process-backed: it owns no
// child whose death would end a read, and it obeys only the context each call
// hands it. It is what proves a job's cancellation reaches the session through
// drive's context rather than through some connection the session happens to
// own.
type ctxOnlySession struct {
	config  codingagent.Effective
	closed  atomic.Bool
	nextCtx atomic.Pointer[context.Context]
}

func (s *ctxOnlySession) PID() int                         { return 123 }
func (s *ctxOnlySession) Initialize(context.Context) error { return nil }
func (s *ctxOnlySession) Effective() codingagent.Effective { return s.config }
func (s *ctxOnlySession) StartTurn(context.Context, string) error {
	return nil
}
func (s *ctxOnlySession) Next(ctx context.Context) (codingagent.Event, error) {
	<-ctx.Done()
	return codingagent.Event{}, ctx.Err()
}
func (s *ctxOnlySession) ValidateResponse(codingagent.Request, codingagent.Response) error {
	return nil
}
func (s *ctxOnlySession) Respond(context.Context, codingagent.Request, codingagent.Response) error {
	return nil
}
func (s *ctxOnlySession) Close() (codingagent.Exit, error) {
	s.closed.Store(true)
	return codingagent.Exit{Reaped: true}, nil
}

// stopFixture is a store holding one prepared managed run named "run".
func stopFixture(t *testing.T, factory ManagedFactory) *ManagedRunner {
	t.Helper()
	client := store.New(testsupport.DCStore(t), filepath.Join(t.TempDir(), "profile.sqlite"))
	t.Cleanup(func() { client.Close() })
	root := t.TempDir()
	common := filepath.Join(root, ".git")
	enhancementCall(t, client, "repositories.put", fmt.Sprintf(`{"id":"r","request_id":"r","expected_revision":0,"name":"Repo","identity_key":%q}`, "local:"+common))
	enhancementCall(t, client, "items.put", `{"id":"t","request_id":"t","expected_revision":0,"title":"Inspect fixture","repository_ids":["r"],"primary_repository_id":"r"}`)
	enhancementCall(t, client, "runs.prepare", fmt.Sprintf(`{"id":"run","request_id":"prep","expected_revision":0,"kind":"managed","task_id":"t","source_revision":1,"repository_id":"r","repository_revision":1,"provider":"codex","permission_mode":"ask","cwd":%q,"git_dir":%q,"git_common_dir":%q,"head_oid":null}`, root, common, common))
	runner, err := NewManagedRunner(client, factory, func(err error) string { return err.Error() })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runner.Close(); err != nil {
			t.Error(err)
		}
	})
	return runner
}

func stopJob(t *testing.T, r *ManagedRunner) *managedJob {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	job := r.jobs["run"]
	if job == nil {
		t.Fatal("no managed job named run")
	}
	return job
}

func stopRequest() json.RawMessage { return json.RawMessage(`{"id":"run"}`) }

func ctxOnlyFactory(sessions *[]*ctxOnlySession, mu *sync.Mutex) ManagedFactory {
	return func(_ context.Context, options codingagent.Options) (ManagedSession, error) {
		config := codingagent.Effective{Cwd: options.Cwd, ApprovalPolicy: "on-request", ApprovalsReviewer: "user", Sandbox: codingagent.Sandbox{Type: "readOnly"}, Model: "fixture", ModelProvider: "fixture"}
		config.Thread.ID = "thread"
		config.Thread.Cwd = options.Cwd
		config.Thread.Ephemeral = true
		s := &ctxOnlySession{config: config}
		mu.Lock()
		*sessions = append(*sessions, s)
		mu.Unlock()
		return s, nil
	}
}

func activateInput(p ManagedPrepared) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"id":"run","owner_id":%q,"session_id":%q,"process_id":123,"process_start":"fixture-birth"}`, p.Owner, p.Session))
}

func requireStopped(t *testing.T, r *ManagedRunner, job *managedJob, wantProviderState string) {
	t.Helper()
	select {
	case <-job.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not end the managed job: job.done is still open after 5s")
	}
	run, raw, err := r.run(t.Context(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if run.ProviderState != wantProviderState {
		t.Fatalf("run recorded provider_state %q, want %q: %s", run.ProviderState, wantProviderState, raw)
	}
}

// A job stopped while its factory is still starting the provider ends, and the
// run is recorded interrupted rather than failed: nobody judged the provider.
func TestManagedStopDuringPreparingEndsTheJob(t *testing.T) {
	entered := make(chan struct{})
	runner := stopFixture(t, func(ctx context.Context, _ codingagent.Options) (ManagedSession, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	prepErr := make(chan error, 1)
	go func() {
		_, err := runner.Prepare(t.Context(), json.RawMessage(`{"id":"run","request_id":"launch","expected_revision":1}`))
		prepErr <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the factory was never reached")
	}
	job := stopJob(t, runner)
	if err := runner.Stop(stopRequest()); err != nil {
		t.Fatal(err)
	}
	if err := <-prepErr; err == nil {
		t.Fatal("a stopped preparation reported success")
	}
	requireStopped(t, runner, job, "interrupted")
}

func TestManagedStopWhileAwaitingActivationEndsTheJob(t *testing.T) {
	var sessions []*ctxOnlySession
	var mu sync.Mutex
	runner := stopFixture(t, ctxOnlyFactory(&sessions, &mu))
	prepared, err := runner.Prepare(t.Context(), json.RawMessage(`{"id":"run","request_id":"launch","expected_revision":1}`))
	if err != nil || prepared.Phase != "awaiting_activation" {
		t.Fatalf("prepare: %+v %v", prepared, err)
	}
	job := stopJob(t, runner)
	if err := runner.Stop(stopRequest()); err != nil {
		t.Fatal(err)
	}
	requireStopped(t, runner, job, "interrupted")
	mu.Lock()
	defer mu.Unlock()
	if !sessions[0].closed.Load() {
		t.Fatal("the stopped provider was never closed")
	}
}

// The session here has no process: it ends a read only when the context it was
// handed ends. A drive that built its own Background context ignored Stop.
func TestManagedStopWhileRunningEndsANonProcessSession(t *testing.T) {
	var sessions []*ctxOnlySession
	var mu sync.Mutex
	runner := stopFixture(t, ctxOnlyFactory(&sessions, &mu))
	prepared, err := runner.Prepare(t.Context(), json.RawMessage(`{"id":"run","request_id":"launch","expected_revision":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Activate(t.Context(), activateInput(prepared)); err != nil {
		t.Fatal(err)
	}
	job := stopJob(t, runner)
	time.Sleep(100 * time.Millisecond) // let drive reach Next
	if err := runner.Stop(stopRequest()); err != nil {
		t.Fatal(err)
	}
	requireStopped(t, runner, job, "interrupted")
	if !sessions[0].closed.Load() {
		t.Fatal("the stopped provider was never closed")
	}
}

// Stop is idempotent: a second stop, and a stop after the job ended, are
// answered and change nothing.
func TestManagedStopTwiceAndAfterDone(t *testing.T) {
	var sessions []*ctxOnlySession
	var mu sync.Mutex
	runner := stopFixture(t, ctxOnlyFactory(&sessions, &mu))
	prepared, err := runner.Prepare(t.Context(), json.RawMessage(`{"id":"run","request_id":"launch","expected_revision":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Activate(t.Context(), activateInput(prepared)); err != nil {
		t.Fatal(err)
	}
	job := stopJob(t, runner)
	for range 2 {
		if err := runner.Stop(stopRequest()); err != nil {
			t.Fatal(err)
		}
	}
	requireStopped(t, runner, job, "interrupted")
	if err := runner.Stop(stopRequest()); err != nil {
		t.Fatalf("stop after done: %v", err)
	}
	if err := runner.Stop(json.RawMessage(`{"id":"nobody"}`)); err == nil {
		t.Fatal("stopping a run this host does not own was accepted")
	}
}

// Stop races activation: whichever wins, the job ends, nothing deadlocks and
// the provider is closed exactly once the job is done.
func TestManagedStopRacingActivateAlwaysEndsTheJob(t *testing.T) {
	for i := range 20 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			var sessions []*ctxOnlySession
			var mu sync.Mutex
			runner := stopFixture(t, ctxOnlyFactory(&sessions, &mu))
			prepared, err := runner.Prepare(t.Context(), json.RawMessage(`{"id":"run","request_id":"launch","expected_revision":1}`))
			if err != nil {
				t.Fatal(err)
			}
			job := stopJob(t, runner)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); _, _ = runner.Activate(t.Context(), activateInput(prepared)) }()
			go func() { defer wg.Done(); _ = runner.Stop(stopRequest()) }()
			wg.Wait()
			select {
			case <-job.done:
			case <-time.After(5 * time.Second):
				t.Fatal("job.done never closed after Stop raced Activate")
			}
			mu.Lock()
			defer mu.Unlock()
			if !sessions[0].closed.Load() {
				t.Fatal("provider left open")
			}
		})
	}
}
