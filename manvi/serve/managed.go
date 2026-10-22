package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/Manvi/manvi/codingagent"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
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
	// follows is armed follow-ups keyed by the run that queued them. The record
	// is process memory: when this sidecar exits, a message that was not yet
	// launched is gone. taken is set when a later attempt's factory starts, so
	// two polls cannot launch two processes; a failure before that factory
	// leaves the record so the host can retry.
	follows map[string]*armedFollow
	closed  bool
	// harness resolves the harness an attempt names; nil until SetHarness.
	harness *HarnessResolver
	// checkoutMu makes the conflict check on a checkout and the attempt's
	// registration on it one step; see enterCheckout. Taken before mu and before
	// any job's mu.
	checkoutMu sync.Mutex
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
	// checkout is the resolved directory this attempt runs in, wantsHarness
	// whether it asked for a harness, and recovery what Prepare found and
	// cleared there before anything else (see enterCheckout).
	checkout     string
	wantsHarness bool
	recovery     string
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
	// tools is the latest snapshot of each provider tool call, replaced by id.
	// It is not appended to output until a protocol write, so the text the turn
	// is still producing stays text. wake is a buffered hint that a decision
	// was saved; a nil channel means nobody is listening and a nudge is a no-op.
	tools []codingagent.Tool
	wake  chan struct{}
	// pending is set by Wake and cleared when decisions() runs, so a hint that
	// arrives in the same moment as a provider frame is still applied.
	pending atomic.Bool
	// followSet is one message queued while this run is still going. It is
	// read once, after finish releases the checkout. providerName, cwd and mode
	// are copied from the saved attempt so the next attempt can be checked
	// against them. parentID, when set, is the run whose armed follow-up this
	// attempt continues, and turnText is that message.
	followSet     bool
	followMessage string
	providerName  string
	cwd, mode     string
	parentID      string
	turnText      string
	settled       bool
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
	// HarnessRecovery says that this Prepare took, out of the checkout, a harness
	// block a run that never finished had left, before doing anything else. Absent
	// when there was nothing to take out.
	HarnessRecovery string `json:"harness_recovery,omitempty"`
}

// ErrHarnessBusy means the checkout is held: a managed run with a harness is
// live on it, or this attempt asks for a harness and another run is. Not
// retryable until that run ends.
const ErrHarnessBusy = "E_HARNESS_BUSY"

func harnessBusy(format string, args ...any) *Error {
	return &Error{Code: ErrHarnessBusy, Message: fmt.Sprintf(format, args...)}
}

// enterCheckout is what an attempt does about its checkout before anything else
// touches it, and it does it for every attempt, with a harness or without:
//
//   - one live harness per checkout. An attempt that asks for a harness is
//     refused while any other managed run is live on that checkout, and an
//     attempt that does not is refused while a run with a harness is, because the
//     block is in the instruction file and in the checkout's diff until that run
//     ends, and a second run would read it as repository content and have it in
//     its own diff. Two attempts without a harness share a checkout as they
//     always did. The check and the claim are one step under checkoutMu.
//   - a block a run left behind (Manvi died mid-run) is taken out, by the same
//     exact-bytes rule as any restore, and reported in the prepare result. A block
//     that cannot be taken out is refused once, typed, and the record is cleared.
//
// A checkout that cannot be resolved has nothing to guard; a harness attempt on
// one is refused where its spec is read.
func (r *ManagedRunner) enterCheckout(job *managedJob, run managedRun) error {
	wants := job.harnessRef != (ManagedHarness{})
	resolved, ok := resolvedCheckout(run.Cwd)
	if !ok {
		if wants {
			return badRoot("cwd does not exist or cannot be resolved")
		}
		return nil // nothing to guard for an attempt with no harness
	}
	r.checkoutMu.Lock()
	defer r.checkoutMu.Unlock()
	r.mu.Lock()
	others := make([]*managedJob, 0, len(r.jobs))
	for _, o := range r.jobs {
		if o != job {
			others = append(others, o)
		}
	}
	r.mu.Unlock()
	for _, o := range others {
		select {
		case <-o.done:
			continue
		default:
		}
		o.mu.Lock()
		same, harnessed := o.checkout == resolved, o.wantsHarness
		o.mu.Unlock()
		switch {
		case !same:
		case wants:
			return harnessBusy("another managed run is live on this checkout, and an attempt with a harness takes it alone: its block would be in the other run's diff; try again when that run ends")
		case harnessed:
			return harnessBusy("a managed run with a harness is live on this checkout: its block is in the instruction file and in the checkout's diff until it ends; try again when that run ends")
		}
	}
	note, err := recoverPendingHarness(resolved)
	if err != nil {
		return err
	}
	job.mu.Lock()
	job.checkout, job.wantsHarness, job.recovery = resolved, wants, note
	job.mu.Unlock()
	return nil
}

