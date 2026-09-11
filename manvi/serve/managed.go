package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// ManagedSession is one provider connection, never an external terminal.
type ManagedSession interface {
	PID() int
	Initialize(context.Context) error
	Effective() codingagent.Effective
	StartTurn(context.Context, string) error
	Next(context.Context) (codingagent.Event, error)
	ValidateResponse(codingagent.Request, codingagent.Response) error
	Respond(context.Context, codingagent.Request, codingagent.Response) error
	Close() (codingagent.Exit, error)
}

// ManagedFactory returns a spawned, uninitialized session on success. On error
// it owns startup cleanup; the runner never dereferences an error result or
// interprets an ordinary error as proof that the provider did not start.
type ManagedFactory func(context.Context, codingagent.Options) (ManagedSession, error)
type ManagedRunner struct {
	client      *store.Client
	factory     ManagedFactory
	failureText func(error) string
	mu          sync.Mutex
	jobs        map[string]*managedJob
	closed      bool
}
type managedRun struct {
	ID             string `json:"id"`
	Revision       int64  `json:"revision"`
	Kind           string `json:"kind"`
	State          string `json:"state"`
	Provider       string `json:"provider"`
	Mode           string `json:"permission_mode"`
	Bypass         bool   `json:"bypass_acknowledged"`
	Cwd            string `json:"cwd"`
	Owner          string `json:"owner_id"`
	Session        string `json:"session_id"`
	SourceRevision int64  `json:"source_revision"`
	Brief          struct {
		Markdown string `json:"markdown"`
	} `json:"brief"`
	Thread        string  `json:"provider_thread_id"`
	Turn          *string `json:"provider_turn_id"`
	ProviderState string  `json:"provider_state"`
	Configuration string  `json:"effective_configuration"`
}
type managedJob struct {
	mu                 sync.Mutex
	id, owner, session string
	preparation        generationRequest
	cancel             context.CancelFunc
	ready, done        chan struct{}
	provider           ManagedSession
	configuration      string
	phase              string
	err                error
	activated          bool
	activation         json.RawMessage
	callbacks          map[string]*managedCallback
	output             strings.Builder
	truncated          bool
}
type managedCallback struct {
	request  codingagent.Request
	captured store.DecisionRequest
}
type ManagedPrepared struct {
	ProtocolVersion int    `json:"protocol_version"`
	ID              string `json:"id"`
	Owner           string `json:"owner_id"`
	Session         string `json:"session_id"`
	PID             int    `json:"process_id"`
	Phase           string `json:"phase"`
}

