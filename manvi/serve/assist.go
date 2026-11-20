package serve

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
)

// OpWorkAssistComplete is one plain, tool-free model completion for the host's
// own AI features: explaining a commit, writing a commit message, suggesting a
// branch name. It is a host operation only: it is registered by WorkbenchModule
// beside OpWorkRefine, on the host's enhancement selection, and never offered to
// an agent. It reads and writes no board state and stores nothing.
//
// The host never holds the model's credential, so the call is made here, by the
// provider the selection names and no other. It does not share the enhancement
// runner's single generation slot: a task enhancement in flight never makes an
// explain-commit call answer busy.
const OpWorkAssistComplete = "work.assist.complete"

// ErrProviderAuth answers an assist call the model provider refused because it
// rejected the credential itself (Anthropic: HTTP 401, a wrong, revoked or
// disabled key). It is not retryable: the same key fails the same way until it
// is replaced. A host branches on the code, never on the message, which stays
// the runner's redacted provider text.
const ErrProviderAuth = "E_PROVIDER_AUTH"

// The per-call limits. A request over any of them is refused whole, never
// truncated, so the host never sees an answer to a prompt it did not send.
const (
	maxAssistSystemBytes = 32 << 10
	maxAssistUserBytes   = 2 << 20
	maxAssistOutput      = 16384
	// maxAssistInFlight bounds concurrent model calls across every connection
	// sharing the runner. A further call is answered E_BUSY at once.
	maxAssistInFlight = 2
	// assistLaneWidth is how many assist requests the lane runs at once: one more
	// than the bound, so a request over the bound reaches the handler and is
	// refused immediately instead of waiting in the lane's queue for a slot.
	assistLaneWidth = maxAssistInFlight + 1
)

// AssistParams is the request. System and User may be absent only in a dry run.
type AssistParams struct {
	System string `json:"system,omitempty"`
	User   string `json:"user,omitempty"`
	// MaxOutputTokens is the output bound asked for, 1 to 16384, lowered to the
	// model's own cap; the lower number is what AssistResult reports.
	MaxOutputTokens int `json:"max_output_tokens"`
	// DryRun makes no model call and returns the model's capability only.
	DryRun bool `json:"dry_run,omitempty"`
}

// AssistResult is the completion. Reasoning is the model's own, kept apart and
// never part of Text. In a dry run Text, Reasoning and StopReason are empty and
// the counts are zero.
type AssistResult struct {
	OK       bool   `json:"ok"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// ContextWindow and MaxOutputTokens come from the provider's static
	// capability for the model; no network is involved.
	ContextWindow   int    `json:"context_window"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	Text            string `json:"text"`
	Reasoning       string `json:"reasoning"`
	StopReason      string `json:"stop_reason"`
	// Truncated is true when the model stopped at the output cap.
	Truncated    bool `json:"truncated"`
	InputTokens  int  `json:"input_tokens"`
	OutputTokens int  `json:"output_tokens"`
	// Redactions counts secrets removed from the prompt before it was sent. The
	// enhancement path has no prompt-redaction owner, so it is always zero and
	// the prompt is sent as the host wrote it.
	Redactions int `json:"redactions"`
}

// decodeAssistParams reads and validates the request. It is stricter than the
// decoder every other op uses: an unknown or case-variant member, a duplicate
// member, invalid UTF-8 (a raw byte or a lone surrogate escape) and trailing
// content are all refused, because a prompt that was silently repaired is not
// the prompt the host sent.
func decodeAssistParams(raw []byte) (AssistParams, *Error) {
	var params AssistParams
	if len(raw) == 0 {
		return params, badRequest("assist params are required")
	}
	if err := json.Unmarshal(raw, &params, json.RejectUnknownMembers(true)); err != nil {
		return params, badRequest("assist params: %v", err)
	}
	switch {
	case len(params.System) > maxAssistSystemBytes:
		return params, badRequest("assist system prompt is %d bytes; the limit is %d", len(params.System), maxAssistSystemBytes)
	case len(params.User) > maxAssistUserBytes:
		return params, badRequest("assist user prompt is %d bytes; the limit is %d", len(params.User), maxAssistUserBytes)
	case !utf8.ValidString(params.System) || !utf8.ValidString(params.User):
		return params, badRequest("assist prompts must be valid UTF-8")
	case params.MaxOutputTokens < 1 || params.MaxOutputTokens > maxAssistOutput:
		return params, badRequest("assist max_output_tokens must be 1 to %d, not %d", maxAssistOutput, params.MaxOutputTokens)
	case !params.DryRun && (params.System == "" || params.User == ""):
		return params, badRequest("assist requires a non-empty system and user prompt outside a dry run")
	}
	return params, nil
}