// resolvedCheckout is a directory with its symbolic links resolved, and whether
// it could be.
func resolvedCheckout(dir string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(dir)
	return resolved, err == nil
}

// HarnessBusy reports, for a worktree, whether a harness block may be in it: a
// managed run with a harness is live on it, or one left a record that has not
// been applied. Anything that captures the worktree's diff asks first and
// refuses, so no check judges a change the agent did not make. It is refused and
// not worked around by excluding the span: a refusal is honest about what the
// diff would have held. Nil means no block is known to be there.
func (r *ManagedRunner) HarnessBusy(worktree string) *Error {
	resolved, ok := resolvedCheckout(worktree)
	if !ok {
		return nil
	}
	r.mu.Lock()
	jobs := make([]*managedJob, 0, len(r.jobs))
	for _, j := range r.jobs {
		jobs = append(jobs, j)
	}
	r.mu.Unlock()
	for _, j := range jobs {
		select {
		case <-j.done:
			continue
		default:
		}
		j.mu.Lock()
		live := j.checkout == resolved && j.materialised != nil
		j.mu.Unlock()
		if live {
			return harnessBusy("a managed harness run is in progress on this worktree: its block is in the checkout until the run ends, so its diff is not the agent's; verify again when the run has ended")
		}
	}
	if _, err := devcouncil.ReadPinned(resolved, harnessPendingFile, maxPendingBytes); err == nil {
		return harnessBusy("a managed harness run left its block in this worktree and it has not been taken out; the next work.runs.managed.prepare on this checkout takes it out and reports it")
	}
	return nil
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
	return &ManagedRunner{client: client, factory: factory, failureText: failureText, jobs: make(map[string]*managedJob), follows: make(map[string]*armedFollow)}, nil
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
	fields, err := enhancementObject(raw, 4096, "id", "request_id", "expected_revision", "harness", "parent_run_id")
	if err != nil {
		return ManagedPrepared{}, err
	}
	var request generationRequest
	if json.Unmarshal(raw, &request) != nil || request.ID == "" || request.RequestID == "" || request.ExpectedRevision < 1 {
		return ManagedPrepared{}, errors.New("managed preparation requires a saved attempt and request identity")
	}
	parentID, err := followParent(fields["parent_run_id"])
	if err != nil {
		return ManagedPrepared{}, err
	}
	if parentID == request.ID {
		return ManagedPrepared{}, errors.New("a follow-up names the run it continues")
	}
	if parentID != "" {
		if err := r.followAvailable(parentID); err != nil {
			return ManagedPrepared{}, err
		}
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
		job = &managedJob{id: request.ID, owner: owner, session: session, preparation: request, harnessRef: harnessRef, parentID: parentID, ctx: jobCtx, cancel: cancel, ready: make(chan struct{}), done: make(chan struct{}), wake: make(chan struct{}, 1), phase: "preparing", callbacks: make(map[string]*managedCallback)}
		r.jobs[request.ID] = job
		go r.prepare(jobCtx, job)
	} else if job.preparation != request || job.harnessRef != harnessRef || job.parentID != parentID {
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
	return ManagedPrepared{2, job.id, job.owner, job.session, job.provider.PID(), job.phase, job.harnessDigest, job.recovery}, nil
}
func (r *ManagedRunner) prepare(ctx context.Context, job *managedJob) {
	defer close(job.ready)
	run, _, err := r.run(ctx, job.id)
	if err == nil {
		job.providerName, job.cwd, job.mode = run.Provider, run.Cwd, run.Mode
	}
	// The bound is checked on the turn the provider will receive, not on the
	// brief alone: a brief just under the adapter's limit would otherwise pass
	// here and be refused by StartTurn after the process was already claimed.
	// A follow-up's turn is the queued message, not the task brief again.
	if err == nil && job.parentID != "" {
		turnText, matchErr := r.followMessage(job.parentID, run.Provider, run.Cwd, run.Mode)
		if matchErr != nil {
			err = matchErr
		} else if len(withHarnessNote(turnText, harnessFileFor(job.harnessRef, run.Provider))) > 128*1024 || strings.TrimSpace(turnText) == "" {
			err = errors.New("a managed attempt must be fresh, name a provider with a managed adapter, and carry a complete brief within 128 KiB")
		} else {
			job.turnText = turnText
		}
	}
	if err == nil && (run.State != "prepared" || run.Kind != "managed" || !codingagent.IsManagedProvider(run.Provider) || run.Revision != job.preparation.ExpectedRevision || len(withHarnessNote(managedTurn(run.Provider, run.Mode, run.Brief.Markdown), harnessFileFor(job.harnessRef, run.Provider))) > 128*1024 || strings.TrimSpace(run.Brief.Markdown) == "") {
		err = errors.New("a managed attempt must be fresh, name a provider with a managed adapter, and carry a complete brief within 128 KiB")
	}
	if err == nil {
		err = r.enterCheckout(job, run)
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
	resume := ""
	if err == nil && job.parentID != "" {
		thread, ok := r.takeFollow(job.parentID)
		if !ok {
			err = errors.New("that follow-up is already being launched")
		} else {
			resume = thread
		}
	}
	if err == nil {
		provider, err = r.factory(ctx, codingagent.Options{Provider: run.Provider, Cwd: run.Cwd, Mode: run.Mode, AcknowledgeBypass: run.Bypass, Resume: resume})
	}
	// The slot was taken so a second poll cannot start a second process. A
	// factory that never started one gives the slot back; the host can retry.
	if job.parentID != "" && resume != "" && err != nil && codingagent.NoProcessStarted(err) {
		r.releaseFollow(job.parentID, resume)
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
			// finish keeps its store error on the job (job.err), which is
			// where this goroutine's caller reads the run's end from.
			_ = r.finish(job, outcome, nil, err, providerState)
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
			// The store error, if any, is kept on the job by finish.
			_ = r.finish(job, "unresolved", nil, errors.Join(errors.New("managed preparation ended before activation"), closeErr), "interrupted")
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
	if job.parentID != "" && strings.TrimSpace(job.turnText) == "" {
		return nil, errors.New("the follow-up has no message")
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
	// A follow-up's session already has the task. Sending the brief again would
	// repeat the first turn. The queued message is the one turn this process gets.
	brief := managedTurn(run.Provider, run.Mode, run.Brief.Markdown)
	if job.parentID != "" {
		brief = job.turnText
	}
	go r.drive(job, brief)
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
	// The store error, if any, is kept on the job by finish.
	_ = r.finish(job, "unresolved", nil, errors.Join(cause, err), "interrupted")
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
	output, truncated := savedOutput(job)
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
	}{job.id, id, run.Revision, job.owner, job.session, thread, turn, state, job.configuration, output, truncated})
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
	ctx, cancel := context.WithTimeout(job.ctx, managedTurnBudget)
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
	lastCheckpoint := time.Time{}
	for err == nil {
		e, readErr := r.await(ctx, job)
		if readErr != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
				break
			}
			// A decision was saved, or the earliest unanswered callback reached
			// its own deadline. Read decisions before declaring the expiry, so
			// one that landed in time is delivered.
			if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
				if err = r.decisions(ctx, job); err != nil {
					break
				}
				if err = callbackExpiry(job); err != nil {
					break
				}
				continue
			}
			err = readErr
			break
		}
		changed := false
		if e.Text != "" {
			remaining := maxManagedOutput - job.output.Len()
			text := e.Text
			if len(text) > remaining {
				text = text[:remaining]
				for !utf8.ValidString(text) && len(text) > 0 {
					text = text[:len(text)-1]
				}
				job.truncated = true
			}
			if text != "" {
				job.output.WriteString(text)
				changed = true
			}
		}
		if noteTools(job, e.Tools) {
			changed = true
		}
		checkpointed := false
		if e.TurnID != "" && turn == nil {
			v := e.TurnID
			turn = &v
			err = r.protocol(ctx, job, "running", thread, turn)
			checkpointed = true
			lastCheckpoint = time.Now()
			if err != nil {
				break
			}
		}
		if e.Request != nil {
			if turn == nil {
				v := e.Request.TurnID
				turn = &v
				err = r.protocol(ctx, job, "running", thread, turn)
				checkpointed = true
				lastCheckpoint = time.Now()
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
			// A decision can already be saved by the time the row exists.
			if err = r.decisions(ctx, job); err != nil {
				break
			}
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
		if job.pending.Load() {
			if err = r.decisions(ctx, job); err != nil {
				break
			}
		}
		if e.Status != "" {
			status = e.Status
			break
		}
		if configured && turn != nil && changed && !checkpointed && time.Since(lastCheckpoint) >= outputCheckpoint {
			if err = r.protocol(ctx, job, "running", thread, turn); err != nil {
				break
			}
			lastCheckpoint = time.Now()
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
	protocolOK := false
	if configured {
		if protocolErr := r.protocol(receiptCtx, job, status, thread, turn); protocolErr != nil {
			err = errors.Join(err, protocolErr)
		} else {
			protocolOK = true
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
	finishErr := r.finish(job, outcome, code, err, finalState)
	// Arm only after finish has released the checkout, and never for a timeout.
	// A provider-reported failure and a clean stop both arm. The queued message
	// is snapshotted here: a follow that arrives after this is refused.
	if followTerminal(status, protocolOK, closeErr == nil, err, finishErr) {
		r.armFollow(job, thread)
	} else {
		job.mu.Lock()
		job.settled = true
		job.mu.Unlock()
	}
}

// outputCheckpoint is how often a still-running turn writes its text and tool
// snapshot. The final protocol at Close is not gated by it. Tests shorten it.
var outputCheckpoint = time.Second

// managedTurnBudget is the model turn's active-time limit. Tests shorten it.
// Serve tests do not use t.Parallel.
var managedTurnBudget = 45 * time.Minute

const (
	maxManagedOutput = 131072
	maxManagedTools  = 64
	toolsMarker      = "\n[tools]\n"
)

// await blocks on the provider until the next frame, a decision wake, or the
// earliest unanswered callback's own deadline. With no callback open there is
// no timer: the read waits on the provider. A nil wake channel never fires.
func (r *ManagedRunner) await(ctx context.Context, job *managedJob) (codingagent.Event, error) {
	deadline := time.Time{}
	for _, callback := range job.callbacks {
		if callback.delivered {
			continue
		}
		if deadline.IsZero() || callback.request.ExpiresAt.Before(deadline) {
			deadline = callback.request.ExpiresAt
		}
	}
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return codingagent.Event{}, context.DeadlineExceeded
	}
	observe, stop := context.WithCancel(ctx)
	defer stop()
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		observe, cancel = context.WithDeadline(observe, deadline)
		defer cancel()
	}
	if job.wake != nil {
		go func() {
			select {
			case <-job.wake:
				stop()
			case <-observe.Done():
			}
		}()
	}
	return job.provider.Next(observe)
}

func callbackExpiry(job *managedJob) error {
	now := time.Now()
	for _, callback := range job.callbacks {
		if !callback.delivered && !now.Before(callback.request.ExpiresAt) {
			return errors.New("the managed provider request expired without confirmation")
		}
	}
	return nil
}

// savedOutput is the record written by runs.protocol. The tool snapshot is
// appended only when the whole suffix fits in the existing output cap; a
// snapshot that would have to be cut mid-JSON is omitted and the record is
// marked truncated. The in-memory text is left unchanged.
func savedOutput(job *managedJob) (string, bool) {
	text := job.output.String()
	truncated := job.truncated
	if len(job.tools) == 0 {
		return text, truncated
	}
	payload, err := json.Marshal(job.tools)
	if err != nil {
		return text, true
	}
	suffix := toolsMarker + string(payload)
	if len(text)+len(suffix) > maxManagedOutput {
		return text, true
	}
	return text + suffix, truncated
}

// noteTools merges this frame's tools into the job snapshot, replacing by id
// and keeping a prior field when the new one is empty. Past maxManagedTools,
// further new ids are dropped and the output is marked truncated.
func noteTools(job *managedJob, incoming []codingagent.Tool) bool {
	changed := false
	for _, tool := range incoming {
		tool, ok := codingagent.BoundTool(tool)
		if !ok {
			continue
		}
		idx := -1
		for i := range job.tools {
			if job.tools[i].ID == tool.ID {
				idx = i
				break
			}
		}
		if idx >= 0 {
			merged := mergeTool(job.tools[idx], tool)
			if merged != job.tools[idx] {
				job.tools[idx] = merged
				changed = true
			}
			continue
		}
		if len(job.tools) >= maxManagedTools {
			job.truncated = true
			continue
		}
		job.tools = append(job.tools, tool)
		changed = true
	}
	return changed
}

func mergeTool(prev, next codingagent.Tool) codingagent.Tool {
	if next.Name == "" {
		next.Name = prev.Name
	}
	if next.Status == "" {
		next.Status = prev.Status
	}
	if next.ParentID == "" {
		next.ParentID = prev.ParentID
	}
	if next.Input == "" {
		next.Input = prev.Input
	}
	if next.Output == "" {
		next.Output = prev.Output
	}
	return next
}

func nudge(job *managedJob) {
	if job == nil {
		return
	}
	job.pending.Store(true)
	if job.wake == nil {
		return
	}
	select {
	case job.wake <- struct{}{}:
	default:
	}
}

// Wake asks the drive loop of a live run to read decisions now. An unknown or
// ended run is a no-op, and an empty id wakes every live job: a decision
// payload names the callback, not the run. It never fails the decision that
// triggered it.
func (r *ManagedRunner) Wake(raw json.RawMessage) error {
	var in struct {
		ID string `json:"id"`
	}
	if len(bytes.TrimSpace(raw)) > 0 && json.Unmarshal(raw, &in) != nil {
		return errors.New("wake requires a managed run identity")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if in.ID == "" {
		for _, job := range r.jobs {
			nudge(job)
		}
		return nil
	}
	nudge(r.jobs[in.ID])
	return nil
}

// maxFollowMessage is the same bound StartTurn already enforces. The body cap
// leaves room for the JSON around that message.
const maxFollowMessage = 128 * 1024

// armedFollow is one queued message waiting for a new process. It is keyed by
// the run that queued it. The sidecar is the only copy: when that process
// exits, a message that was not launched is gone.
type armedFollow struct {
	threadID, message, provider, cwd, mode string
	taken                                  bool
}

type followResult struct {
	State    string `json:"state"`
	ThreadID string `json:"thread_id,omitempty"`
	Message  string `json:"message,omitempty"`
	Provider string `json:"provider,omitempty"`
	Cwd      string `json:"cwd,omitempty"`
	Mode     string `json:"mode,omitempty"`
}

// followTerminal reports whether a queued follow-up may be armed. A timeout is
// never armed, even when the provider state was recorded as failed. A
// provider-reported failure and a clean stop both arm when the close, the
// protocol receipt, and the store finish all succeeded.
func followTerminal(status string, protocolOK, closeOK bool, cause, finishErr error) bool {
	if !protocolOK || !closeOK || finishErr != nil {
		return false
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return false
	}
	switch status {
	case "completed", "failed", "interrupted":
		return true
	default:
		return false
	}
}

func managedID(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 256 && !strings.ContainsRune(s, '\x00')
}

func followParent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var id string
	if json.Unmarshal(raw, &id) != nil || !managedID(id) {
		return "", errors.New("a follow-up names the run it continues")
	}
	return id, nil
}

func (r *ManagedRunner) followAvailable(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.follows[id]
	if f == nil {
		return errors.New("no follow-up is armed for that run")
	}
	if f.taken {
		return errors.New("that follow-up is already being launched")
	}
	return nil
}

func (r *ManagedRunner) followMessage(id, provider, cwd, mode string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.follows[id]
	if f == nil {
		return "", errors.New("no follow-up is armed for that run")
	}
	if f.taken {
		return "", errors.New("that follow-up is already being launched")
	}
	if f.provider != provider || f.cwd != cwd || f.mode != mode {
		return "", errors.New("the follow-up does not match this attempt")
	}
	return f.message, nil
}

func (r *ManagedRunner) takeFollow(id string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.follows[id]
	if f == nil || f.taken || !managedID(f.threadID) {
		return "", false
	}
	f.taken = true
	return f.threadID, true
}

func (r *ManagedRunner) releaseFollow(id, thread string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.follows[id]
	if f != nil && f.threadID == thread {
		f.taken = false
	}
}

func (r *ManagedRunner) armFollow(job *managedJob, thread string) {
	job.mu.Lock()
	set, message := job.followSet, job.followMessage
	provider, cwd, mode := job.providerName, job.cwd, job.mode
	job.settled = true
	job.mu.Unlock()
	if !set || strings.TrimSpace(message) == "" || !managedID(thread) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.follows == nil {
		r.follows = make(map[string]*armedFollow)
	}
	if r.follows[job.id] != nil {
		return
	}
	r.follows[job.id] = &armedFollow{threadID: thread, message: message, provider: provider, cwd: cwd, mode: mode}
}

// Follow queues one message on a live run, or reports that queue. A second
// message while one is pending is refused. An empty message is a status read.
// The message is not sent on the process that is closing; the next attempt's
// StartTurn receives it after this run has released the checkout.
func (r *ManagedRunner) Follow(raw json.RawMessage) (followResult, error) {
	if len(raw) > maxFollowMessage+1024 || !utf8.Valid(raw) {
		return followResult{}, errors.New("a follow-up accepts one bounded, nonblank message")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var in struct {
		ID      string  `json:"id"`
		Message *string `json:"message"`
	}
	if err := dec.Decode(&in); err != nil || !managedID(in.ID) {
		return followResult{}, errors.New("follow-up requires a managed run identity")
	}
	var trail json.RawMessage
	if err := dec.Decode(&trail); !errors.Is(err, io.EOF) {
		return followResult{}, errors.New("follow-up requires a managed run identity")
	}
	if in.Message == nil {
		return r.followStatus(in.ID)
	}
	message := *in.Message
	if strings.TrimSpace(message) == "" || len(message) > maxFollowMessage {
		return followResult{}, errors.New("a follow-up accepts one bounded, nonblank message")
	}
	return r.queueFollow(in.ID, message)
}

func (r *ManagedRunner) queueFollow(id, message string) (followResult, error) {
	r.mu.Lock()
	closed := r.closed
	job := r.jobs[id]
	armed := r.follows[id]
	r.mu.Unlock()
	if closed {
		return followResult{}, errors.New("managed runner is closed")
	}
	if armed != nil {
		return followResult{}, errors.New("a follow-up is already queued for this run")
	}
	if job == nil {
		return followResult{}, errors.New("this host does not own the managed run")
	}
	select {
	case <-job.done:
		return followResult{}, errors.New("a follow-up can only be queued while the managed run is still going")
	default:
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.settled {
		return followResult{}, errors.New("a follow-up can only be queued while the managed run is still going")
	}
	if job.followSet {
		return followResult{}, errors.New("a follow-up is already queued for this run")
	}
	job.followSet = true
	job.followMessage = message
	return followResult{State: "queued", Message: message}, nil
}

func (r *ManagedRunner) followStatus(id string) (followResult, error) {
	r.mu.Lock()
	f := r.follows[id]
	job := r.jobs[id]
	r.mu.Unlock()
	if f != nil {
		state := "ready"
		if f.taken {
			state = "launching"
		}
		return followResult{State: state, ThreadID: f.threadID, Message: f.message, Provider: f.provider, Cwd: f.cwd, Mode: f.mode}, nil
	}
	if job != nil {
		job.mu.Lock()
		set, settled, message := job.followSet, job.settled, job.followMessage
		job.mu.Unlock()
		if set && !settled {
			return followResult{State: "queued", Message: message}, nil
		}
		if set && settled {
			return followResult{}, errors.New("the follow-up was not kept; this run did not end in a way that can resume")
		}
	}
	return followResult{}, errors.New("no follow-up is queued for this run")
}

func (r *ManagedRunner) decisions(ctx context.Context, job *managedJob) error {
	job.pending.Store(false)
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
func (r *ManagedRunner) finish(job *managedJob, outcome string, code *int, cause error, providerState string) error {
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
	storeErr := err
	job.mu.Lock()
	job.phase = outcome
	job.err = errors.Join(cause, storeErr)
	job.mu.Unlock()
	return storeErr
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
