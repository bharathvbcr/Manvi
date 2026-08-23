// Package agent is the turn/step driver — the default agent loop.
//
// The flow, ported from the reference architecture:
//
//	turn/start
//	  assemble system prompt + tool schemas
//	  agent/pre-step waterfall        reject, or rewrite the messages entering
//	  step/start
//	    llm/request waterfall          last chance to shape the request
//	    provider stream -> chunks -> assistant/message
//	    tool calls, each through the tools pipeline
//	  step/end
//	  agent/turn-stopping serial       terminal checkpoint
//	turn/end
//
// Two properties are load-bearing and neither is optional.
//
// The history sent to a model is *always* projected from the session log, never
// accumulated in a local slice. That is what makes the model-visible-means-
// logged invariant enforceable: there is no code path that could build a
// request from anything else.
//
// And the loop terminates on evidence, not on the model saying it is finished.
// It stops when the provider reports no tool calls, when a step ceiling is hit,
// or when a turn-stopping listener says so.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"manvi/core/bus"
	"manvi/llm"
	"manvi/session"
	"manvi/tools"
)

// PreStep is the waterfall that runs before a step is entered. A listener may
// rewrite the messages or reject the step entirely — compaction and steering
// both live here.
type PreStep struct {
	Ctx      context.Context
	Turn     int
	Step     int
	Messages []llm.Message
	// Reject, when set, abandons the step without spending it.
	Reject error
}

// LLMRequest is the waterfall wrapping request assembly.
type LLMRequest struct {
	Ctx     context.Context
	Request llm.Request
}

// TurnStopping is the serial terminal checkpoint. A listener returning an error
// keeps the turn open; returning nil lets it close.
type TurnStopping struct {
	Turn     int
	Steps    int
	Response llm.Response
}

// Config drives one loop.
type Config struct {
	Provider llm.Provider
	Registry *llm.Registry
	Model    string
	// Effort is the reasoning tier a turn *starts* at. It is not necessarily
	// the tier every request carries: see EffortCeiling.
	Effort string
	// EffortCeiling is how far the loop may raise Effort within one turn when
	// that turn is going in circles, expressed as a level on the model's own
	// ladder. Empty means it never raises it, which is what every run did
	// before this existed.
	//
	// Reasoning is not uniformly worth its price. Measured on this harness
	// against Qwen3.8-27B: the same "fix the binary-search bug" task passed at
	// effort low in 238s and 2,860 generated tokens, and failed with reasoning
	// off — a wrong fix, then 41 model round trips over 674s spent defending
	// it, ending on the step ceiling with the bug still there. On a mechanical
	// task (one literal replaced by a constant at 90 sites) reasoning cost 2.6x
	// the tokens and 1.8x the wall clock for an identical result: both variants
	// passed. One static tier cannot be right for both, and the harness cannot
	// tell which kind of task it has been handed by reading the prompt.
	//
	// What it can see is a turn that has stopped getting anywhere, which is
	// already tracked for other reasons — see the repeat ledger and
	// agent/progress.go. So the tier is raised on that evidence rather than
	// guessed from the request: mechanical work never trips it and stays cheap,
	// and work that is genuinely stuck buys more thinking exactly when the
	// cheap tier has been shown not to be enough.
	EffortCeiling string
	// SystemPrompt is the assembled prompt for this run.
	SystemPrompt string
	// MaxSteps bounds a turn. A ceiling in code, not a prompt instruction:
	// models delegate and retry readily, and an instruction is not a bound.
	//
	// It is spent per step, but not always one at a time. A step that produced
	// observable progress costs 1; a step that ran tool calls and changed
	// nothing observable costs StallCost — see agent/progress.go for what
	// "progress" means and why it is defined that way. Because no step costs
	// less than 1, MaxSteps is still a hard ceiling on the number of steps a
	// turn can take: the budget can be spent faster than one per step, never
	// extended. A turn that is getting somewhere gets the whole ceiling; a turn
	// going in circles reaches the end of it sooner.
	MaxSteps int
	// MaxTokens is passed through to the provider.
	MaxTokens int
	// ReadOnly offers only non-mutating tools, for search agents.
	ReadOnly bool
	// CoreToolsOnly omits tools marked Extended — task lifecycle, code graph,
	// sub-agent dispatch. It removes capability as well as tokens, so it is a
	// deliberate choice for a model that picks badly from a long list rather
	// than something to turn on for the saving alone.
	CoreToolsOnly bool
	// AssertInvariant enables the model-visible-means-logged check before every
	// dispatch. Mirrors flags.LogModelVisibleAssert.
	AssertInvariant bool
}

// RepeatLimit is the maximum number of times an identical tool call may run
// in one turn before being refused.
const RepeatLimit = 3

type repeatTracker struct {
	counts map[string]int
}

func newRepeatTracker() *repeatTracker {
	return &repeatTracker{counts: make(map[string]int)}
}

func (r *repeatTracker) seen(name string, args []byte) int {
	if r == nil {
		return 1
	}
	key := name + ":" + string(args)
	r.counts[key]++
	return r.counts[key]
}

