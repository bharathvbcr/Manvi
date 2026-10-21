package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/Manvi/manvi/codingagent"
	"github.com/bharathvbcr/Manvi/manvi/harness"
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

// MaxManagedSessions bounds the provider processes one runner holds at once.
// How many agents run concurrently is the user's choice, enforced where every
// attempt is admitted: the task store's runs.prepare, which a host passes the
// user's limit. A run reaches this runner only after the store admitted it,
// so this is a resource ceiling, not a second product limit, and it equals
// dc-store's MAX_ACTIVE_RUNS_CEILING so it can never refuse what the store
// let through. It was a fixed 2, which refused a third session the store and
// the host had both already accepted.
const MaxManagedSessions = 64

type ManagedRunner struct {
	client      *store.Client
	factory     ManagedFactory
	failureText func(error) string
	mu          sync.Mutex
	jobs        map[string]*managedJob
	closed      bool
	// harness resolves the harness an attempt names; nil until SetHarness.
	harness *HarnessResolver
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
	// harnessRef is the harness the host asked this attempt to carry, zero for
	// none. harnessDigest is the digest of the spec it resolved to, harnessFile
	// the instruction file its block was written to, and materialised what takes
	// the block out again; all four are set before the provider exists.
	harnessRef    ManagedHarness
	harnessDigest string
	harnessFile   string
	materialised  *harnessMaterialisation
	// ctx is the job's lifetime and cancel ends it: Stop, Close and the
	// two-hour bound all act through it. Every provider call derives from it,
	// so a session that owns no process still ends when the job does.
	ctx           context.Context
	cancel        context.CancelFunc
	ready, done   chan struct{}
	provider      ManagedSession
	configuration string
	phase         string
	err           error
	activated     bool
	activation    json.RawMessage
	callbacks     map[string]*managedCallback
	output        strings.Builder
	truncated     bool
}
type managedCallback struct {
	request  codingagent.Request
	captured store.DecisionRequest
	// delivered is set once the human's decision was written to the provider.
	// From then on nothing is awaited from a person, so the expiry that guards
	// an unanswered callback no longer applies; the row is already resolved,
	// whether or not the provider ever confirms (Codex does, Claude does not).
	delivered bool
}
type ManagedPrepared struct {
	ProtocolVersion int    `json:"protocol_version"`
	ID              string `json:"id"`
	Owner           string `json:"owner_id"`
	Session         string `json:"session_id"`
	PID             int    `json:"process_id"`
	Phase           string `json:"phase"`
	// HarnessDigest is the digest of the harness written into the checkout for
	// this attempt, absent when none was asked for. A host records it beside the
	// run; the same digest is in the run's effective configuration.
	HarnessDigest string `json:"harness_digest,omitempty"`
}

