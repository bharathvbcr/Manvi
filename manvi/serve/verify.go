package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
)

// work.verify.task answers one question for a board task: is it really done,
// judged by evidence this process gathers itself rather than by the agent's
// word. It runs the operator's verification command and the rigor gates over
// the paths git says changed, asks DevMap which tests reach them, and has a
// read-only critic score each acceptance criterion.
//
// Authority. The op is host-plane: it is registered only by WorkbenchModule,
// which the owning host enables explicitly, and it is not an agent tool. It
// runs no command that arrives in the request. The only command it executes is
// VerifyDeps.Command, which the host takes from operator scope (see
// operatorVerifyCommand in cmd/manvi); the request names a worktree, two
// revisions and a board item, and nothing else.
//
// Shape. Verification outlives a lane request, whose deadline is a minute, so
// it follows the enhancement pattern: validate, durably claim, answer at once
// with the claimed record, and run on a job context of its own. Progress and
// the verdict are read back through work.verify.get.
//
// Outcome. passed, failed or degraded, never a quiet fourth state. Passed
// requires every check to have run and every criterion to be met; a check that
// could not run degrades the verdict, and a defect found fails it whatever else
// could not run.

const (
	// OpVerifyTask starts, or replays, one verification.
	OpVerifyTask = "work.verify.task"
	// OpVerifyGet reads a verification record.
	OpVerifyGet = "work.verify.get"

	// verifyEvidenceType is the dc-store evidence type a record is stored under.
	// Evidence rows are append-only; the newest row for a request_id is its
	// current state.
	verifyEvidenceType = "work.verify.task"
	// verifyRunSandbox labels the dc-store verification run row.
	verifyRunSandbox = "work.verify.task"

	// DefaultVerifyTimeout bounds one verification job end to end.
	DefaultVerifyTimeout = 15 * time.Minute
	// verifyMaxActive bounds concurrent verification jobs. Each holds a critic
	// turn and a verification command, so the bound is on cost, not on memory.
	verifyMaxActive = 4

	// maxVerifyChanged bounds how many changed paths one verification collects.
	// Past it the change set is reported as capped and the verdict degrades;
	// the paths beyond are counted, never silently dropped.
	maxVerifyChanged = 2000
	// maxVerifyGitOutput bounds one git listing read into memory.
	maxVerifyGitOutput = 8 << 20
	// maxVerifyDiffPrompt bounds the diff text handed to the critic.
	maxVerifyDiffPrompt = 32 << 10
	// maxVerifyCriterionRunes bounds one criterion's text in a record and prompt.
	maxVerifyCriterionRunes = 1000
	// maxVerifyCriteria bounds how many criteria one verification judges.
	maxVerifyCriteria = 50

	// maxVerifyRecordBytes keeps a stored record under what one dcstore
	// argument carries (Linux caps a single argument at 128 KiB).
	maxVerifyRecordBytes = 60 << 10
)

// Verification outcomes.
const (
	VerifyPassed   = devcouncil.VerdictPassed
	VerifyFailed   = devcouncil.VerdictFailed
	VerifyDegraded = devcouncil.VerdictDegraded
)

// Verification states.
const (
	verifyRunning = "running"
	verifySettled = "settled"
)

// VerifyCritic runs one read-only critic turn and returns its final answer.
//
// The implementation owns read-only enforcement: it must give the critic a
// tool registry without write tools, not a registry it is asked not to use.
// criteria is the number of acceptance criteria the prompt asks it to judge, so
// the implementation can state the verdict contract that matches.
type VerifyCritic interface {
	Judge(ctx context.Context, prompt string, criteria int) (string, error)
}