// Loop is the default turn/step driver.
type Loop struct {
	cfg     Config
	bus     *bus.Bus
	log     *session.Log
	tools   *tools.Registry
	repeats *repeatTracker
	// progress decides whether a step got anywhere. It is rebuilt per turn,
	// after tool registration, because it reads the registry to learn which
	// tools are allowed to mutate.
	progress *progressTracker
	// effortPlan is the ladder this loop may climb when a turn goes in
	// circles, resolved once against the model's declared levels.
	effortPlan EffortPlan
	// overflowed records that compaction could not make history fit. Set by
	// the compaction path, copied onto the Outcome at turn end. Reset per turn
	// alongside the other per-turn state.
	overflowed bool
	// effort is the tier the *next* request will carry. It is per-turn state,
	// reset by Run: a turn that had to think harder says nothing about the next
	// one, for the same reason the repeat ledger and the progress tracker do
	// not survive a turn either.
	effort string
	// calib corrects the token estimator against the counts the server
	// reports, so the budget converges on the tokenizer's answer instead of a
	// heuristic's.
	calib Calibrator
	// lastEstimate is what the harness thought the request it just sent would
	// cost, held so the server's reply can be compared against it.
	lastEstimate int
}

// NewLoop builds a driver.
func NewLoop(cfg Config, b *bus.Bus, log *session.Log, registry *tools.Registry) (*Loop, error) {
	if cfg.Provider == nil {
		return nil, errors.New("agent: no provider")
	}
	if cfg.Model == "" {
		return nil, errors.New("agent: no model")
	}
	if cfg.MaxSteps <= 0 {
		return nil, errors.New("agent: MaxSteps must be positive; an unbounded turn is not a turn")
	}

	plan, err := planEffort(cfg)
	if err != nil {
		return nil, err
	}

	l := &Loop{
		cfg: cfg, bus: b, log: log, tools: registry,
		repeats:    newRepeatTracker(),
		effortPlan: plan,
		effort:     cfg.Effort,
	}

	// Compaction runs in the pre-step waterfall, but it does not rewrite the
	// messages travelling through it. It appends to the log and re-derives, so
	// what the model receives is what the log says — see agent/compaction.go
	// for why that is load-bearing rather than tidy.
	if b != nil {
		if _, err := bus.OnWaterfall(b, func(e PreStep, next bus.Next[PreStep]) PreStep {
			messages, err := l.compact(e.Messages)
			if err != nil {
				e.Reject = err
				return next(e)
			}
			e.Messages = messages
			return next(e)
		}); err != nil {
			// A compaction stage that failed to register is a turn that will
			// overflow its window with nothing standing in the way — and no
			// error anywhere saying why. Registration is construction work;
			// it fails loudly here or not at all.
			return nil, fmt.Errorf("agent: registering compaction: %w", err)
		}
	}

	return l, nil
}

// Budget reports what this loop has to fit inside.
//
// A capability that does not state a context window is a gap in what the
// adapter could discover, not a licence to guess large: the fallback is small
// enough that being wrong costs an early compaction rather than a request the
// server truncates mid-turn.
func (l *Loop) Budget() Budget {
	const fallbackWindow = 32768
	const fallbackOutput = 8192
	// Chat-template scaffolding, tool-call framing and the generation prompt
	// are not modelled token for token; this covers them.
	const templateOverhead = 2048

	cap, ok := l.cfg.Provider.Capability(l.cfg.Model)
	window := fallbackWindow
	if ok && cap.ContextWindow > 0 {
		window = cap.ContextWindow
	}
	reserved := l.effectiveMaxTokens()
	if reserved <= 0 {
		reserved = fallbackOutput
	}
	// A reservation at or above the window leaves nothing for history, and the
	// arithmetic would clamp to the floor without ever saying why. Hold back
	// half the window at most.
	if reserved > window/2 {
		reserved = window / 2
	}
	return Budget{ContextWindow: window, ReservedOutput: reserved, Overhead: templateOverhead}
}

// compact plans, records and re-derives. It returns the history to send.
func (l *Loop) compact(messages []llm.Message) ([]llm.Message, error) {
	schemas := l.schemas()
	// The ledger of what is already compacted is read from the log, not held
	// on the Loop. A Loop lasts one turn; the log lasts the session, and a
	// per-turn ledger silently re-compacts everything from the previous turn.
	// See session.Log.CompactedCalls for what that costs.
	already, err := l.log.CompactedCalls()
	if err != nil {
		return nil, err
	}

	plan := PlanCompactionCalibrated(messages, l.cfg.SystemPrompt, schemas, l.Budget(), already, &l.calib)
	if plan.Empty() {
		if plan.Insufficient {
			if err := l.reportOverflow(plan); err != nil {
				return nil, err
			}
			l.overflowed = true
		}
		return messages, nil
	}

	// Apply writes a ToolResultCompacted event per step, which *is* the ledger:
	// the next call reads it back through CompactedCalls. There is deliberately
	// no second copy held on the Loop — that copy was the bug, because it did
	// not survive the turn boundary the log does.
	if err := plan.Apply(l.log); err != nil {
		return nil, err
	}
	if plan.Insufficient {
		if err := l.reportOverflow(plan); err != nil {
			return nil, err
		}
	}

	// Re-derive rather than patching in place. The projection is the only
	// thing that decides what a compaction means, and reading it back here is
	// what guarantees the request and the log cannot disagree.
	return l.log.DeriveMessages()
}