// ManagedHarness names the harness a managed attempt should carry. Only a
// repository's own spec (.devcouncil/harness/<name>.json in the checkout) or a
// named spec the process registered is accepted; an inline spec is refused.
//
// Inline is refused here although harness.resolve takes it, because over this
// plane the spec is not advice a host composes but text written into the file an
// unattended agent obeys. A repository or named spec is an artifact somebody
// reviewed and can be named again; an inline one would exist nowhere but in one
// request, whose retry identity and 4 KiB bound it would also have to carry.
type ManagedHarness struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// ManagedPrepareParams is work.runs.managed.prepare's request.
type ManagedPrepareParams struct {
	ID               string `json:"id"`
	RequestID        string `json:"request_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	// Harness is the harness to materialise into the checkout for this attempt.
	// Optional; absent means the attempt runs as it always has.
	Harness *ManagedHarness `json:"harness,omitempty"`
}

// SetHarness gives the runner the means to resolve a harness. Without it an
// attempt that names one is refused, never run without it.
func (r *ManagedRunner) SetHarness(h *HarnessResolver) error {
	if err := h.check(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.harness = h
	return nil
}

// harnessRef decodes and checks the optional harness member of a preparation.
// Absent or null is no harness.
func (r *ManagedRunner) harnessRef(raw json.RawMessage) (ManagedHarness, *Error) {
	var ref ManagedHarness
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ref, nil
	}
	if e := strictParams(raw, "work.runs.managed.prepare harness", &ref); e != nil {
		return ref, e
	}
	switch ref.Kind {
	case HarnessKindRepo, HarnessKindNamed:
	case HarnessKindInline:
		return ManagedHarness{}, harnessRefused("an inline harness is not accepted over the managed plane: commit the spec to %s or register it as a named spec", harness.SpecsDir)
	default:
		return ManagedHarness{}, badRequest("harness kind %q is not one of %s, %s", ref.Kind, HarnessKindRepo, HarnessKindNamed)
	}
	if !harness.ValidName(ref.Name) {
		return ManagedHarness{}, badRequest("harness name %s is not a spec name", harness.QuoteName(ref.Name))
	}
	r.mu.Lock()
	served := r.harness != nil
	r.mu.Unlock()
	if !served {
		return ManagedHarness{}, harnessRefused("this build serves no harness, so the attempt is not started without the one it names")
	}
	return ref, nil
}

// resolveHarness reads and judges the harness for one attempt against the
// provider the run names, and returns the checkout's root, which the spec is
// read from and written under. It reads and judges; it writes nothing.
func (r *ManagedRunner) resolveHarness(ctx context.Context, run managedRun, ref ManagedHarness) (harness.Spec, string, error) {
	// The run's cwd is where the host put the checkout and may be spelled through
	// a symlink (/var for /private/var); it is resolved once here, and the
	// resolved path must still be the top of a git work tree.
	resolved, err := filepath.EvalSymlinks(run.Cwd)
	if err != nil {
		return harness.Spec{}, "", badRoot("cwd does not exist or cannot be resolved: %v", err)
	}
	root, e := canonicalRepoRoot(ctx, "cwd", resolved)
	if e != nil {
		return harness.Spec{}, "", e
	}
	r.mu.Lock()
	h := r.harness
	r.mu.Unlock()
	spec, e := h.resolveSpec(ctx, run.Provider, root, HarnessSpecRef{Kind: ref.Kind, Name: ref.Name})
	if e != nil {
		return harness.Spec{}, "", e
	}
	return spec, root, nil
}

// restoreHarness takes the harness block out of the checkout. It is called from
// finish, once, before the run is recorded finished, so every end of a run
// (success, failure, cancel, stop, abandonment) leaves the checkout without it
// and the diff a verifier takes afterwards is the agent's own.
func (j *managedJob) restoreHarness() error {
	j.mu.Lock()
	m := j.materialised
	j.materialised = nil
	j.mu.Unlock()
	return m.Restore()
}

// managedOwnerID is the owner a managed attempt records:
// manvi-{this process's pid}-{unix nanos now}-{random}. A host reconciling
// after a crash can then prove the owner gone — no process with that pid, or
// one that started after the stamp — the way it already proves a terminal
// launch's owner gone. A random id alone named nothing that could be checked,
// so an attempt whose Manvi died before activation held its checkout forever.
func managedOwnerID() (string, error) {
	token, err := generationID()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("manvi-%d-%d-%s", os.Getpid(), time.Now().UnixNano(), token), nil
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
	fields, err := enhancementObject(raw, 4096, "id", "request_id", "expected_revision", "harness")
	if err != nil {
		return ManagedPrepared{}, err
	}
	var request generationRequest
	if json.Unmarshal(raw, &request) != nil || request.ID == "" || request.RequestID == "" || request.ExpectedRevision < 1 {
		return ManagedPrepared{}, errors.New("managed preparation requires a saved attempt and request identity")
	}
	harnessRef, e := r.harnessRef(fields["harness"])
	if e != nil {
		return ManagedPrepared{}, e
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ManagedPrepared{}, errors.New("managed runner is closed")
	}
	job := r.jobs[request.ID]
	if job != nil {
		// A finished job is history, not a claim on the id. One that failed
		// before runs.claim succeeded left the store row prepared, so the
		// corrected retry (a new request identity included) must be admitted;
		// one that ran to the end is refused by the store, which no longer
		// finds the attempt prepared.
		select {
		case <-job.done:
			delete(r.jobs, request.ID)
			job = nil
		default:
		}
	}
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
		if active >= MaxManagedSessions {
			r.mu.Unlock()
			return ManagedPrepared{}, fmt.Errorf("%d managed sessions are already active, the most one machine supervises; finish or stop one first", MaxManagedSessions)
		}
		owner, err := managedOwnerID()
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
		job = &managedJob{id: request.ID, owner: owner, session: session, preparation: request, harnessRef: harnessRef, ctx: jobCtx, cancel: cancel, ready: make(chan struct{}), done: make(chan struct{}), phase: "preparing", callbacks: make(map[string]*managedCallback)}
		r.jobs[request.ID] = job
		go r.prepare(jobCtx, job)
	} else if job.preparation != request || job.harnessRef != harnessRef {
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
	return ManagedPrepared{2, job.id, job.owner, job.session, job.provider.PID(), job.phase, job.harnessDigest}, nil
}
func (r *ManagedRunner) prepare(ctx context.Context, job *managedJob) {
	defer close(job.ready)
	run, _, err := r.run(ctx, job.id)
	// The bound is checked on the turn the provider will receive, not on the
	// brief alone: a brief just under the adapter's limit would otherwise pass
	// here and be refused by StartTurn after the process was already claimed.
	if err == nil && (run.State != "prepared" || run.Kind != "managed" || !codingagent.IsManagedProvider(run.Provider) || run.Revision != job.preparation.ExpectedRevision || len(withHarnessNote(managedTurn(run.Provider, run.Mode, run.Brief.Markdown), harnessFileFor(job.harnessRef, run.Provider))) > 128*1024 || strings.TrimSpace(run.Brief.Markdown) == "") {
		err = errors.New("a managed attempt must be fresh, name a provider with a managed adapter, and carry a complete brief within 128 KiB")
	}
	// A harness is resolved and judged before the attempt is claimed, so a
	// refused one leaves the saved attempt prepared and the host free to
	// correct it. Nothing is written until the claim has succeeded.
	var spec harness.Spec
	var root string
	if err == nil && job.harnessRef != (ManagedHarness{}) {
		spec, root, err = r.resolveHarness(ctx, run, job.harnessRef)
		if err == nil {
			job.mu.Lock()
			job.harnessDigest = spec.Digest()
			job.mu.Unlock()
		}
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
	// The harness is written into the checkout after the claim and before any
	// process exists, so a failed write aborts the run with nothing running: it
	// never starts without the spec it was asked to carry. It is taken out again
	// when the run finishes, by finish, whichever way it ends.
	aborted := false
	if err == nil && job.harnessRef != (ManagedHarness{}) {
		var m *harnessMaterialisation
		m, err = materialiseHarness(root, run.Provider, spec)
		aborted = err != nil
		job.mu.Lock()
		job.materialised = m
		if m != nil {
			job.harnessFile = m.File
		}
		job.mu.Unlock()
	}
	var provider ManagedSession
	if err == nil {
		provider, err = r.factory(ctx, codingagent.Options{Provider: run.Provider, Cwd: run.Cwd, Mode: run.Mode, AcknowledgeBypass: run.Bypass})
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
			if aborted || codingagent.NoProcessStarted(err) {
				outcome, providerState = "failed", "failed"
			} else if errors.Is(err, context.Canceled) {
				// Stop or Close ended the preparation. The factory owns its
				// cleanup, so a started process still cannot be ruled out and
				// the outcome stays unresolved, but the provider's state is
				// known to be an interruption rather than a failure.
				providerState = "interrupted"
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
	go r.drive(job, managedTurn(run.Provider, run.Mode, run.Brief.Markdown))
	return result, nil
}

// managedTurn is the one message a managed provider receives: what the
// launching side knows about this attempt, then the task brief exactly as the
// store recorded it.
//
// How to work is not repeated here. The store's brief opens with the agent
// guidance every handoff carries, and the run snapshot records it, so a
// managed agent and a terminal agent are told the same thing by construction.
// What only the launcher knows goes here instead:
//
//   - The permission mode decides whether the agent may finish the task. An
//     inspect attempt must not change the task's status, so it is never told
//     how to. The external-terminal lane states the same scope in GitPulse's
//     launch argument; the wording matches on purpose.
//   - The turn is unattended and single: approvals reach the host's decision
//     queue, but a question asked in prose gets no answer before it ends.
//   - A Claude session here runs with `--setting-sources user` (see
//     claudeArgs), which leaves the repository's project context unloaded:
//     measured on Claude Code 2.1.289, the project CLAUDE.md is absent from
//     the session's context. The agent must read those files itself, or it
//     works with none of the repository's rules.
func managedTurn(provider, mode, brief string) string {
	parts := []string{
		"You are a managed coding agent that Manvi started for a saved GitPulse task, in the checkout that is your working directory. This is one unattended turn: permission requests go to the host's approval queue, but nobody can answer a question you ask in text before the turn ends, so when you need a person's decision, stop and say what is needed.",
	}
	if provider == "claude" {
		parts = append(parts, "This session loads user settings only, so the repository's project instructions, rules, skills and MCP servers were not loaded for you. Before anything else, read the repository's AGENTS.md and CLAUDE.md, and any rules or skills directories it has, and follow them.")
	}
	if mode == "inspect" {
		parts = append(parts, "Inspect and propose a plan; do not modify files or change the task's status.")
	} else {
		parts = append(parts, "Carry out the saved task within the selected permission mode. When every acceptance criterion is met and your verification passed, mark the task done with the gitpulse_complete_task tool, using the id on the brief's Task: line and a short summary of what changed and how you verified it; if anything is unfinished or needs a person's judgement, use status review instead and say what remains.")
	}
	parts = append(parts, "Do not publish changes (push, merge, release) on the user's behalf.")
	// Say the brief carries guidance only when this brief does. Manvi and the
	// store that renders briefs ship separately, so a store that predates the
	// section must not be described as having it.
	if strings.Contains(brief, managedGuidanceHeading) {
		parts = append(parts, "The task brief follows. It opens with the agent guidance every GitPulse handoff carries, then the saved task.")
	} else {
		parts = append(parts, "The task brief follows. Before you change anything, read the repository's own instructions and the code the task touches, and verify your work with the project's own checks.")
	}
	return strings.Join(append(parts, brief), "\n\n")
}

// managedGuidanceHeading is the section dc-store's brief renders its agent
// guidance under (dc-store `AGENT_GUIDANCE_HEADING`).
const managedGuidanceHeading = "\n## Agent guidance\n"

func (r *ManagedRunner) abandon(job *managedJob, cause error) {
	_, err := job.provider.Close()
	r.finish(job, "unresolved", nil, errors.Join(cause, err), "interrupted")
	close(job.done)
}

// The effective configuration is written once and never revised.
//
// The store enforces this — a second, different value is refused as
// "the provider identity and effective configuration are immutable for this
// attempt" — and that is the right rule: it is the record of what the run was
// approved under, and a record that can change under the reader is not one. An
// adapter therefore has to be able to describe itself *before* its first turn,
// rather than filling the record in as the stream reveals things.
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
	// Derived from the job's context, never from Background: Stop and Close
	// cancel the job, and that must end Initialize, StartTurn, Next and the
	// decision round-trips for any ManagedSession, not only one whose reads die
	// with its child process. The 45-minute bound is the model turn's
	// active-time budget on top of that.
	ctx, cancel := context.WithTimeout(job.ctx, 45*time.Minute)
	defer cancel()
	err := job.provider.Initialize(ctx)
	status := "failed"
	thread := ""
	configured := false
	if err == nil {
		var encoded []byte
		// The adapter's own description, flattened, with the digest of the harness
		// written for this attempt beside it when there is one. The string is
		// still the effective configuration it always was; the key is additive
		// and absent without a harness.
		encoded, err = json.Marshal(struct {
			codingagent.Effective
			HarnessDigest string `json:"harness_digest,omitempty"`
		}{job.provider.Effective(), job.harnessDigest})
		if err == nil {
			job.configuration = string(encoded)
			thread = job.provider.Effective().Thread.ID
			err = r.protocol(ctx, job, "ready", thread, nil)
			configured = err == nil
		}
	}
	if err == nil {
		err = job.provider.StartTurn(ctx, withHarnessNote(brief, job.harnessFile))
	}
	var turn *string
	lastPoll := time.Time{}
	for err == nil {
		for _, callback := range job.callbacks {
			if !callback.delivered && !time.Now().Before(callback.request.ExpiresAt) {
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
			job.callbacks[e.Request.ID] = &managedCallback{request: *e.Request, captured: capture}
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
		if e.CancelledID != "" {
			// A callback the provider withdrew before it was answered. One
			// already delivered has nothing to withdraw, and one never captured
			// is not ours to resolve.
			if callback := job.callbacks[e.CancelledID]; callback != nil && !callback.delivered {
				err = r.resolve(ctx, job, callback, "cancelled", withdrawnReason)
				delete(job.callbacks, e.CancelledID)
			}
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
		state, reason := "cancelled", "The managed connection ended; callback delivery may be uncertain"
		if callback.delivered {
			// Retries the receipt written at delivery, which is a no-op once the
			// row is resolved.
			state, reason = "resolved", deliveredReason
		}
		if resolveErr := r.resolve(receiptCtx, job, callback, state, reason); resolveErr != nil {
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
			if errors.Is(err, codingagent.ErrCallbackWithdrawn) {
				if err = r.resolve(ctx, job, callback, "cancelled", withdrawnReason); err != nil {
					return err
				}
				delete(job.callbacks, item.Request)
				continue
			}
			return err
		}
		if _, err = r.client.ClaimDecision(ctx, callback.captured, id, item.Revision); err != nil {
			return err
		}
		if err = job.provider.Respond(ctx, callback.request, response); err != nil {
			return err
		}
		// Respond succeeded, so the decision is delivered. Recorded now rather
		// than waiting for a provider confirmation: only Codex sends one, and a
		// Claude approval was otherwise left live until the run ended and then
		// recorded as cancelled, the opposite of what happened.
		callback.delivered = true
		if err = r.resolve(ctx, job, callback, "resolved", deliveredReason); err != nil {
			return err
		}
	}
	return nil
}

// deliveredReason is the receipt for a decision written to the provider.
const deliveredReason = "Decision delivered to the provider"

// withdrawnReason is the receipt for a callback the provider cancelled.
const withdrawnReason = "The provider withdrew the request before it was answered"

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
	// Before anything is recorded, and whatever the run's end: the checkout is
	// given back without the harness block. A restore that fails is part of the
	// reason the run is recorded with, typed, because a block left behind is a
	// change in the checkout the agent did not make.
	if restoreErr := job.restoreHarness(); restoreErr != nil {
		cause = errors.Join(cause, restoreErr)
	}
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