// VerifyDeps is everything a VerifyRunner takes from its host.
type VerifyDeps struct {
	// Client is the profile store holding the board item and the record.
	Client *store.Client
	// Critic judges the acceptance criteria.
	Critic VerifyCritic
	// Command is the operator's verification command, taken from operator
	// scope. Empty means none is configured, and the check runs without one.
	Command string
	// VerifierBinary and CoverageFile configure the rigor gates.
	VerifierBinary string
	CoverageFile   string
	// Devmap returns the code-intelligence client for a worktree.
	Devmap func(worktree string) DevmapClient
	// NewVerifier builds the path verifier for a worktree. Nil uses
	// devcouncil.NewPathVerifier.
	NewVerifier func(root, verifierBinary, coverageFile string) (devcouncil.PathChecker, error)
	// Timeout bounds one job. Zero means DefaultVerifyTimeout.
	Timeout time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// VerifyRunner owns verification jobs. Storage claims make a request replay
// the same record after a lost reply; the in-process job table is what lets a
// replay tell a live job from one whose process died.
type VerifyRunner struct {
	deps VerifyDeps

	mu     sync.Mutex
	jobs   map[string]*verifyJob // request_id -> live job
	closed bool
	wg     sync.WaitGroup
}

type verifyJob struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// NewVerifyRunner validates deps and returns a runner.
func NewVerifyRunner(deps VerifyDeps) (*VerifyRunner, error) {
	if deps.Client == nil || nilInterface(deps.Critic) || deps.Devmap == nil {
		return nil, errors.New("verify runner requires storage, a critic and a devmap client source")
	}
	if deps.NewVerifier == nil {
		deps.NewVerifier = func(root, binary, coverage string) (devcouncil.PathChecker, error) {
			return devcouncil.NewPathVerifier(root, binary, coverage)
		}
	}
	if deps.Timeout <= 0 {
		deps.Timeout = DefaultVerifyTimeout
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &VerifyRunner{deps: deps, jobs: map[string]*verifyJob{}}, nil
}

// VerifyTaskParams is the work.verify.task request. It carries no command, no
// path list and no criteria: the changed paths come from git and the criteria
// from the stored board item.
type VerifyTaskParams struct {
	RequestID string `json:"request_id"`
	ItemID    string `json:"item_id"`
	Worktree  string `json:"worktree"`
	Base      string `json:"base"`
	Head      string `json:"head"`
}

// VerifyGetParams is the work.verify.get request.
type VerifyGetParams struct {
	ItemID    string `json:"item_id"`
	RequestID string `json:"request_id,omitempty"`
}

// VerifyChanges describes the change set the verdict is about.
type VerifyChanges struct {
	// Paths are the changed paths examined, capped at what a record holds.
	Paths []string `json:"paths"`
	// Total counts every changed path git reported, including those past the
	// cap. Paths is a sample whenever Total exceeds its length.
	Total     int `json:"total"`
	Untracked int `json:"untracked"`
	// Capped is true when more paths changed than were collected.
	Capped bool `json:"capped"`
}

// VerifyRigor is the VerifyPaths result, bounded for storage.
type VerifyRigor struct {
	Verdict      string   `json:"verdict"`
	Source       string   `json:"source"`
	Examined     int      `json:"examined"`
	Findings     []string `json:"findings"`
	Degraded     []string `json:"degraded"`
	Omitted      []string `json:"omitted"`
	OmittedTotal int      `json:"omitted_total"`
}

// VerifyTests is what DevMap said about the tests that reach the change.
type VerifyTests struct {
	Candidates []string `json:"candidates"`
	Total      int      `json:"total"`
	// Truncated and WalkIncomplete are DevMap's own completeness signals. Either
	// one makes Candidates a lower bound and degrades the verdict.
	Truncated      bool   `json:"truncated"`
	WalkIncomplete string `json:"walk_incomplete"`
	IndexFresh     bool   `json:"index_fresh"`
	Generation     int    `json:"generation"`
	Queried        bool   `json:"queried"`
}

// VerifyCriterion is one acceptance criterion's finding.
type VerifyCriterion struct {
	Index    int      `json:"index"`
	Text     string   `json:"text"`
	Status   string   `json:"status"`
	Findings []string `json:"findings"`
}

// VerifyRecord is one verification, claimed and then settled.
type VerifyRecord struct {
	RequestID string `json:"request_id"`
	ItemID    string `json:"item_id"`
	RunID     string `json:"run_id"`
	State     string `json:"state"`
	// Outcome is empty while State is running.
	Outcome    string            `json:"outcome"`
	Worktree   string            `json:"worktree"`
	Base       string            `json:"base"`
	Head       string            `json:"head"`
	StartedAt  string            `json:"started_at"`
	FinishedAt string            `json:"finished_at"`
	Changes    VerifyChanges     `json:"changes"`
	Rigor      *VerifyRigor      `json:"rigor"`
	Tests      *VerifyTests      `json:"tests"`
	Criteria   []VerifyCriterion `json:"criteria"`
	// Degraded names every check that was owed and did not run or could not be
	// trusted. A passed record has none.
	Degraded       []string `json:"degraded"`
	CriticSummary  string   `json:"critic_summary"`
	CriticReadOnly bool     `json:"critic_read_only"`
}

// VerifyResult is what both ops answer.
type VerifyResult struct {
	Record VerifyRecord `json:"record"`
	// Replayed is true when the request was already known and nothing new was
	// started.
	Replayed bool `json:"replayed"`
}

func (p VerifyTaskParams) validate() error {
	for name, value := range map[string]string{
		"request_id": p.RequestID, "item_id": p.ItemID, "worktree": p.Worktree, "base": p.Base, "head": p.Head,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
		if len(value) > 4096 || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\n\r") {
			return fmt.Errorf("%s is not a usable string", name)
		}
	}
	if len(p.RequestID) > 128 || len(p.ItemID) > 128 {
		return errors.New("request_id and item_id are at most 128 bytes")
	}
	if !filepath.IsAbs(p.Worktree) || filepath.Clean(p.Worktree) != p.Worktree {
		return errors.New("worktree must be a clean absolute path")
	}
	for _, rev := range []string{p.Base, p.Head} {
		if strings.HasPrefix(rev, "-") {
			return errors.New("base and head must be revisions, not options")
		}
	}
	return nil
}

func decodeStrict(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing data after the request object")
	}
	return nil
}