// reportOverflow records that history still exceeds the window after
// compaction. It is an event rather than an error: the turn can still be
// attempted, and the server's own refusal is more informative than a
// pre-emptive one — but it must not happen silently.
// overflowed is set by the compaction path and read once at turn end. It lives
// on the Loop rather than being threaded through because compaction runs from a
// helper that has no Outcome in hand, and inventing a return value for it would
// put the signal on a path that three callers would each have to remember to
// propagate — which is how it came to be unreported in the first place.
// reportOverflow records that history still exceeds the window after
// compaction. It is an event rather than an error: the turn can still be
// attempted, and the server's own refusal is more informative than a
// helper that has no Outcome in hand. The Append's own failure, though, is
// returned: a durable record that silently failed to write is exactly the
// "must not happen silently" this function exists for.
func (l *Loop) reportOverflow(plan CompactionPlan) error {
	_, err := l.log.Append(session.ContextOverflow, session.OverflowData{
		EstimatedTokens: plan.After,
		Threshold:       l.Budget().Threshold(),
		ContextWindow:   l.Budget().ContextWindow,
	})
	return err
}

// mergeDecoding accumulates compensation flags across a turn.
//
// Each flag means "this happened at least once", so once set it stays set.
// FallbackFormat keeps the first name seen rather than the last: a server that
// needed text recovery on step 1 is the same misconfiguration whether or not
// step 6 needed it too, and the earliest evidence is the one an operator can
// still find in the log.
func mergeDecoding(into, from llm.DecodingReport) llm.DecodingReport {
	if into.FallbackFormat == "" {
		into.FallbackFormat = from.FallbackFormat
	}
	into.ReasoningReclassified = into.ReasoningReclassified || from.ReasoningReclassified
	into.PrefillDisproved = into.PrefillDisproved || from.PrefillDisproved
	return into
}

// hitOutputCap reports whether a response ran to the end of the output budget,
// whatever the server said about why it stopped.
//
// The stop reason alone is not enough, because it is a claim made by the wire
// and servers get it wrong. Measured against omlx 0.6.2 on 2026-08-19: three
// separate responses generated exactly the requested 16,384 tokens — 495s,
// 452s and 398s of a model looping inside its reasoning block — and every one
// of them was reported as finish_reason="stop" rather than "length". Keyed on
// the stop reason alone, the harness recorded all three as models that had
// finished saying what they had to say.
//
// The token count is something the harness can check for itself: it chose the
// budget, and the usage comes back with the response. Only usable when a budget
// was actually set — a zero MaxTokens means the adapter picked its own default
// and the loop does not know the number to compare against, so that case still
// rests on the stop reason.
func (l *Loop) hitOutputCap(response llm.Response) bool {
	if response.StopReason == llm.StopMaxTokens {
		return true
	}
	// The adapter's own number first: it built the request and is the only
	// thing that knows what bound went on the wire. effectiveMaxTokens is a
	// fallback for adapters that do not report one, and it is an estimate —
	// Capability.MaxOutputTokens is the model's ceiling, which on three of the
	// four providers is a different number from the one actually sent.
	budget := response.MaxTokensApplied
	if budget <= 0 {
		budget = l.effectiveMaxTokens()
	}
	return budget > 0 && response.Usage.OutputTokens >= budget
}

// effectiveMaxTokens is the output budget actually in force, which is not the
// same thing as the one this loop was configured with.
//
// cfg.MaxTokens is routinely zero: `manvi run` leaves it so on purpose, because
// zero means "whatever the adapter defaults to" rather than "no budget". A
// check written against cfg.MaxTokens is therefore comparing a response to a
// number nobody used — which is how the first version of hitOutputCap came to
// be dead code on the one path that most needed it. A benchmark turn ran to
// exactly the adapter's 16,384-token default, in 410 seconds, and was reported
// as a model that had finished.
//
// Zero here means genuinely unknown — no configured value and no declared
// capability — and callers treat it as "cannot say" rather than "no limit".
func (l *Loop) effectiveMaxTokens() int {
	if l.cfg.MaxTokens > 0 {
		return l.cfg.MaxTokens
	}
	if cap, ok := l.cfg.Provider.Capability(l.cfg.Model); ok && cap.MaxOutputTokens > 0 {
		return cap.MaxOutputTokens
	}
	return 0
}

// reportMalformed records the failure and hands the model a usable account of
// it, as a user message rather than a tool result: a tool result must name a
// call id the assistant message actually contains, and a call that could not be
// reconstructed was never surfaced as one.
func (l *Loop) reportMalformed(response llm.Response) error {
	var b strings.Builder
	b.WriteString("Your last message contained tool call(s) this harness could not read:\n")
	for _, m := range response.Malformed {
		name := m.Name
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Fprintf(&b, "  - %s: %s\n", name, m.Reason)
	}
	if l.hitOutputCap(response) {
		b.WriteString("\nThe response hit its output limit, which is what cut them off. ")
		b.WriteString("Make one call at a time, or pass smaller arguments.")
	} else {
		b.WriteString("\nSend the call again in full.")
	}

	for _, m := range response.Malformed {
		if _, err := l.log.Append(session.MalformedToolCall, session.MalformedCallData{
			ToolCallID: m.ID, Tool: m.Name, Reason: m.Reason,
		}); err != nil {
			return err
		}
	}
	_, err := l.log.Append(session.UserMessage, session.MessageData{
		Message: llm.Message{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{llm.TextBlock{Text: b.String()}},
		},
	})
	return err
}

