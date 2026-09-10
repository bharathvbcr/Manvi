package serve

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bharathvbcr/Manvi/manvi/dc/store"
	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// enhancementJobTimeout bounds one provider call. Keep it below the durable
// create/claim lease in dc-store so settle can still complete after inference.
const enhancementJobTimeout = 150 * time.Second

// EnhancementProvider resolves the explicitly selected provider/model using the
// host's configured adapters. It must never silently substitute another backend.
type EnhancementProvider func(context.Context, string, string) (llm.Provider, error)

// EnhancementRunner owns at most one inference call. Storage claims exclude
// other host processes; an unknown outcome is never an automatic retry.
type EnhancementRunner struct {
	client      *store.Client
	provider    EnhancementProvider
	failureText func(error) string
	mu          sync.Mutex
	active      *enhancementJob
	automatic   *automaticWorker
	closed      bool
}

type enhancementJob struct {
	id        string
	owner     string
	cancel    context.CancelFunc
	done      chan struct{}
	taskID    string
	automatic bool
}

type enhancementRecord struct {
	ID        string          `json:"id"`
	Revision  int64           `json:"revision"`
	State     string          `json:"state"`
	WorkerID  string          `json:"worker_id"`
	Provider  string          `json:"provider"`
	Model     string          `json:"model"`
	Fields    []string        `json:"fields"`
	Source    json.RawMessage `json:"source"`
	TaskID    string          `json:"task_id"`
	Automatic bool            `json:"automatic"`
	ExpiresAt int64           `json:"expires_at"`
}

type generationRequest struct {
	ID               string `json:"id"`
	RequestID        string `json:"request_id"`
	ExpectedRevision int64  `json:"expected_revision"`
}

type generationClaim struct {
	generationRequest
	WorkerID string `json:"worker_id"`
}

type generationCompletion struct {
	generationClaim
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
	Rationale   *string `json:"rationale,omitempty"`
	Failure     *string `json:"failure,omitempty"`
}

func NewEnhancementRunner(client *store.Client, provider EnhancementProvider, failureText func(error) string) (*EnhancementRunner, error) {
	if client == nil || provider == nil || failureText == nil {
		return nil, errors.New("enhancement runner requires storage, a provider resolver and redacted error formatting")
	}
	return &EnhancementRunner{client: client, provider: provider, failureText: failureText}, nil
}

func generationID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