// Start validates and durably claims one verification, then runs it on a job
// context that outlives the request. A request_id already known replays its
// record; nothing is run twice.
func (r *VerifyRunner) Start(ctx context.Context, raw json.RawMessage) (VerifyResult, error) {
	var params VerifyTaskParams
	if err := decodeStrict(raw, &params); err != nil {
		return VerifyResult{}, &store.WorkbenchError{Code: "invalid_input", Message: err.Error()}
	}
	if err := params.validate(); err != nil {
		return VerifyResult{}, &store.WorkbenchError{Code: "invalid_input", Message: err.Error()}
	}
	// The worktree is the only root this job reads. It must exist and be the
	// top of a git work tree; nothing falls back to the process's directory.
	if _, e := canonicalRepoRoot(ctx, "worktree", params.Worktree); e != nil {
		return VerifyResult{}, &store.WorkbenchError{Code: e.Code, Message: e.Message}
	}

	// One claim at a time. The check-then-write below is not atomic in storage,
	// so two identical requests must not both find nothing and both claim.
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return VerifyResult{}, &store.WorkbenchError{Code: "busy", Message: "the verification worker is closed"}
	}
	if known, err := r.lookupLocked(ctx, params.ItemID, params.RequestID); err != nil {
		return VerifyResult{}, err
	} else if known != nil {
		if known.Worktree != params.Worktree || known.Base != params.Base || known.Head != params.Head {
			return VerifyResult{}, &store.WorkbenchError{Code: "request_conflict",
				Message: "request_id was already used for a different worktree or revision range"}
		}
		return VerifyResult{Record: *known, Replayed: true}, nil
	}
	if len(r.jobs) >= verifyMaxActive {
		return VerifyResult{}, &store.WorkbenchError{Code: "busy", Message: "too many verifications are running"}
	}

	item, err := r.item(ctx, params.ItemID)
	if err != nil {
		return VerifyResult{}, err
	}
	scope, err := resolveVerifyScope(ctx, params)
	if err != nil {
		return VerifyResult{}, &store.WorkbenchError{Code: "invalid_input", Message: err.Error()}
	}

	now := r.deps.Now().UTC().Format(time.RFC3339)
	record := VerifyRecord{
		RequestID: params.RequestID, ItemID: params.ItemID, RunID: "verify-" + params.ItemID + "-" + params.RequestID,
		State: verifyRunning, Worktree: params.Worktree, Base: params.Base, Head: params.Head, StartedAt: now,
		Changes: VerifyChanges{Paths: []string{}}, Degraded: []string{}, Criteria: []VerifyCriterion{},
		CriticReadOnly: true,
	}
	if err := r.persist(ctx, record); err != nil {
		return VerifyResult{}, err
	}

	jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.deps.Timeout)
	job := &verifyJob{cancel: cancel, done: make(chan struct{})}
	r.jobs[params.RequestID] = job
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer close(job.done)
		defer cancel()
		settled := r.executeSafely(jobCtx, record, item, scope)
		// Settled on a context of its own: the job context is exactly what ended
		// when the deadline fired or the host shut down, and the verdict that
		// says so still has to be recorded.
		settleCtx, settleCancel := context.WithTimeout(context.WithoutCancel(jobCtx), 10*time.Second)
		defer settleCancel()
		if err := r.persist(settleCtx, settled); err != nil {
			// The running claim stays in storage. It is unresolved work, never an
			// implicit pass; the next read settles it as degraded.
			log.Printf("verification %s settlement unresolved: %v", settled.RequestID, err)
		}
		r.mu.Lock()
		delete(r.jobs, params.RequestID)
		r.mu.Unlock()
	}()
	return VerifyResult{Record: record}, nil
}