func (l *Loop) schemas() []llm.ToolSchema {
	if l.tools == nil {
		return nil
	}
	if l.tools.IsDynamic() {
		switch {
		case l.cfg.ReadOnly && l.cfg.CoreToolsOnly:
			return l.tools.ActiveCoreReadOnlySchemas()
		case l.cfg.ReadOnly:
			return l.tools.ActiveReadOnlySchemas()
		case l.cfg.CoreToolsOnly:
			return l.tools.ActiveCoreSchemas()
		default:
			return l.tools.ActiveSchemas()
		}
	}
	switch {
	case l.cfg.ReadOnly && l.cfg.CoreToolsOnly:
		return l.tools.CoreReadOnlySchemas()
	case l.cfg.ReadOnly:
		return l.tools.ReadOnlySchemas()
	case l.cfg.CoreToolsOnly:
		return l.tools.CoreSchemas()
	default:
		return l.tools.Schemas()
	}
}

// Outcome summarises a completed turn.
type Outcome struct {
	Steps      int
	StopReason llm.StopReason
	Usage      llm.Usage
	Final      llm.Message
	// ToolCalls is how many tool calls ran across the turn.
	ToolCalls int
	// Denied is how many were short-circuited by a pre-execute listener.
	Denied int

	// Qualified is how many were allowed, but not on the rules alone — a
	// demotion, a widening, or a check that could not run. Kept apart from
	// Denied because they are opposite outcomes: these calls ran and their
	// effects landed. Folding them together reported a yolo run, in which
	// nothing is refused, as one in which every write was.
	Qualified int
	// Repeated is how many calls were refused for being verbatim repeats. It is
	// reported rather than folded into Denied: a policy refusal and a model
	// going in circles are different problems with different fixes.
	Repeated int
	// Stalled is how many calls were refused because the turn had spent
	// NoProgressLimit consecutive steps producing nothing observable.
	//
	// Kept apart from Repeated as well as from Denied. A verbatim repeat and
	// near-duplicate churn are the same lack of progress wearing different
	// arguments, and an operator who sees them as one number cannot tell
	// whether the repeat ledger is working or being walked around. Neither is
	// a policy denial: the gate never saw these calls.
	Stalled int
	// NoProgressSteps counts steps that ran tool calls and produced nothing
	// this turn had not already been told. It is the diagnostic behind
	// BudgetSpent — the reason a turn with a 500-step ceiling can end at 170.
	NoProgressSteps int
	// BudgetSpent is how much of MaxSteps the turn consumed. It is not Steps:
	// a step that got nowhere costs StallCost. Reported so a truncated turn can
	// be read without guessing which of the two ended it.
	BudgetSpent int
	// TruncatedBySteps is true when the step ceiling ended the turn rather than
	// the model finishing. Reported, never silent: a turn that ran out of steps
	// has not completed its work, and a caller that cannot tell will treat it
	// as if it had.
	TruncatedBySteps bool
	// TruncatedByTokens is true when a response hit the output cap. It is
	// separate from TruncatedBySteps because the two have different fixes —
	// raise the output cap, or raise the step ceiling — and because a
	// max-tokens stop with no tool calls otherwise ends a turn looking exactly
	// like a model that finished.
	TruncatedByTokens bool
	// FinalTruncated is true when the response that *ended* the turn hit the
	// output cap. Kept apart from TruncatedByTokens because only one of them
	// means the caller was handed incomplete work: a turn that hit the cap
	// mid-way and then finished properly is whole, and a signal that fired on
	// both would be ignored. This one fires when there is no next step to put
	// it right — the answer stops mid-sentence and the turn ends looking
	// exactly like one that was done.
	FinalTruncated bool
	// FinalEmpty is true when the response that ended the turn carried no
	// answer and no tool call — nothing but reasoning, or nothing at all.
	//
	// Such a turn has not finished; it has run out of anything to say. It is
	// tracked separately from FinalTruncated because the harness cannot rely on
	// the stop reason to notice it. Observed against a local server: a model
	// looped inside its reasoning block, emitted exactly the 16,384-token
	// output cap over 495 seconds, and the server reported the stop reason as
	// "stop" rather than "length" — so FinalTruncated stayed false, no tool
	// calls meant a natural stop, and a turn that changed no files and produced
	// no answer exited 0.
	//
	// The check deliberately does not consult StopReason. Whether the wire told
	// the truth about why generation ended is exactly what cannot be assumed;
	// whether an answer arrived is something the harness can see for itself.
	FinalEmpty bool
	// ContextOverflowed is true when history still exceeded the budget after
	// everything compactable had been compacted.
	//
	// reportOverflow's own comment says this "must not happen silently", and
	// it did: the event it appends has no case in ui.Project, which is the only
	// bridge from the session log to either face, and there was no Outcome
	// field for it either. So the one signal saying the session has outgrown
	// its window — and that compaction can no longer save it — existed solely
	// as a JSON blob nothing read.
	ContextOverflowed bool
	// Panicked counts calls refused because a stage of the tool pipeline
	// panicked. A defect in this harness rather than in the model or the
	// server, and the only class of refusal that is nobody's decision.
	Panicked int
	// Malformed counts tool calls the adapter could not reconstruct.
	Malformed int
	// Decoding reports compensations the adapter made across the turn, so a
	// misconfigured server is visible in the outcome rather than only in a log.
	Decoding llm.DecodingReport

	// EffortFrom is the reasoning tier the turn's first request carried, and
	// EffortTo the tier its last one did. They are equal unless the loop raised
	// the tier because the turn was going in circles.
	//
	// Both are reported, not just the fact that they differ: a turn that cost
	// what a high-effort turn costs while having been asked for at low is not
	// explainable from the token counts alone, and an escalation nobody can see
	// is a bill with no line item.
	EffortFrom string
	EffortTo   string
	// EffortRaised counts the steps at which the tier was raised a rung. It is
	// zero on every turn that never escalated, including every turn run without
	// a ceiling configured.
	EffortRaised int
}

