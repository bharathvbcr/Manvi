package serve

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/dc/store"
)

// AutomaticStatus reports this host's coordinator, not proof that another host
// or provider has stopped. Durable proposal ownership remains in the store.
type AutomaticStatus struct {
	OK          bool   `json:"ok"`
	State       string `json:"state"`
	Reason      string `json:"reason"`
	TaskID      string `json:"task_id"`
	ProposalID  string `json:"proposal_id"`
	NextCheckAt int64  `json:"next_check_at"`
}

type automaticWorker struct {
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	wake          chan struct{}
	configuration EnhancementConfiguration
	status        AutomaticStatus
}

type automaticStep struct {
	status  AutomaticStatus
	delay   time.Duration
	jobDone <-chan struct{}
}

type automaticSettings struct {
	ID       string  `json:"id"`
	Revision int64   `json:"revision"`
	Enabled  *bool   `json:"enabled"`
	Provider *string `json:"provider"`
	Model    *string `json:"model"`
}

type automaticQueuedTask struct {
	ID          string `json:"id"`
	Revision    int64  `json:"revision"`
	NotBeforeMS int64  `json:"not_before_ms"`
}

func automaticInput(raw json.RawMessage) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	_, err := enhancementObject(raw, 1024)
	return err
}

// AutomaticStatus never starts the coordinator, storage, or a provider.
func (r *EnhancementRunner) AutomaticStatus() AutomaticStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.automatic == nil {
		return AutomaticStatus{OK: true, State: "not_started"}
	}
	return r.automatic.status
}

// Wake coalesces requests into one host coordinator. Empty queues use a blocked
// channel, not an idle timer. Repeated wake-ups cannot repeat a provider claim.
func (r *EnhancementRunner) Wake(ctx context.Context, raw json.RawMessage, configuration EnhancementConfiguration) (AutomaticStatus, error) {
	if err := automaticInput(raw); err != nil {
		return AutomaticStatus{}, err
	}
	if err := ctx.Err(); err != nil {
		return AutomaticStatus{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return AutomaticStatus{}, &store.WorkbenchError{Code: "closed", Message: "the enhancement worker is closed"}
	}
	if r.automatic == nil {
		workerCtx, cancel := context.WithCancel(context.Background())
		r.automatic = &automaticWorker{ctx: workerCtx, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1)}
		go r.runAutomatic(r.automatic)
	}
	worker := r.automatic
	worker.configuration = configuration
	worker.configuration.Providers = nil
	worker.status = AutomaticStatus{OK: true, State: "checking"}
	select {
	case worker.wake <- struct{}{}:
	default:
	}
	return worker.status, nil
}