func NewManagedRunner(client *store.Client, factory ManagedFactory, failureText func(error) string) (*ManagedRunner, error) {
	if client == nil || factory == nil || failureText == nil {
		return nil, errors.New("managed runner requires a store, provider factory and error scrubber")
	}
	return &ManagedRunner{client: client, factory: factory, failureText: failureText, jobs: make(map[string]*managedJob)}, nil
}
func (r *ManagedRunner) run(ctx context.Context, id string) (managedRun, json.RawMessage, error) {
	input, err := json.Marshal(struct {
		ID string `json:"id"`
	}{id})
	if err != nil {
		return managedRun{}, nil, err
	}
	raw, err := r.client.Workbench(ctx, "runs.get", input)
	if err != nil {
		return managedRun{}, nil, err
	}
	var result struct {
		Item managedRun `json:"item"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Item.ID != id || result.Item.Revision < 1 {
		return managedRun{}, nil, errors.New("invalid managed run record")
	}
	return result.Item, raw, nil
}

// Prepare claims and opens the provider without sending the task brief. The
// native host must inspect the returned PID's OS creation identity, then call
// Activate. Merely reading a run or its status never starts a model turn.
func (r *ManagedRunner) Prepare(ctx context.Context, raw json.RawMessage) (ManagedPrepared, error) {
	if _, err := enhancementObject(raw, 4096, "id", "request_id", "expected_revision"); err != nil {
		return ManagedPrepared{}, err
	}
	var request generationRequest
	if json.Unmarshal(raw, &request) != nil || request.ID == "" || request.RequestID == "" || request.ExpectedRevision < 1 {
		return ManagedPrepared{}, errors.New("managed preparation requires a saved attempt and request identity")
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ManagedPrepared{}, errors.New("managed runner is closed")
	}
	job := r.jobs[request.ID]
	if job == nil {
		active := 0
		for id, j := range r.jobs {
			select {
			case <-j.done:
				delete(r.jobs, id)
			default:
				active++
			}
		}
		if active >= 2 {
			r.mu.Unlock()
			return ManagedPrepared{}, errors.New("two managed sessions are already active")
		}
		owner, err := generationID()
		if err != nil {
			r.mu.Unlock()
			return ManagedPrepared{}, err
		}
		session, err := generationID()
		if err != nil {
			r.mu.Unlock()
			return ManagedPrepared{}, err
		}
		jobCtx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		job = &managedJob{id: request.ID, owner: owner, session: session, preparation: request, cancel: cancel, ready: make(chan struct{}), done: make(chan struct{}), phase: "preparing", callbacks: make(map[string]*managedCallback)}
		r.jobs[request.ID] = job
		go r.prepare(jobCtx, job)
	} else if job.preparation != request {
		r.mu.Unlock()
		return ManagedPrepared{}, errors.New("retry the original managed preparation request")
	}
	r.mu.Unlock()
	select {
	case <-job.ready:
	case <-ctx.Done():
		return ManagedPrepared{}, ctx.Err()
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.err != nil {
		return ManagedPrepared{}, job.err
	}
	if job.provider == nil {
		return ManagedPrepared{}, errors.New("managed process is no longer available")
	}
	return ManagedPrepared{2, job.id, job.owner, job.session, job.provider.PID(), job.phase}, nil
}
func (r *ManagedRunner) prepare(ctx context.Context, job *managedJob) {
	defer close(job.ready)
	run, _, err := r.run(ctx, job.id)
	if err == nil && (run.State != "prepared" || run.Kind != "managed" || run.Provider != "codex" || run.Revision != job.preparation.ExpectedRevision || len(run.Brief.Markdown) > 128*1024 || strings.TrimSpace(run.Brief.Markdown) == "") {
		err = errors.New("this managed adapter requires a fresh Codex attempt and a complete brief within 128 KiB")
	}
	claimed := false
	if err == nil {
		claim, e := json.Marshal(struct {
			generationRequest
			Owner   string `json:"owner_id"`
			Session string `json:"session_id"`
			Kind    string `json:"kind"`
		}{job.preparation, job.owner, job.session, "managed"})
		err = e
		if err == nil {
			_, err = r.client.Workbench(ctx, "runs.claim", claim)
			claimed = err == nil
		}
	}
	var provider ManagedSession
	if err == nil {
		provider, err = r.factory(ctx, codingagent.Options{Cwd: run.Cwd, Mode: run.Mode, AcknowledgeBypass: run.Bypass})
	}
	// A failing factory owns its startup cleanup. Its error may contain a typed
	// nil session; never dereference an error result or infer that no process ran.
	if err != nil {
		provider = nil
	}
	if err == nil && provider == nil {
		err = errors.New("managed factory returned no process")
	}
	job.mu.Lock()
	job.provider = provider
	job.err = err
	if err == nil {
		job.phase = "awaiting_activation"
	} else {
		job.phase = "failed"
	}
	job.mu.Unlock()
	if err != nil {
		if provider != nil {
			_, closeErr := provider.Close()
			err = errors.Join(err, closeErr)
		}
		if claimed {
			outcome, providerState := "unresolved", "unresolved"
			if codingagent.NoProcessStarted(err) {
				outcome, providerState = "failed", "failed"
			}
			r.finish(job, outcome, nil, err, providerState)
		}
		job.cancel()
		close(job.done)
		return
	}
	// An abandoned preparation cannot keep a helper alive indefinitely. The
	// activation signal is the persisted running state, checked without a turn.
	go func() {
		timer := time.NewTimer(5 * time.Minute)
		defer timer.Stop()
		select {
		case <-job.done:
			return
		case <-ctx.Done():
		case <-timer.C:
		}
		job.mu.Lock()
		activated := job.activated
		if !activated {
			job.activated = true
		}
		job.mu.Unlock()
		if !activated {
			_, closeErr := provider.Close()
			r.finish(job, "unresolved", nil, errors.Join(errors.New("managed preparation ended before activation"), closeErr), "interrupted")
			job.cancel()
			close(job.done)
		}
	}()
}

// Activate is host-only. process_start must be a fresh native OS observation of
// this exact prepared PID; the renderer is never allowed to supply that evidence.
func (r *ManagedRunner) Activate(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if _, err := enhancementObject(raw, 4096, "id", "owner_id", "session_id", "process_id", "process_start"); err != nil {
		return nil, err
	}
	var in struct {
		ID      string `json:"id"`
		Owner   string `json:"owner_id"`
		Session string `json:"session_id"`
		PID     int    `json:"process_id"`
		Birth   string `json:"process_start"`
	}
	if json.Unmarshal(raw, &in) != nil || in.ID == "" || strings.TrimSpace(in.Birth) == "" || len(in.Birth) > 256 {
		return nil, errors.New("activation requires native process identity evidence")
	}
	r.mu.Lock()
	job := r.jobs[in.ID]
	closed := r.closed
	r.mu.Unlock()
	if job == nil || closed {
		return nil, errors.New("the managed session is not owned by this host")
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.provider == nil || job.err != nil || in.Owner != job.owner || in.Session != job.session || in.PID != job.provider.PID() {
		return nil, errors.New("activation does not match the prepared process")
	}
	if job.activated {
		if len(job.activation) > 0 {
			return append(json.RawMessage(nil), job.activation...), nil
		}
		return nil, errors.New("activation is already consumed or unresolved")
	}
	job.activated = true // A lost store reply cannot launch twice.
	launched := false
	defer func() {
		if !launched {
			job.cancel()
			go r.abandon(job, errors.New("managed activation did not produce a confirmed running receipt"))
		}
	}()
	run, _, err := r.run(ctx, job.id)
	if err != nil {
		return nil, err
	}
	requestID, err := generationID()
	if err != nil {
		return nil, err
	}
	input, err := json.Marshal(struct {
		ID       string `json:"id"`
		Request  string `json:"request_id"`
		Revision int64  `json:"expected_revision"`
		Owner    string `json:"owner_id"`
		Session  string `json:"session_id"`
		PID      int    `json:"process_id"`
		Birth    string `json:"process_start"`
	}{job.id, requestID, run.Revision, job.owner, job.session, in.PID, in.Birth})
	if err != nil {
		return nil, err
	}
	result, err := r.client.Workbench(ctx, "runs.started", input)
	if err != nil {
		return nil, err
	}
	job.activation = append(json.RawMessage(nil), result...)
	job.phase = "running"
	launched = true
	go r.drive(job, run.Brief.Markdown)
	return result, nil
}
func (r *ManagedRunner) abandon(job *managedJob, cause error) {
	_, err := job.provider.Close()
	r.finish(job, "unresolved", nil, errors.Join(cause, err), "interrupted")
	close(job.done)
}
func (r *ManagedRunner) protocol(ctx context.Context, job *managedJob, state, thread string, turn *string) error {
	run, _, err := r.run(ctx, job.id)
	if err != nil {
		return err
	}
	id, err := generationID()
	if err != nil {
		return err
	}
	input, err := json.Marshal(struct {
		ID            string  `json:"id"`
		Request       string  `json:"request_id"`
		Revision      int64   `json:"expected_revision"`
		Owner         string  `json:"owner_id"`
		Session       string  `json:"session_id"`
		Thread        string  `json:"provider_thread_id"`
		Turn          *string `json:"provider_turn_id"`
		State         string  `json:"provider_state"`
		Configuration string  `json:"effective_configuration"`
		Output        string  `json:"output"`
		Truncated     bool    `json:"output_truncated"`
	}{job.id, id, run.Revision, job.owner, job.session, thread, turn, state, job.configuration, job.output.String(), job.truncated})
	if err != nil {
		return err
	}
	_, err = r.client.Workbench(ctx, "runs.protocol", input)
	return err
}
func (r *ManagedRunner) drive(job *managedJob, brief string) {
	defer close(job.done)
	defer job.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	// Session lifetime belongs to the preparation context; closing the runner
	// cancels it. The model turn additionally has a bounded active-time budget.
	err := job.provider.Initialize(ctx)
	status := "failed"
	thread := ""
	configured := false
	if err == nil {
		var encoded []byte
		encoded, err = json.Marshal(job.provider.Effective())
		if err == nil {
			job.configuration = string(encoded)
			thread = job.provider.Effective().Thread.ID
			err = r.protocol(ctx, job, "ready", thread, nil)
			configured = err == nil
		}
	}
	if err == nil {
		err = job.provider.StartTurn(ctx, brief)
	}
	var turn *string
	lastPoll := time.Time{}
	for err == nil {
		for _, callback := range job.callbacks {
			if !time.Now().Before(callback.request.ExpiresAt) {
				err = errors.New("the managed provider request expired without confirmation")
				break
			}
		}
		if err != nil {
			break
		}
		if time.Since(lastPoll) >= 500*time.Millisecond {
			err = r.decisions(ctx, job)
			lastPoll = time.Now()
			if err != nil {
				break
			}
		}
		observe, stop := context.WithTimeout(ctx, 250*time.Millisecond)
		e, readErr := job.provider.Next(observe)
		stop()
		if errors.Is(readErr, context.DeadlineExceeded) && ctx.Err() == nil {
			continue
		}
		if readErr != nil {
			err = readErr
			break
		}
		if e.TurnID != "" && turn == nil {
			v := e.TurnID
			turn = &v
			err = r.protocol(ctx, job, "running", thread, turn)
			if err != nil {
				break
			}
		}
		if e.Request != nil {
			if turn == nil {
				v := e.Request.TurnID
				turn = &v
				err = r.protocol(ctx, job, "running", thread, turn)
				if err != nil {
					break
				}
			}
			id, eid := generationID()
			if eid != nil {
				err = eid
				break
			}
			requestID, eid := generationID()
			if eid != nil {
				err = eid
				break
			}
			capture := store.DecisionRequest{ID: id, RequestID: requestID, RunID: job.id, OwnerID: job.owner, SessionID: job.session, ProviderThreadID: e.Request.ThreadID, ProviderTurnID: e.Request.TurnID, ProtocolRequestID: e.Request.ID, Kind: e.Request.Kind, Payload: e.Request.Payload, Deadline: e.Request.ExpiresAt.Unix()}
			_, err = r.client.CreateDecision(ctx, capture)
			if err != nil {
				break
			}
			job.callbacks[e.Request.ID] = &managedCallback{*e.Request, capture}
		}
		if e.ResolvedID != "" {
			callback := job.callbacks[e.ResolvedID]
			if callback == nil {
				err = errors.New("provider resolved a request absent from durable capture")
				break
			}
			err = r.resolve(ctx, job, callback, "resolved", "Provider confirmed callback resolution")
			delete(job.callbacks, e.ResolvedID)
		}
		if e.Text != "" {
			remaining := 131072 - job.output.Len()
			text := e.Text
			if len(text) > remaining {
				text = text[:remaining]
				for !utf8.ValidString(text) && len(text) > 0 {
					text = text[:len(text)-1]
				}
				job.truncated = true
			}
			job.output.WriteString(text)
		}
		if e.Status != "" {
			status = e.Status
			break
		}
	}
	exit, closeErr := job.provider.Close()
	err = errors.Join(err, closeErr)
	receiptCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	for _, callback := range job.callbacks {
		if resolveErr := r.resolve(receiptCtx, job, callback, "cancelled", "The managed connection ended; callback delivery may be uncertain"); resolveErr != nil {
			err = errors.Join(err, resolveErr)
		}
	}
	if err != nil {
		status = "failed"
		if errors.Is(err, context.Canceled) {
			status = "interrupted"
		}
	}
	finalState := status
	if configured {
		if protocolErr := r.protocol(receiptCtx, job, status, thread, turn); protocolErr != nil {
			err = errors.Join(err, protocolErr)
		}
		finalState = "" // The protocol record owns the initialized provider state.
	}
	outcome := "unresolved"
	var code *int
	// Provider failure and callback uncertainty do not imply that a reaped
	// process is alive. Preserve their reasons; task acceptance remains separate.
	if exit.Reaped && closeErr == nil {
		outcome = "exited"
		if exit.Code >= 0 {
			v := exit.Code
			code = &v
		}
	}
	r.finish(job, outcome, code, err, finalState)
}
func (r *ManagedRunner) decisions(ctx context.Context, job *managedJob) error {
	if len(job.callbacks) == 0 {
		return nil
	}
	input, err := json.Marshal(struct {
		Run   string `json:"run_id"`
		State string `json:"state"`
		Limit int    `json:"limit"`
	}{job.id, "decided", 32})
	if err != nil {
		return err
	}
	raw, err := r.client.Workbench(ctx, "decisions.list", input)
	if err != nil {
		return err
	}
	var page struct {
		Items []struct {
			ID       string  `json:"id"`
			Revision int64   `json:"revision"`
			Request  string  `json:"protocol_request_id"`
			Decision string  `json:"decision"`
			Answer   *string `json:"answer"`
		} `json:"items"`
	}
	if json.Unmarshal(raw, &page) != nil {
		return errors.New("invalid saved decisions")
	}
	for _, item := range page.Items {
		callback := job.callbacks[item.Request]
		if callback == nil || callback.captured.ID != item.ID {
			return errors.New("saved decision belongs to another live callback")
		}
		response := codingagent.Response{Decision: item.Decision}
		if item.Decision == "answer" {
			if item.Answer == nil {
				return errors.New("saved question has no answer")
			}
			if len(callback.request.Questions) == 1 {
				response.Answers = map[string][]string{callback.request.Questions[0].ID: {*item.Answer}}
			} else if json.Unmarshal([]byte(*item.Answer), &response.Answers) != nil {
				return errors.New("multiple questions require answers keyed by question identity")
			}
		}
		id, err := generationID()
		if err != nil {
			return err
		}
		if err = job.provider.ValidateResponse(callback.request, response); err != nil {
			return err
		}
		if _, err = r.client.ClaimDecision(ctx, callback.captured, id, item.Revision); err != nil {
			return err
		}
		if err = job.provider.Respond(ctx, callback.request, response); err != nil {
			return err
		}
	}
	return nil
}
func (r *ManagedRunner) resolve(ctx context.Context, job *managedJob, callback *managedCallback, state, reason string) error {
	input, err := json.Marshal(struct {
		ID string `json:"id"`
	}{callback.captured.ID})
	if err != nil {
		return err
	}
	raw, err := r.client.Workbench(ctx, "decisions.get", input)
	if err != nil {
		return err
	}
	var row struct {
		Item struct {
			Revision int64  `json:"revision"`
			State    string `json:"state"`
		} `json:"item"`
	}
	if json.Unmarshal(raw, &row) != nil {
		return errors.New("invalid callback receipt")
	}
	if row.Item.State == "resolved" || row.Item.State == "cancelled" {
		return nil
	}
	id, err := generationID()
	if err != nil {
		return err
	}
	input, err = json.Marshal(struct {
		ID       string `json:"id"`
		Request  string `json:"request_id"`
		Revision int64  `json:"expected_revision"`
		Owner    string `json:"owner_id"`
		Session  string `json:"session_id"`
		State    string `json:"state"`
		Reason   string `json:"reason"`
	}{callback.captured.ID, id, row.Item.Revision, job.owner, job.session, state, reason})
	if err != nil {
		return err
	}
	_, err = r.client.Workbench(ctx, "decisions.resolve", input)
	return err
}
func (r *ManagedRunner) finish(job *managedJob, outcome string, code *int, cause error, providerState string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	run, _, err := r.run(ctx, job.id)
	if err == nil {
		reason := "Managed provider connection ended; task acceptance requires review"
		if cause != nil {
			reason = r.failureText(cause)
			if len(reason) > 2048 {
				reason = reason[:2048]
				for !utf8.ValidString(reason) {
					reason = reason[:len(reason)-1]
				}
			}
		}
		var id string
		id, err = generationID()
		if err == nil {
			var input []byte
			input, err = json.Marshal(struct {
				ID            string `json:"id"`
				Request       string `json:"request_id"`
				Revision      int64  `json:"expected_revision"`
				Owner         string `json:"owner_id"`
				Session       string `json:"session_id"`
				Outcome       string `json:"outcome"`
				Exit          *int   `json:"exit_code"`
				Reason        string `json:"reason"`
				ProviderState string `json:"provider_state,omitempty"`
			}{job.id, id, run.Revision, job.owner, job.session, outcome, code, reason, providerState})
			if err == nil {
				_, err = r.client.Workbench(ctx, "runs.finish", input)
			}
		}
	}
	job.mu.Lock()
	job.phase = outcome
	job.err = errors.Join(cause, err)
	job.mu.Unlock()
}
func (r *ManagedRunner) Stop(raw json.RawMessage) error {
	if _, err := enhancementObject(raw, 1024, "id"); err != nil {
		return err
	}
	var in struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &in) != nil || in.ID == "" {
		return errors.New("stop requires a managed run identity")
	}
	r.mu.Lock()
	job := r.jobs[in.ID]
	r.mu.Unlock()
	if job == nil {
		return errors.New("this host does not own the managed run")
	}
	job.cancel()
	return nil
}
func (r *ManagedRunner) Close() error {
	r.mu.Lock()
	r.closed = true
	var jobs []*managedJob
	for _, j := range r.jobs {
		jobs = append(jobs, j)
		j.cancel()
	}
	r.mu.Unlock()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for _, j := range jobs {
		select {
		case <-j.done:
		case <-deadline.C:
			return fmt.Errorf("managed shutdown remains unresolved for run %s", j.id)
		}
	}
	return nil
}