// Run drives one turn to completion.
func (l *Loop) Run(ctx context.Context, prompt llm.Message) (Outcome, error) {
	l.repeats = newRepeatTracker()
	l.progress = newProgressTracker(l.tools)
	l.overflowed = false
	turnEvent, err := l.log.Append(session.TurnStart, nil)
	if err != nil {
		return Outcome{}, err
	}
	currentTurn := turnEvent.Turn
	if l.cfg.SystemPrompt != "" && l.log.SystemPrompt() != l.cfg.SystemPrompt {
		if _, err := l.log.Append(session.SystemPrompt,
			session.SystemPromptData{Text: l.cfg.SystemPrompt}); err != nil {
			return Outcome{}, err
		}
	}
	if _, err := l.log.Append(session.UserMessage,
		session.MessageData{Message: prompt}); err != nil {
		return Outcome{}, err
	}

	l.effort = l.cfg.Effort

	var out Outcome
	out.EffortFrom, out.EffortTo = l.cfg.Effort, l.cfg.Effort
	// nullRetries counts responses that carried nothing, across the whole turn
	// rather than per step: a server producing them repeatedly is not a
	// condition more attempts will fix, and an unbounded retry would spend the
	// turn's budget on empty requests.
	nullRetries := 0
	for step := 1; ; step++ {
		// The budget, not the step number. They are the same for a turn that
		// keeps getting somewhere, and the ceiling still cannot be exceeded
		// either way, because the cheapest step costs 1.
		if out.BudgetSpent >= l.cfg.MaxSteps {
			out.TruncatedBySteps = true
			break
		}

		// Cleared per step, because both describe *the response that ended the
		// turn*. They are set in the no-tool-calls branch below, which is not
		// necessarily the last step: the terminal checkpoint may keep the turn
		// open, and a value left from an earlier step then described a later
		// response that did have tool calls — "the last response carried no
		// text and no tool call" printed about one that carried both.
		out.FinalTruncated, out.FinalEmpty = false, false

		// History is always projected from the log. There is no other source.
		messages, err := l.log.DeriveMessages()
		if err != nil {
			return out, err
		}

		pre, err := bus.Waterfall(l.bus, PreStep{
			Ctx: ctx, Turn: currentTurn, Step: step, Messages: messages,
		})
		if err != nil {
			return out, err
		}
		if pre.Reject != nil {
			return out, fmt.Errorf("agent: step %d rejected: %w", step, pre.Reject)
		}
		messages = pre.Messages

		if _, err := l.log.Append(session.StepStart, nil); err != nil {
			return out, err
		}

		response, err := l.step(ctx, messages)
		if err != nil {
			return out, err
		}
		out.Steps = step
		out.StopReason = response.StopReason
		out.Usage.InputTokens += response.Usage.InputTokens
		out.Usage.OutputTokens += response.Usage.OutputTokens
		out.Usage.ReasoningTokens += response.Usage.ReasoningTokens
		out.Usage.CacheReadTokens += response.Usage.CacheReadTokens
		// Throughput is a rate, not a total: the last step's is what an
		// operator wants to see, and summing them would be meaningless.
		if response.Usage.OutputTokensPerSecond > 0 {
			out.Usage.OutputTokensPerSecond = response.Usage.OutputTokensPerSecond
		}
		if response.Usage.PromptTokensPerSecond > 0 {
			out.Usage.PromptTokensPerSecond = response.Usage.PromptTokensPerSecond
		}
		out.Final = response.Message

		if !response.Decoding.Clean() {
			// Merged, not assigned. Outcome.Decoding is documented as reporting
			// compensations "across the turn", and plain assignment made it
			// report the last non-clean step only. A turn whose first step
			// disproved a declared prefill and whose sixth recovered a tool
			// call from text ended with the prefill flag erased — so the notice
			// that exists to get llm.local.assume_reasoning_prefill turned off
			// never printed, on exactly the turns where it was needed, and the
			// same condition recurred on the next turn.
			out.Decoding = mergeDecoding(out.Decoding, response.Decoding)
			if _, err := l.log.Append(session.DecodingCompensated, session.DecodingData{
				FallbackFormat:        response.Decoding.FallbackFormat,
				ReasoningReclassified: response.Decoding.ReasoningReclassified,
				PrefillDisproved:      response.Decoding.PrefillDisproved,
			}); err != nil {
				return out, err
			}
		}

		if l.hitOutputCap(response) {
			out.TruncatedByTokens = true
		}

		// A response that carried nothing at all is asked for again, a bounded
		// number of times, before it is allowed to end the turn.
		//
		// Not a papering-over of a bad answer: this is the response with no
		// text, no tool call, and zero output tokens — the server completed the
		// interaction having generated nothing. Measured against the live
		// Gemini endpoint on 2026-08-19, 5 of 21 responses in one benchmark
		// came back this way (status "completed", total_output_tokens 0, a lone
		// content-free thought step), which ends four of five scenarios on a
		// turn that did no work.
		//
		// It is retried rather than reported because nothing was produced to be
		// harmed by asking again — the same reason an error frame before any
		// content is retried — and because the alternative is a harness that
		// reports "the model stopped having anything to say" for a provider
		// hiccup. What is not done is hiding it: every retry is recorded, and a
		// null response that survives the budget still ends the turn as empty.
		if isNullResponse(response) && nullRetries < maxNullRetries {
			nullRetries++
			if _, err := l.log.Append(session.NullResponseRetried, session.NullResponseData{
				Step: step, Attempt: nullRetries, Of: maxNullRetries,
			}); err != nil {
				return out, err
			}
			// The step is over even though the turn continues: leaving its
			// start unclosed made every retried response dangle a step/start
			// in the durable log for the TUI projector, the invariant check,
			// and resume to trip over.
			if _, err := l.log.Append(session.StepEnd, nil); err != nil {
				return out, err
			}
			continue
		}

		calls := response.Message.ToolCalls()
		// ran counts the calls that actually reached the pipeline, so a step
		// whose every call was refused is not mistaken for one that worked.
		ran := 0
		progressed := false
		// circling records that this step refused at least one call because the
		// turn was going round — as a verbatim repeat or for making no
		// observable progress. It is per step rather than per call so a step
		// that refused three calls climbs one rung, not three.
		circling := false
		for _, call := range calls {
			if _, err := l.log.Append(session.ToolCall, session.ToolCallData{
				ID: call.ID, Name: call.Name, Arguments: call.Arguments,
			}); err != nil {
				return out, err
			}

			var result tools.Result
			repeat := l.repeats.seen(call.Name, call.Arguments)
			switch {
			case repeat > RepeatLimit:
				// Refused rather than run, and said out loud.
				//
				// A model that repeats one call verbatim is not making
				// progress, and re-running it cannot produce a different
				// answer: the arguments are identical and nothing between the
				// two calls changed the world. Left alone this consumes the
				// entire step budget — observed on a one-file edit, where
				// twelve identical greps ran and the turn ended with the file
				// untouched and 84k tokens of context spent.
				//
				// The refusal carries the repeat count and names what would
				// actually move: it is a fact the model can act on, not a
				// silent skip that would look to it like the tool returning
				// nothing.
				out.Repeated++
				circling = true
				// Deliberately carries no Rule. A result with one is recorded
				// as session.PolicyDenied, and this refusal did not come from
				// the gate — the gate never saw the call. Tagging it would put
				// a denial in the evidence trail that no policy made, and the
				// trail is the thing the whole system reasons from. The model
				// gets a plain error result, which is what it can act on
				// anyway; the count is reported separately in Outcome.Repeated.
				result = tools.Result{
					IsError: true,
					Text: fmt.Sprintf(
						"this exact call to %s was already made %d times in this turn with the same "+
							"arguments, so it was not run again — an identical call cannot return a "+
							"different answer. Use what the earlier result told you, call it with "+
							"different arguments, or take the next action the task actually needs.",
						call.Name, repeat-1),
				}

			case l.progress.stalled():
				// The same lack of progress, wearing different arguments.
				//
				// Varying one character of a pattern walks straight around the
				// verbatim ledger above while telling the model nothing it did
				// not already know — five distinct greps, five identical empty
				// results, the file untouched. This refusal keys on what came
				// back rather than what was asked for, so the disguise does not
				// work; see agent/progress.go.
				//
				// It carries no Rule, for the same reason the repeat refusal
				// does not: a result with one is recorded as
				// session.PolicyDenied, and no policy made this decision — the
				// gate never saw the call. A denial the gate never made in the
				// evidence trail would be a lie told to the thing the whole
				// system reasons from. The count is reported separately in
				// Outcome.Stalled, and the model gets a plain error result,
				// which is what it can act on anyway.
				out.Stalled++
				circling = true
				l.progress.interrupted()
				result = tools.Result{
					IsError: true,
					Text: fmt.Sprintf(
						"the last %d steps changed nothing and returned nothing this turn had not "+
							"already been told, so %s was not run. Varying the arguments of a search "+
							"that keeps coming back the same is not progress. Act on what you already "+
							"have: make the edit, run the command that would change something, or say "+
							"what is blocking you.",
						NoProgressLimit, call.Name),
				}

			default:
				result = l.tools.Run(ctx, tools.Call{
					ID: call.ID, Name: call.Name, Arguments: call.Arguments,
				})
				ran++
				if l.progress.observe(call.Name, result) {
					progressed = true
				}
			}
			out.ToolCalls++

			// A gate decision is recorded as its own event *in addition to*
			// the tool result, so the evidence trail explains the outcome
			// without the model-facing result having to carry policy detail.
			//
			// Blocked is the gate's own verdict, and the only thing that says
			// a call was refused. The loop used to infer it, first from Rule
			// alone and then from Rule with IsError, and both readings were
			// wrong in the same direction. A Rule means policy decided, not
			// which way — devcouncil.annotate puts one on *allowed* results
			// too, naming the rule that would have blocked a write the posture
			// or a grant let through. Adding IsError narrowed the misread
			// without closing it: a command the gate allowed under a demotion
			// and which then failed on its own exit code has both, and a yolo
			// run — in which nothing can be refused by construction — reported
			// it as "refused by the gate".
			//
			// Blocked first, because a refusal is a refusal however many other
			// qualifications it also carries; under a disabled hard-rule set an
			// enforcing soft rule can refuse a call whose Degraded list is not
			// empty, and that is still a denial.
			switch {
			case result.PipelinePanic != "":
				// Ahead of Qualified: a stage that could not finish did not
				// allow anything, and "allowed but not on the rules alone" is
				// the opposite of what happened.
				out.Panicked++
				// Still recorded, and deliberately as a qualification. Taking
				// this case away from Qualified() also took its log entry, and
				// that entry was the only durable trace — Outcome.Panicked is
				// in memory and the stack goes to stderr, so a resumed or
				// replayed session showed nothing at all. tools.stagePanic's
				// own comment asserts the log records this; the record is what
				// keeps that true. The claim it makes is exactly right here:
				// this outcome was not reached by the rules running cleanly.
				if _, err := l.log.Append(session.PolicyQualified, session.QualificationData{
					ToolCallID: call.ID, Tool: call.Name,
					Reason: result.Text, Degraded: result.Degraded,
				}); err != nil {
					return out, err
				}
			case result.Blocked:
				out.Denied++
				if _, err := l.log.Append(session.PolicyDenied, session.DenialData{
					ToolCallID: call.ID, Tool: call.Name,
					Rule: result.Rule, Severity: result.Severity, Reason: result.Text,
				}); err != nil {
					return out, err
				}
			case result.Qualified():
				out.Qualified++
				if _, err := l.log.Append(session.PolicyQualified, session.QualificationData{
					ToolCallID: call.ID, Tool: call.Name,
					Rule: result.Rule, Severity: result.Severity, Reason: result.Text,
					Demoted: result.Demoted, Widened: result.Widened, Degraded: result.Degraded,
				}); err != nil {
					return out, err
				}
			}
			if result.GrantID != "" {
				if _, err := l.log.Append(session.GrantApplied, session.GrantData{
					GrantID: result.GrantID, GrantedBy: result.GrantedBy,
					Rule: result.Rule, Target: call.Name, Reason: result.Text,
				}); err != nil {
					return out, err
				}
			}

			if _, err := l.log.Append(session.ToolResult, session.ToolResultData{
				ToolCallID: call.ID, Text: result.Text, IsError: result.IsError,
			}); err != nil {
				return out, err
			}
		}

		// The step is charged here, before any path that leaves the iteration,
		// so no route through the loop can take a step without paying for it.
		cost, noProgress := l.progress.endStep(len(calls), ran, progressed)
		out.BudgetSpent += cost
		if noProgress {
			out.NoProgressSteps++
		}

		// The turn has demonstrated that the tier it is running at is not
		// getting it anywhere, so the next request buys one rung more thinking.
		// Charged against the same evidence the refusal above was made on, and
		// never on a turn that is still getting somewhere: see agent/effort.go.
		//
		// Both halves are required. A refusal says the model asked for
		// something it had already been given; the absence of progress says
		// nothing else in the step made up for it. A step that refused a repeat
		// and still moved the work forward is a model correcting itself, which
		// is the behaviour this must not tax.
		if circling && !progressed {
			if next, ok := l.effortPlan.Next(l.effort); ok {
				l.effort = next
				out.EffortTo = next
				out.EffortRaised++
			}
		}

		// A call the adapter could not reconstruct is told to the model rather
		// than ending the turn. The step is not wasted: the model learns that
		// its request was cut off, which is something it can act on by asking
		// for less. Left as an error, a response that merely ran out of room
		// discarded every completed step of the turn with it.
		if len(response.Malformed) > 0 {
			out.Malformed += len(response.Malformed)
			if err := l.reportMalformed(response); err != nil {
				return out, err
			}
			if _, err := l.log.Append(session.StepEnd, nil); err != nil {
				return out, err
			}
			continue
		}

		if _, err := l.log.Append(session.StepEnd, nil); err != nil {
			return out, err
		}

		if len(calls) == 0 {
			// Not always a natural stop. A response with no tool calls that
			// stopped on the output cap did not finish — it ran out of room
			// mid-sentence — and the break below would end the turn as though
			// it had. Recorded before the checkpoint, which may yet keep the
			// turn open and let a later step finish the answer properly.
			out.FinalTruncated = l.hitOutputCap(response)
			// Asked of the response itself rather than of the stop reason, so
			// a server that mislabels why it stopped cannot turn a dead end
			// into a clean finish. Reasoning does not count as an answer: it is
			// stripped before the caller ever sees the message.
			out.FinalEmpty = strings.TrimSpace(response.Message.Text()) == ""

			// Natural stop. The terminal checkpoint may keep the turn open —
			// a verification gate that wants one more step, for instance.
			if err := bus.Serial(l.bus, ctx, TurnStopping{
				Turn: currentTurn, Steps: out.Steps, Response: response,
			}); err != nil {
				continue
			}
			break
		}
	}

	out.ContextOverflowed = l.overflowed

	if _, err := l.log.Append(session.TurnEnd, nil); err != nil {
		return out, err
	}
	return out, nil
}