func (r *EnhancementRunner) runAutomatic(worker *automaticWorker) {
	defer close(worker.done)
	// Consume the initial signal before inspecting the queue. Otherwise it would
	// survive the first pass and immediately repeat a paused or uncertain check.
	select {
	case <-worker.ctx.Done():
	case <-worker.wake:
	}
	for {
		if worker.ctx.Err() != nil {
			r.mu.Lock()
			worker.status = AutomaticStatus{OK: true, State: "stopped"}
			r.mu.Unlock()
			return
		}
		r.mu.Lock()
		configuration := worker.configuration
		r.mu.Unlock()
		ctx, cancel := context.WithTimeout(worker.ctx, 5*time.Second)
		step := r.stepAutomatic(ctx, configuration)
		cancel()
		step.status.OK = true
		var timer *time.Timer
		var deadline <-chan time.Time
		if step.delay > 0 {
			step.status.NextCheckAt = time.Now().Add(step.delay).UnixMilli()
			timer = time.NewTimer(step.delay)
			deadline = timer.C
		}
		r.mu.Lock()
		worker.status = step.status
		r.mu.Unlock()
		select {
		case <-worker.ctx.Done():
		case <-worker.wake:
		case <-step.jobDone:
		case <-deadline:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (r *EnhancementRunner) automaticPaused(err error) automaticStep {
	return automaticStep{status: AutomaticStatus{State: "paused", Reason: r.errorText(err)}}
}

func (r *EnhancementRunner) stepAutomatic(ctx context.Context, configuration EnhancementConfiguration) automaticStep {
	raw, err := r.client.Workbench(ctx, "automation.get", json.RawMessage(`{"id":"profile"}`))
	if err != nil {
		return r.automaticPaused(err)
	}
	var settings struct {
		Item automaticSettings `json:"item"`
	}
	if err = json.Unmarshal(raw, &settings); err != nil {
		return r.automaticPaused(err)
	}
	config := settings.Item
	if config.ID != "profile" || config.Revision < 1 || config.Enabled == nil || (config.Provider == nil) != (config.Model == nil) {
		return r.automaticPaused(errors.New("automation settings are incomplete"))
	}
	r.mu.Lock()
	active := r.active
	r.mu.Unlock()
	if !*config.Enabled {
		if active != nil && active.automatic {
			return automaticStep{status: AutomaticStatus{State: "stopping", TaskID: active.taskID, ProposalID: active.id}, jobDone: active.done}
		}
		return automaticStep{status: AutomaticStatus{State: "disabled"}}
	}
	if active != nil {
		state := "waiting"
		if active.automatic {
			state = "generating"
		}
		return automaticStep{status: AutomaticStatus{State: state, TaskID: active.taskID, ProposalID: active.id}, jobDone: active.done}
	}
	// Find prepared work even when the queue was consumed before a crash. The
	// newest active record also reveals claims held by other host processes.
	raw, err = r.client.Workbench(ctx, "enhancements.list", json.RawMessage(`{"newest":true,"states":["pending","running","cancel_requested"],"limit":1}`))
	if err != nil {
		return r.automaticPaused(err)
	}
	var proposals struct {
		Items []enhancementRecord `json:"items"`
	}
	if err = json.Unmarshal(raw, &proposals); err != nil {
		return r.automaticPaused(err)
	}
	if len(proposals.Items) > 1 {
		return r.automaticPaused(errors.New("active proposal lookup exceeded its requested bound"))
	}
	if len(proposals.Items) == 1 {
		proposal := proposals.Items[0]
		if proposal.ID == "" || proposal.TaskID == "" || proposal.Revision < 1 || proposal.ExpiresAt <= 0 {
			return r.automaticPaused(errors.New("active proposal metadata is incomplete"))
		}
		if proposal.State == "running" || proposal.State == "cancel_requested" {
			state, delay, reason := "waiting", 5*time.Second, "An existing generation owns the profile slot."
			if proposal.ExpiresAt <= time.Now().Unix() {
				state, delay, reason = "paused", 0, "The previous provider outcome is uncertain. Review recovery for this proposal."
			}
			return automaticStep{status: AutomaticStatus{State: state, Reason: reason, TaskID: proposal.TaskID, ProposalID: proposal.ID}, delay: delay}
		}
		if proposal.State != "pending" {
			return r.automaticPaused(errors.New("active lookup returned a non-active proposal"))
		}
		if proposal.ExpiresAt > time.Now().Unix() {
			if proposal.Automatic {
				return r.startAutomatic(ctx, proposal)
			}
			return automaticStep{status: AutomaticStatus{State: "waiting", Reason: "A manually prepared suggestion is waiting to start.", TaskID: proposal.TaskID, ProposalID: proposal.ID}, delay: 5 * time.Second}
		}
	}
	raw, err = r.client.Workbench(ctx, "automation.list", json.RawMessage(`{"limit":1}`))
	if err != nil {
		return r.automaticPaused(err)
	}
	var queue struct {
		Items []automaticQueuedTask `json:"items"`
	}
	if err = json.Unmarshal(raw, &queue); err != nil {
		return r.automaticPaused(err)
	}
	if len(queue.Items) == 0 {
		return automaticStep{status: AutomaticStatus{State: "idle"}}
	}
	if len(queue.Items) > 1 {
		return r.automaticPaused(errors.New("queue lookup exceeded its requested bound"))
	}
	task := queue.Items[0]
	if task.ID == "" || task.Revision < 1 || task.NotBeforeMS <= 0 {
		return r.automaticPaused(errors.New("queued task metadata is incomplete"))
	}
	if config.Provider != nil {
		configuration.Provider, configuration.Model = *config.Provider, *config.Model
	}
	if strings.TrimSpace(configuration.Provider) == "" || strings.TrimSpace(configuration.Model) == "" {
		return r.automaticPaused(errors.New("Choose an explicit provider and model for automatic enhancements."))
	}
	if wait := time.Until(time.UnixMilli(task.NotBeforeMS)); wait > 0 {
		if wait > time.Minute {
			return r.automaticPaused(errors.New("Queued work has an unexpected future deadline. Check the system clock before resuming."))
		}
		return automaticStep{status: AutomaticStatus{State: "waiting", Reason: "Waiting for the text-save debounce.", TaskID: task.ID}, delay: wait}
	}
	id, err := generationID()
	if err != nil {
		return r.automaticPaused(err)
	}
	input, err := json.Marshal(struct {
		generationRequest
		TaskID           string `json:"task_id"`
		TaskRevision     int64  `json:"expected_task_revision"`
		SettingsRevision int64  `json:"expected_settings_revision"`
		Provider         string `json:"provider"`
		Model            string `json:"model"`
	}{generationRequest{id, "prepare-" + id, 0}, task.ID, task.Revision, config.Revision, configuration.Provider, configuration.Model})
	if err != nil {
		return r.automaticPaused(err)
	}
	raw, err = r.client.Workbench(ctx, "automation.prepare", input)
	if err != nil {
		var refusal *store.WorkbenchError
		if errors.As(err, &refusal) {
			switch refusal.Code {
			case "busy", "revision_conflict", "not_ready", "not_found", "automation_disabled":
				return automaticStep{status: AutomaticStatus{State: "waiting", Reason: refusal.Message, TaskID: task.ID}, delay: time.Second}
			case "enhancement_limit":
				return automaticStep{status: AutomaticStatus{State: "waiting", Reason: refusal.Message, TaskID: task.ID}, delay: time.Minute}
			}
		}
		// An uncertain preparation may have committed. A later explicit wake
		// reconciles by reading durable proposals before preparing anything else.
		return r.automaticPaused(err)
	}
	var prepared struct {
		Item enhancementRecord `json:"item"`
	}
	if err = json.Unmarshal(raw, &prepared); err != nil {
		return r.automaticPaused(err)
	}
	proposal := prepared.Item
	if proposal.ID != id || proposal.TaskID != task.ID || proposal.Revision != 1 || proposal.State != "pending" || !proposal.Automatic || proposal.Provider != configuration.Provider || proposal.Model != configuration.Model {
		return r.automaticPaused(errors.New("prepared proposal does not match its reservation"))
	}
	return r.startAutomatic(ctx, proposal)
}

func (r *EnhancementRunner) startAutomatic(ctx context.Context, proposal enhancementRecord) automaticStep {
	receipt, err := generationID()
	if err != nil {
		return r.automaticPaused(err)
	}
	input, err := json.Marshal(generationRequest{proposal.ID, receipt, proposal.Revision})
	if err != nil {
		return r.automaticPaused(err)
	}
	_, err = r.Generate(ctx, input)
	if err != nil {
		var refusal *store.WorkbenchError
		if errors.As(err, &refusal) {
			switch refusal.Code {
			case "busy":
				return automaticStep{status: AutomaticStatus{State: "waiting", Reason: refusal.Message}, delay: time.Second}
			case "revision_conflict", "field_locked", "automation_disabled", "expired", "invalid_transition", "not_found":
				if cleanup := r.failAutomaticPending(ctx, proposal.ID, err); cleanup != nil {
					return r.automaticPaused(cleanup)
				}
				return automaticStep{status: AutomaticStatus{State: "waiting", Reason: refusal.Message}, delay: 250 * time.Millisecond}
			}
		}
		// A lost claim reply cannot prove that inference is safe to start again.
		return r.automaticPaused(err)
	}
	r.mu.Lock()
	job := r.active
	r.mu.Unlock()
	if job != nil && job.id == proposal.ID {
		return automaticStep{status: AutomaticStatus{State: "generating", TaskID: proposal.TaskID, ProposalID: proposal.ID}, jobDone: job.done}
	}
	return automaticStep{status: AutomaticStatus{State: "checking"}, delay: 250 * time.Millisecond}
}

func (r *EnhancementRunner) failAutomaticPending(ctx context.Context, id string, cause error) error {
	proposal, _, err := r.record(ctx, id)
	if err != nil {
		return err
	}
	if proposal.State != "pending" || !proposal.Automatic {
		return nil
	}
	receipt, err := generationID()
	if err != nil {
		return err
	}
	input, err := json.Marshal(struct {
		generationRequest
		Failure string `json:"failure"`
	}{generationRequest{id, receipt, proposal.Revision}, r.errorText(cause)})
	if err != nil {
		return err
	}
	_, err = r.client.Workbench(ctx, "enhancements.complete", input)
	return err
}