// executeSafely turns a panic in a check into a degraded record. A job runs on
// its own goroutine, where an escaping panic ends the whole host and every
// managed session it carries, and the verdict for a task whose verification
// crashed is "could not verify", not silence.
func (r *VerifyRunner) executeSafely(ctx context.Context, record VerifyRecord, item verifyItem, scope verifyScope) (settled VerifyRecord) {
	defer func() {
		if panicked := recover(); panicked != nil {
			settled = record
			settled.State, settled.Outcome = verifySettled, VerifyDegraded
			settled.FinishedAt = r.deps.Now().UTC().Format(time.RFC3339)
			settled.Degraded = append(settled.Degraded, fmt.Sprintf("verification crashed (%v), so nothing was concluded", panicked))
		}
	}()
	return r.execute(ctx, record, item, scope)
}

// Get returns the newest record for a request, or for an item when no
// request_id is given.
func (r *VerifyRunner) Get(ctx context.Context, raw json.RawMessage) (VerifyResult, error) {
	var params VerifyGetParams
	if err := decodeStrict(raw, &params); err != nil {
		return VerifyResult{}, &store.WorkbenchError{Code: "invalid_input", Message: err.Error()}
	}
	if strings.TrimSpace(params.ItemID) == "" || len(params.ItemID) > 128 || len(params.RequestID) > 128 {
		return VerifyResult{}, &store.WorkbenchError{Code: "invalid_input", Message: "item_id is required"}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, err := r.lookupLocked(ctx, params.ItemID, params.RequestID)
	if err != nil {
		return VerifyResult{}, err
	}
	if record == nil {
		return VerifyResult{}, &store.WorkbenchError{Code: "not_found", Message: "no verification record for that item and request"}
	}
	return VerifyResult{Record: *record, Replayed: true}, nil
}

// lookupLocked finds the newest stored record for an item, optionally for one
// request. A running record with no live job here belongs to a process that
// ended before it settled; it is settled as degraded rather than reported as
// running forever, and the critic is not run again.
func (r *VerifyRunner) lookupLocked(ctx context.Context, itemID, requestID string) (*VerifyRecord, error) {
	rows, truncated, err := r.deps.Client.EvidenceList(ctx, itemID)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.Type != verifyEvidenceType {
			continue
		}
		var record VerifyRecord
		if err := json.Unmarshal(row.DataJSON, &record); err != nil {
			continue
		}
		if record.RequestID == "" || (requestID != "" && record.RequestID != requestID) {
			continue
		}
		if record.State == verifyRunning && r.jobs[record.RequestID] == nil {
			record.State, record.Outcome = verifySettled, VerifyDegraded
			record.FinishedAt = r.deps.Now().UTC().Format(time.RFC3339)
			record.Degraded = append(record.Degraded,
				"the process that was verifying this task ended before it recorded a verdict; nothing was concluded, start a new verification with a new request_id")
			if err := r.persist(ctx, record); err != nil {
				return nil, err
			}
		}
		return &record, nil
	}
	if truncated {
		// Absence cannot be established from a capped listing, and acting on it
		// could re-run a verification that already has a record.
		return nil, &store.WorkbenchError{Code: "unavailable",
			Message: "the item's evidence history is too long to establish whether this request was already verified"}
	}
	return nil, nil
}