// maxNullRetries bounds how many times a turn will re-ask for a response that
// carried nothing. Three, because the condition is transient and uncorrelated
// in what was measured — a fourth consecutive null is a provider that is not
// going to answer, and spending the whole step budget discovering that helps
// nobody.
const maxNullRetries = 3

// isNullResponse reports a response the server completed having generated
// nothing: no text, no tool call, and no output tokens.
//
// All three are required. Reasoning alone does not make a response non-null —
// it is stripped before the caller sees it — and a response with tool calls is
// work even with no prose. The output-token check is what keeps a deliberate
// empty answer, if a model ever gives one, from being retried forever: a model
// that generated tokens said something, whatever it was.
func isNullResponse(r llm.Response) bool {
	// Any content block at all — including a reasoning block — means the model
	// produced something. Reasoning is not an *answer*, which is why FinalEmpty
	// discounts it, but it is work: it was generated and it was billed, and
	// asking again would throw it away and pay for the replacement. The two
	// questions are genuinely different and were briefly conflated here, which
	// turned "the model thought and had nothing to say" — a real state the
	// turn's terminal checkpoint exists to handle — into a retry.
	if len(r.Message.Content) > 0 {
		return false
	}
	// And no tokens of any kind. This is the shape the live endpoint sends:
	// status "completed", total_output_tokens 0, total_thought_tokens 0, not a
	// single step carrying content.
	return r.Usage.OutputTokens == 0 && r.Usage.ReasoningTokens == 0
}