// assistState is the runner's bookkeeping for assist calls. The zero value is
// ready. It is separate from the enhancement slot (EnhancementRunner.active) on
// purpose.
type assistState struct {
	mu       sync.Mutex
	next     int
	inflight map[int]context.CancelFunc
}

// assistFailure carries a provider's error as the runner's redacted text while
// keeping the cause for errors.Is, so a deadline still reads as retryable.
type assistFailure struct {
	text  string
	cause error
}

func (e *assistFailure) Error() string { return e.text }
func (e *assistFailure) Unwrap() error { return e.cause }

func (r *EnhancementRunner) assistError(err error) error {
	return &assistFailure{text: r.errorText(err), cause: err}
}

// begin admits one model call, or refuses it: the runner is closed, or
// maxAssistInFlight calls are already running. The closed check and the
// registration are one step, so Close cannot miss a call it should cancel.
func (s *assistState) begin(r *EnhancementRunner, cancel context.CancelFunc) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, &store.WorkbenchError{Code: "busy", Message: "the enhancement worker is busy or closed"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.inflight) >= maxAssistInFlight {
		return 0, &Error{Code: ErrBusy, Retryable: true,
			Message: fmt.Sprintf("%d assist calls are already in flight; retry when one finishes", maxAssistInFlight)}
	}
	if s.inflight == nil {
		s.inflight = map[int]context.CancelFunc{}
	}
	s.next++
	s.inflight[s.next] = cancel
	return s.next, nil
}

func (s *assistState) end(id int) {
	s.mu.Lock()
	delete(s.inflight, id)
	s.mu.Unlock()
}

// cancelAll stops every assist call in flight; Close calls it.
func (s *assistState) cancelAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cancel := range s.inflight {
		cancel()
	}
}

// Assist runs one completion on the host's explicit selection. It never
// substitutes another provider or model, and it makes no model call when the
// selection is missing, the model has no declared capability, or the request is
// a dry run. Provider failures come back as the runner's redacted text.
func (r *EnhancementRunner) Assist(ctx context.Context, params AssistParams, selection EnhancementConfiguration) (*AssistResult, error) {
	if !selection.OK || strings.TrimSpace(selection.Provider) == "" || strings.TrimSpace(selection.Model) == "" {
		return nil, errors.New("assist completion requires a selected enhancement provider and model; none is configured")
	}
	if params.DryRun {
		return r.assistDryRun(ctx, params, selection)
	}
	jobCtx, cancel := context.WithTimeout(ctx, enhancementJobTimeout)
	defer cancel()
	id, err := r.assist.begin(r, cancel)
	if err != nil {
		return nil, err
	}
	defer r.assist.end(id)
	done, err := r.complete(jobCtx, selection.Provider, selection.Model, params.System, []byte(params.User), completionLimits{
		MaxTokens:       params.MaxOutputTokens,
		AllowTruncation: true,
		Unavailable:     assistUnavailable,
		OverBudget:      assistOverBudget,
	})
	if err != nil {
		if anthropic.CredentialRejected(err) {
			return nil, &Error{Code: ErrProviderAuth, Message: r.errorText(err)}
		}
		return nil, r.assistError(err)
	}
	return &AssistResult{
		OK: true, Provider: selection.Provider, Model: selection.Model,
		ContextWindow: done.Capability.ContextWindow, MaxOutputTokens: done.MaxTokens,
		Text: done.Text, Reasoning: done.Reasoning, StopReason: string(done.Stop),
		Truncated:   done.Stop == llm.StopMaxTokens,
		InputTokens: done.Usage.InputTokens, OutputTokens: done.Usage.OutputTokens,
	}, nil
}

// assistDryRun answers from the provider's static capability and calls no model.
func (r *EnhancementRunner) assistDryRun(ctx context.Context, params AssistParams, selection EnhancementConfiguration) (*AssistResult, error) {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return nil, &store.WorkbenchError{Code: "busy", Message: "the enhancement worker is busy or closed"}
	}
	_, capability, err := r.resolveModel(ctx, selection.Provider, selection.Model, assistUnavailable)
	if err != nil {
		return nil, r.assistError(err)
	}
	applied := params.MaxOutputTokens
	if capability.MaxOutputTokens > 0 && capability.MaxOutputTokens < applied {
		applied = capability.MaxOutputTokens
	}
	return &AssistResult{OK: true, Provider: selection.Provider, Model: selection.Model,
		ContextWindow: capability.ContextWindow, MaxOutputTokens: applied}, nil
}

func assistUnavailable(provider, model string) error {
	return fmt.Errorf("provider %q declares no context window for model %q, so none is assumed; select a model the provider serves", provider, model)
}

func assistOverBudget(promptTokens, outputTokens, window int) error {
	return fmt.Errorf("the prompt does not fit the model's context window: about %d prompt tokens (bytes/3, rounded up) plus %d output tokens exceeds %d; send less", promptTokens, outputTokens, window)
}