// persist records one state of a verification: the run row (an upsert) and then
// the evidence row carrying the record. The evidence row is what a request is
// matched against, so it is written last — a failure between the two leaves a
// run row nothing reads, never a claim that cannot be found.
func (r *VerifyRunner) persist(ctx context.Context, record VerifyRecord) error {
	data, err := encodeVerifyRecord(record)
	if err != nil {
		return err
	}
	status := record.Outcome
	if record.State == verifyRunning {
		status = verifyRunning
	}
	started := record.StartedAt
	run := store.VerificationRun{
		ID: record.RunID, TaskID: record.ItemID, Sandbox: verifyRunSandbox, Status: status,
		StartedAt: started, FinishedAt: record.FinishedAt,
		CommandsJSON: "[]", EnvironmentJSON: "{}",
	}
	if err := r.deps.Client.RunRecord(ctx, run); err != nil {
		return err
	}
	_, err = r.deps.Client.EvidenceAppend(ctx, verifyEvidenceType, record.ItemID, "", "", data)
	return err
}

// encodeVerifyRecord marshals a record under maxVerifyRecordBytes, shedding
// the bulkiest detail first and saying so. A record too large to store would
// otherwise fail settlement and leave the verification unresolved.
func encodeVerifyRecord(record VerifyRecord) (json.RawMessage, error) {
	for _, step := range []func(*VerifyRecord){
		func(*VerifyRecord) {},
		func(v *VerifyRecord) { v.CriticSummary = devcouncil.TruncateRunes(v.CriticSummary, 1000) },
		func(v *VerifyRecord) {
			for i := range v.Criteria {
				if len(v.Criteria[i].Findings) > 2 {
					v.Criteria[i].Findings = v.Criteria[i].Findings[:2]
				}
			}
		},
		func(v *VerifyRecord) {
			if v.Tests != nil && len(v.Tests.Candidates) > 20 {
				v.Tests.Candidates = v.Tests.Candidates[:20]
			}
			if len(v.Changes.Paths) > 32 {
				v.Changes.Paths = v.Changes.Paths[:32]
			}
			if v.Rigor != nil && len(v.Rigor.Findings) > 5 {
				v.Rigor.Findings = v.Rigor.Findings[:5]
			}
			v.Degraded = append(v.Degraded, "this record was shortened to fit storage; counts are exact, lists are samples")
		},
	} {
		step(&record)
		data, err := json.Marshal(record)
		if err != nil {
			return nil, err
		}
		if len(data) <= maxVerifyRecordBytes {
			return data, nil
		}
	}
	return nil, errors.New("verification record exceeds the storage bound even when shortened")
}

// item reads the board item whose criteria are to be judged. The criteria come
// from storage, never from the request.
func (r *VerifyRunner) item(ctx context.Context, id string) (verifyItem, error) {
	input, err := json.Marshal(struct {
		ID string `json:"id"`
	}{id})
	if err != nil {
		return verifyItem{}, err
	}
	raw, err := r.deps.Client.Workbench(ctx, "items.get", input)
	if err != nil {
		return verifyItem{}, err
	}
	var result struct {
		Item struct {
			ID                 string   `json:"id"`
			Title              string   `json:"title"`
			AcceptanceCriteria []string `json:"acceptance_criteria"`
		} `json:"item"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return verifyItem{}, err
	}
	if result.Item.ID != id {
		return verifyItem{}, &store.WorkbenchError{Code: "not_found", Message: "no such board item"}
	}
	item := verifyItem{Title: result.Item.Title}
	for _, text := range result.Item.AcceptanceCriteria {
		if len(item.Criteria) == maxVerifyCriteria {
			item.CriteriaCapped = true
			break
		}
		item.Criteria = append(item.Criteria, devcouncil.TruncateRunes(strings.TrimSpace(text), maxVerifyCriterionRunes))
	}
	return item, nil
}

type verifyItem struct {
	Title          string
	Criteria       []string
	CriteriaCapped bool
}

// Close cancels running jobs and waits, bounded, for them to settle. A job
// that ignores cancellation stays a running claim, which a later read settles
// as degraded.
func (r *VerifyRunner) Close() error {
	r.mu.Lock()
	r.closed = true
	jobs := make([]*verifyJob, 0, len(r.jobs))
	for _, job := range r.jobs {
		jobs = append(jobs, job)
	}
	r.mu.Unlock()
	for _, job := range jobs {
		job.cancel()
	}
	finished := make(chan struct{})
	go func() { r.wg.Wait(); close(finished) }()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case <-finished:
		return nil
	case <-timer.C:
		return errors.New("verification jobs did not settle before shutdown")
	}
}