// step runs one model call and records it.
func (l *Loop) step(ctx context.Context, messages []llm.Message) (llm.Response, error) {
	schemas := l.schemas()

	// Provider-private state that this target cannot replay is stripped here,
	// and the removal is logged. Cross-provider handoff is explicitly lossy.
	prepared, drops := llm.PrepareHistoryFor(messages, l.cfg.Provider, l.cfg.Model)
	if len(drops) > 0 {
		if _, err := l.log.Append(session.ProvenanceDropped,
			session.DropData{Drops: drops}); err != nil {
			return llm.Response{}, err
		}
	}

	request := llm.Request{
		Model:     l.cfg.Model,
		System:    l.cfg.SystemPrompt,
		Messages:  prepared,
		Tools:     schemas,
		MaxTokens: l.cfg.MaxTokens,
		Effort:    l.effort,
	}

	shaped, err := bus.Waterfall(l.bus, LLMRequest{Ctx: ctx, Request: request})
	if err != nil {
		return llm.Response{}, err
	}
	request = shaped.Request

	// Assembly-time capability check, when a registry is available: an
	// impossible request fails here rather than as a 400 mid-turn.
	if l.cfg.Registry != nil {
		if _, _, err := l.cfg.Registry.Resolve(l.cfg.Provider.Name(), request); err != nil {
			return llm.Response{}, err
		}
	}

	if l.cfg.AssertInvariant {
		if err := session.AssertModelVisible(l.log, request.Messages); err != nil {
			return llm.Response{}, err
		}
	}

	// Held so the server's own prompt_tokens can be compared against it below.
	// The estimate is over the request as finally shaped, after every waterfall
	// — comparing against anything else would be measuring a prompt that was
	// not sent.
	l.lastEstimate = CountRequestTokens(request.System, request.Tools, request.Messages)

	stream, err := l.cfg.Provider.Stream(ctx, request)
	if err != nil {
		return llm.Response{}, err
	}
	defer func() { _ = stream.Close() }()

	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return llm.Response{}, err
		}
		if _, err := l.log.Append(session.AssistantChunk, chunk); err != nil {
			return llm.Response{}, err
		}
	}

	response, err := stream.Response()
	if err != nil {
		return llm.Response{}, err
	}
	// The server counted the prompt with the real tokenizer. Feeding that back
	// is what moves the budget off a byte heuristic and onto the truth.
	l.calib.Observe(l.lastEstimate, response.Usage.InputTokens)

	if _, err := l.log.Append(session.AssistantMessage,
		session.MessageData{Message: response.Message}); err != nil {
		return llm.Response{}, err
	}
	return response, nil
}