func (r *EnhancementRunner) record(ctx context.Context, id string) (enhancementRecord, json.RawMessage, error) {
	input, err := json.Marshal(struct {
		ID string `json:"id"`
	}{id})
	if err != nil {
		return enhancementRecord{}, nil, err
	}
	raw, err := r.client.Workbench(ctx, "enhancements.get", input)
	if err != nil {
		return enhancementRecord{}, nil, err
	}
	var result struct {
		Item enhancementRecord `json:"item"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return enhancementRecord{}, nil, err
	}
	if result.Item.ID != id || result.Item.Revision < 1 || result.Item.State == "" {
		return enhancementRecord{}, nil, errors.New("invalid enhancement record")
	}
	return result.Item, raw, nil
}

// Generate returns after claiming the attempt. Inference never occupies the
// serial host dispatcher, so questions, cancellation and board reads can proceed.
func (r *EnhancementRunner) Generate(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if _, err := enhancementObject(raw, 4096, "id", "request_id", "expected_revision"); err != nil {
		return nil, err
	}
	var request generationRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}
	if request.ID == "" || request.RequestID == "" || request.ExpectedRevision < 1 {
		return nil, errors.New("generation requires id, request_id and expected_revision")
	}
	record, observed, err := r.record(ctx, request.ID)
	if err != nil {
		return nil, err
	}
	// Repeating a start after a lost reply observes existing work. It cannot
	// replay the provider call, including after another host took the claim.
	if record.State != "pending" {
		return observed, nil
	}
	owner, err := generationID()
	if err != nil {
		return nil, err
	}
	jobCtx, cancel := context.WithTimeout(context.Background(), enhancementJobTimeout)
	job := &enhancementJob{id: request.ID, owner: owner, cancel: cancel, done: make(chan struct{}), taskID: record.TaskID, automatic: record.Automatic}
	r.mu.Lock()
	if r.closed || r.active != nil {
		r.mu.Unlock()
		cancel()
		return nil, &store.WorkbenchError{Code: "busy", Message: "the enhancement worker is busy or closed"}
	}
	r.active = job
	r.mu.Unlock()
	claim, err := json.Marshal(generationClaim{request, owner})
	if err != nil {
		r.finish(job)
		return nil, err
	}
	// Cancellation before claim acknowledgement must not launch an unwanted
	// inference. Once acknowledged, the durable job outlives the request.
	claimCtx, cancelClaim := context.WithCancel(jobCtx)
	stopRequestCancellation := context.AfterFunc(ctx, cancelClaim)
	claimed, err := r.client.Workbench(claimCtx, "enhancements.claim", claim)
	stopRequestCancellation()
	cancelClaim()
	if err != nil {
		// The claim may have committed. Leave that uncertainty in storage; do
		// not start inference or guess that a second claim is safe.
		r.finish(job)
		return nil, err
	}
	if ctx.Err() != nil {
		job.cancel()
	}
	go r.run(jobCtx, job, record)
	return claimed, nil
}

func (r *EnhancementRunner) finish(job *enhancementJob) {
	job.cancel()
	r.mu.Lock()
	if r.active == job {
		r.active = nil
	}
	close(job.done)
	r.mu.Unlock()
}

func (r *EnhancementRunner) watch(ctx context.Context, job *enhancementJob, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			record, _, err := r.record(checkCtx, job.id)
			cancel()
			if err != nil || record.WorkerID != job.owner || record.State != "running" {
				job.cancel()
				return
			}
		}
	}
}

func (r *EnhancementRunner) run(ctx context.Context, job *enhancementJob, record enhancementRecord) {
	defer r.finish(job)
	watchCtx, stopWatch := context.WithCancel(ctx)
	watchDone := make(chan struct{})
	go r.watch(watchCtx, job, watchDone)
	proposal, inferenceErr := r.infer(ctx, record)
	if inferenceErr == nil && ctx.Err() != nil {
		inferenceErr = ctx.Err()
	}
	stopWatch()
	<-watchDone
	if err := r.settle(job, proposal, inferenceErr); err != nil {
		// Failure to record a result leaves the durable running claim intact.
		// It is unresolved work, never an implicit success or retry.
		log.Printf("enhancement %s settlement unresolved: %s", job.id, r.errorText(err))
	}
}

// enhancementSettleFailure maps provider stop reasons into durable failure text.
// DeadlineExceeded is the job budget; Canceled is host/watch stop; cancel_requested
// is an explicit user dismiss. Collapsing those into one string made timeouts look
// like user cancels in probe and UI diagnostics.
func enhancementSettleFailure(state string, inferenceErr error) error {
	if state == "cancel_requested" {
		return errors.New("generation cancelled by the user")
	}
	if inferenceErr == nil {
		return nil
	}
	if errors.Is(inferenceErr, context.DeadlineExceeded) {
		return errors.New("generation timed out")
	}
	if errors.Is(inferenceErr, context.Canceled) {
		return errors.New("generation cancelled")
	}
	return inferenceErr
}

func (r *EnhancementRunner) errorText(err error) string {
	message := strings.TrimSpace(strings.ToValidUTF8(strings.ReplaceAll(r.failureText(err), "\x00", ""), ""))
	if message == "" {
		message = "enhancement failed; no printable diagnostic was available"
	}
	if len(message) > 1800 {
		message = message[:1800]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return message
}

func (r *EnhancementRunner) settle(job *enhancementJob, proposal enhancementProposal, inferenceErr error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		record, _, err := r.record(ctx, job.id)
		if err != nil {
			return err
		}
		if record.WorkerID != job.owner || (record.State != "running" && record.State != "cancel_requested") {
			return nil
		}
		id, err := generationID()
		if err != nil {
			return err
		}
		completion := generationCompletion{generationClaim: generationClaim{generationRequest{job.id, id, record.Revision}, job.owner}}
		inferenceErr = enhancementSettleFailure(record.State, inferenceErr)
		if inferenceErr != nil {
			message := r.errorText(inferenceErr)
			completion.Failure = &message
		} else {
			completion.Title, completion.Description, completion.Rationale = proposal.Title, proposal.Description, proposal.Rationale
		}
		input, err := json.Marshal(completion)
		if err != nil {
			return err
		}
		_, err = r.client.Workbench(ctx, "enhancements.complete", input)
		var refused *store.WorkbenchError
		if !errors.As(err, &refused) || refused.Code != "revision_conflict" {
			return err
		}
		// Only a definite, uncommitted revision refusal permits rebuilding a
		// request. Transport uncertainty never regenerates the receipt ID.
	}
	return errors.New("enhancement changed repeatedly while its result was being recorded")
}

// Close cancels the worker and waits for actual return, bounded to five seconds.
// A provider that ignores cancellation remains an unresolved durable claim.
func (r *EnhancementRunner) Close() error {
	r.mu.Lock()
	r.closed = true
	job := r.active
	automatic := r.automatic
	r.mu.Unlock()
	var jobDone, automaticDone <-chan struct{}
	if job != nil {
		job.cancel()
		jobDone = job.done
	}
	if automatic != nil {
		automatic.cancel()
		automaticDone = automatic.done
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for jobDone != nil || automaticDone != nil {
		select {
		case <-jobDone:
			jobDone = nil
		case <-automaticDone:
			automaticDone = nil
		case <-timer.C:
			if job != nil {
				return fmt.Errorf("enhancement %s did not stop before shutdown; provider outcome remains uncertain", job.id)
			}
			return errors.New("automatic enhancement coordinator did not stop before shutdown")
		}
	}
	return nil
}
