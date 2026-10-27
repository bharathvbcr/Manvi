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
	"math"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
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

// TurnStopping is the serial terminal checkpoint: the last chance to say that a
// turn which looks finished is not.
//
// It is dispatched by pointer so a listener can answer rather than only object.
// The contract it replaced was "return an error to keep the turn open", and
// that shape could not work. The loop's response to the error was to take
// another step with the *same* history, so the model was re-asked a question it
// had already answered, with nothing added; a replayed fixture obligingly
// produced a second turn and the test passed, while a live model seeing an
// unchanged history has no reason to do anything but stop again. Riding that to
// the step ceiling is not a verification gate, it is a spin.
//
// So a listener that wants another step has to supply the reason, and the loop
// is what puts it in front of the model. That split is not stylistic: appending
// through the loop is what keeps model-visible-means-logged enforceable, and a
// listener holding the log could append something the model never saw.
//
// An error now means the listener itself failed. It does not keep the turn
// open — a check that could not run has not asked for anything — and it is
// never silent: the turn closes with Outcome.Sensor degraded.
// TurnStarting is published once, before the turn takes its first step and
// therefore before any tool has run.
//
// It exists so a listener can record what the working tree looked like before
// the agent touched it. Everything downstream that asks "what did this turn
// change" needs that state, and there is exactly one moment at which it can be
// read: a listener that waits for TurnStopping is reading the answer, not the
// question.
type TurnStarting struct {
	Turn int
}

// TurnEnded is published when Run returns, on every way out of a turn whose
// TurnStarting was published: a natural stop, an error from the provider or the
// log, a cancelled context, a listener's refusal and a panic. It is the end of
// the scope TurnStarting opens.
//
// It exists so a listener that started work at TurnStarting — the pre-turn
// baseline is recorded that way — can stop and collect it. Without it the work
// outlived any turn that failed before the terminal checkpoint, with its child
// processes still running after Run had returned. It is dispatched with a
// context that is never cancelled, because the turn's own may be the reason the
// turn ended, and a listener's errors are not the turn's: the turn is already
// over, and there is nobody left to hand one to.
type TurnEnded struct {
	Turn int
}

// ToolsDispatching is published before a response's tool calls run, once per
// response that carries any. It is the last point at which nothing the turn
// asked for has touched the world.
//
// It exists for a listener that started work at TurnStarting and let the turn
// go on without it. The pre-turn baseline is taken that way, so the first model
// request is not held behind it; the price is that the baseline must still be a
// photograph of the tree before any tool of the turn has run, and this is where
// that is enforced. A listener that returns an error ends the turn, as it does
// at TurnStarting: a listener that could not hold the line must not have its
// silence read as permission.
//
// PreStep cannot serve: it runs before a step's request, so the first tool call
// of a turn is dispatched before any PreStep that could wait for it. And the
// tool pipeline's own bus is not this one — it outlives the turn, where a
// listener here dies with the loop that registered it.
type ToolsDispatching struct {
	Turn  int
	Step  int
	Calls int
}

type TurnStopping struct {
	Turn     int
	Steps    int
	Response llm.Response

	// Mutated reports that at least one tool that can change the world ran
	// without error this turn. It is the progress tracker's definition, and it
	// is deliberately wider than Wrote: a command that touched the working tree
	// through the shell sets this and names no path.
	Mutated bool
	// Wrote names the repository-relative paths whose bytes this turn changed,
	// as reported by the handlers that changed them. This is what a lease-free
	// check runs against; `git diff` is not, because it also carries whatever
	// the operator had in the tree before the turn started.
	//
	// Bounded — see tools.MaxTrackedWrites — and WroteTruncated says when the bound bit.
	Wrote []string
	// WroteTruncated is true when more paths were written than are listed. A
	// checker that reads Wrote as the complete set would certify files it never
	// looked at, so the incompleteness travels with the list.
	WroteTruncated bool
	// UnenumeratedEffects reports that a tool changed the working tree without
	// naming what it changed — the shell, in practice.
	//
	// It is the difference between "nothing was written" and "something was
	// written and no handler could say what", which Wrote alone cannot express.
	// A turn that edits a.go natively and touches b.go through a command has a
	// one-entry Wrote list, and a checker reading only that list examines a.go
	// and certifies the turn. The end-of-turn check reads this to know its own
	// coverage is partial.
	UnenumeratedEffects bool
	// Truncated reports that the response ending this turn hit the output cap.
	// A cut-off answer is not a finished one, and a checkpoint that certified it
	// would be certifying a sentence that stops mid-word.
	Truncated bool
	// Empty reports that the ending response carried no answer and no tool
	// call.
	Empty bool

	// Inject, when a listener sets it, is appended to the log as a message from
	// the harness and the turn takes another step. Bounded by
	// MaxCheckpointBounces per turn; past that the loop ignores it and closes,
	// recording BouncesExhausted rather than pretending the turn was clean.
	Inject string
	// Verdict is what the listener's check concluded, carried onto the outcome
	// so no face can render a failed check as a completed turn. A listener that
	// leaves it empty is treated as having run no check at all.
	Verdict SensorVerdict
	// Bounce is which attempt this is within the turn: zero the first time the
	// checkpoint fires, one after a single inject, and so on. A listener that
	// wants to escalate on the second look reads this rather than keeping its
	// own counter — a counter in a listener closure outlives the turn, and the
	// two faces disagree about how long a bus does.
	Bounce int
	// Circling is how many calls this turn were refused for repeating
	// themselves or for getting nowhere. It is the same evidence the loop
	// already escalates reasoning effort on, offered here so a listener can
	// escalate on it too rather than inventing a second definition of "stuck".
	//
	// Offered rather than acted on: what to do about a circling turn is a
	// policy question, and this package deliberately holds no policy.
	Circling int
}

// SensorVerdict is what an end-of-turn check concluded.
//
// Four values, not a boolean, and the distinction between the last two is the
// whole point: a check that could not run must never report what a check that
// ran and passed reports.
type SensorVerdict string

const (
	// SensorNone means no check was attempted, because none was registered.
	SensorNone SensorVerdict = ""
	// SensorSkipped means a check was registered and deliberately did not run —
	// a turn that changed nothing has nothing to verify.
	SensorSkipped SensorVerdict = "skipped"
	// SensorPassed means a check ran and found nothing blocking.
	SensorPassed SensorVerdict = "passed"
	// SensorFailed means a check ran and found something.
	SensorFailed SensorVerdict = "failed"
	// SensorDegraded means a check was owed and could not be completed. It is
	// never a pass.
	SensorDegraded SensorVerdict = "degraded"
)

// MaxCheckpointBounces is how many times one turn may be sent back for another
// step by the terminal checkpoint.
//
// Two. The shape it is sized for is the one the bench produces: a check fails,
// the model is told what failed and fixes it, the check runs again. A third
// bounce has never been the difference between a fixed turn and a stuck one —
// a model that has been told twice and changed nothing is not going to be told
// into it — and every bounce is a full model call plus the steps it drags
// behind it. The alternative that was rejected is riding MaxSteps, which
// defaults to 500 and charges StallCost 3 for the steps a bounced turn tends to
// produce: the same dead end, discovered an hour later.
const MaxCheckpointBounces = 2

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
	// ChatOnly offers the model no tools. The native tool set is still
	// constructed, so Session, RepoReader, and Roles stay available; an empty
	// schema list is what stops the model from calling them. The zero value
	// leaves tools on.
	ChatOnly bool
	// AssertInvariant enables the model-visible-means-logged check before every
	// dispatch. Mirrors flags.LogModelVisibleAssert.
	AssertInvariant bool
	// Price, when set, prices one response's usage in nano-USD. Nil means the
	// model has no price table here, and every step is counted as unpriced.
	//
	// It is called once per step, not once per turn, because a price table can
	// switch rates on the size of a single request's prompt: two short requests
	// priced as their sum would be charged at the long-prompt rate.
	Price func(llm.Usage) (int64, error)
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
	// spent is what the open step has cost, written to its step/end. Reset when
	// a step opens and when it closes.
	spent session.StepEndData
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
	// turnOpen and stepOpen record that the log holds a turn/start or
	// step/start with no matching end yet. Run's deferred closer reads them so
	// every exit, including a failing one, leaves the log balanced.
	turnOpen, stepOpen bool
	// openCalls are the tool calls of the response in flight that no tool
	// result in the log answers yet, in the order the response made them. A
	// call joins when its response is logged and leaves when its result is. It
	// is empty on every normal path; it is what an abnormal exit has to answer.
	openCalls []llm.CallID
	// announced is the turn whose TurnStarting has been published and whose
	// TurnEnded has not, zero when there is none. Run's deferred closer reads
	// it, so the pair is balanced on every exit however early the turn failed.
	announced int
	// offered is the names of the tools the current step's request carried: the
	// set assembleRequest sent to the provider, after every waterfall had its
	// say, and nothing else. runCall refuses a call outside it. It is rebuilt
	// every step, because the set can change between steps (a tool activated
	// mid-turn is offered from the next request), and it is never widened by
	// anything a response says. The one widening is what the registry itself
	// activated while this response's calls ran: see allows. Nil before the
	// first request, which refuses every call, so a call can never run against
	// a set nobody sent.
	offered map[string]struct{}
	// candidates is the names the loop's own filters (chat-only, read-only,
	// core-only, dynamic activation) selected when the step's request was
	// assembled, before any waterfall. A name in it but not in offered was
	// withheld by a listener and stays refused; a name in neither that the same
	// filters select later became active during the response.
	candidates map[string]struct{}
}

// NewLoop builds a driver.
func NewLoop(cfg Config, b *bus.Bus, log *session.Log, registry *tools.Registry) (*Loop, error) {
	if b == nil {
		return nil, errors.New("agent: no bus")
	}
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
//
// It is the single owner of "this turn overflowed": it appends the log event
// and sets l.overflowed, which Run copies onto the Outcome at turn end. The flag
// lives on the Loop rather than being threaded through because compaction runs
// from a helper that has no Outcome in hand, and a return value would put the
// signal on a path every caller must remember to propagate — which is how a
// non-empty insufficient plan came to log the event and leave the flag false.
// The flag is set before the Append so a log that refuses the write still
// leaves the turn marked: the overflow happened either way. The Append's own
// failure is returned, since a record that silently failed to write is exactly
// the "must not happen silently" this function exists for.
func (l *Loop) reportOverflow(plan CompactionPlan) error {
	l.overflowed = true
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
	into.ArgumentsRepaired = into.ArgumentsRepaired || from.ArgumentsRepaired
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
		name := boundField(m.Name)
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Fprintf(&b, "  - %s: %s\n", name, boundField(m.Reason))
	}
	if l.hitOutputCap(response) {
		b.WriteString("\nThe response hit its output limit, which is what cut them off. ")
		b.WriteString("Make one call at a time, or pass smaller arguments.")
	} else {
		b.WriteString("\nSend the call again in full.")
	}

	for _, m := range response.Malformed {
		if _, err := l.log.Append(session.MalformedToolCall, session.MalformedCallData{
			ToolCallID: llm.CallID(boundField(string(m.ID))), Tool: boundField(m.Name), Reason: boundField(m.Reason),
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
	if l.cfg.ChatOnly {
		return nil
	}
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
	// CostNanoUSD is what the turn's priced steps cost, from Config.Price.
	CostNanoUSD int64
	// UnpricedSteps counts steps whose usage could not be priced. When it is
	// not zero, CostNanoUSD is a lower bound rather than the bill.
	UnpricedSteps int
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

	// Mutated reports that at least one tool that can change the world ran
	// without error. See TurnStopping.Mutated for why it is not the same
	// question as Wrote.
	Mutated bool
	// Wrote names the repository-relative paths this turn changed, as reported
	// by the handlers that changed them.
	Wrote []string
	// WroteTruncated is true when more paths were changed than Wrote lists.
	WroteTruncated bool
	// UnenumeratedEffects reports a mutation no handler named a path for. See
	// TurnStopping.UnenumeratedEffects.
	UnenumeratedEffects bool
	// Sensor is what the end-of-turn check concluded. It is on the outcome so
	// that no face can render a turn whose verification failed as a turn that
	// finished: a natural stop with text and no tool calls is otherwise
	// completely silent, and looks identical either way.
	Sensor SensorVerdict
	// Bounces counts the times the checkpoint sent this turn back for another
	// step.
	Bounces int
	// BouncesExhausted is true when the checkpoint was still asking for another
	// step at the point the cap ran out. The turn ended without the check being
	// satisfied, and saying so is the difference between a bounded gate and a
	// gate that quietly gave up.
	BouncesExhausted bool
}

// Run drives one turn to completion.
//
// Each step goes through the same phases, in order, and each phase is its own
// method: enterStep projects history and runs the pre-step waterfall, which is
// where compaction happens; step assembles the request and streams the
// response; absorbResponse folds that response into the outcome; dispatchCalls
// runs its tool calls; chargeStep spends the budget. A step with no tool calls
// ends at closeOrBounce, the terminal checkpoint.
func (l *Loop) Run(ctx context.Context, prompt llm.Message) (Outcome, error) {
	// Every exit leaves the log balanced. A failed turn used to return with
	// turn/start and step/start still open, so the projected stream carried a
	// turn.start with no turn.end and the next turn began inside it. The error
	// Run returns is untouched: closing is best-effort and only runs when the
	// normal path did not already end the turn.
	defer l.closeOpenBoundaries()
	defer l.announceTurnEnd(ctx)
	currentTurn, err := l.openTurn(ctx, prompt)
	if err != nil {
		return Outcome{}, err
	}

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

		messages, err := l.enterStep(ctx, currentTurn, step)
		if err != nil {
			return out, err
		}

		response, err := l.step(ctx, messages)
		if err != nil {
			return out, err
		}
		if err := l.absorbResponse(&out, step, response); err != nil {
			return out, err
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
			if err := l.retryNullResponse(&out, step, nullRetries); err != nil {
				return out, err
			}
			continue
		}

		calls := response.Message.ToolCalls()
		if len(calls) > 0 {
			if err := bus.Serial(l.bus, ctx, &ToolsDispatching{
				Turn: currentTurn, Step: step, Calls: len(calls),
			}); err != nil {
				return out, err
			}
		}
		tally, err := l.dispatchCalls(ctx, &out, calls)
		if err != nil {
			return out, err
		}
		l.chargeStep(&out, len(calls), tally)

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
			if err := l.endStep(); err != nil {
				return out, err
			}
			continue
		}

		if err := l.endStep(); err != nil {
			return out, err
		}

		if len(calls) == 0 {
			stop, err := l.closeOrBounce(ctx, &out, currentTurn, response)
			if err != nil {
				return out, err
			}
			if stop {
				break
			}
		}
	}

	out.ContextOverflowed = l.overflowed

	if err := l.endTurn(); err != nil {
		return out, err
	}
	return out, nil
}

// endStep closes the open step in the log, recording what the step spent. A
// step that never received a response — an early exit closing the boundaries —
// records zero usage marked NoResponse, because nothing was spent and, unlike
// an unpriced step, there was nothing to price.
func (l *Loop) endStep() error {
	// Taken and cleared before the append, so a refused append cannot leave
	// this step's figures behind for the next one. stepOpen stays true on a
	// refusal, as it always did: the step is still open in the log.
	spent := l.spent
	l.spent = session.StepEndData{}
	if _, err := l.log.Append(session.StepEnd, spent); err != nil {
		return err
	}
	l.stepOpen = false
	return nil
}

// endTurn closes the open turn in the log.
func (l *Loop) endTurn() error {
	if _, err := l.log.Append(session.TurnEnd, nil); err != nil {
		return err
	}
	l.turnOpen = false
	return nil
}

// closeOpenBoundaries ends whatever step and turn an early exit left open. A
// log that refuses the append is already failing the caller with its own
// error; there is nothing better to do with this one than leave it.
func (l *Loop) closeOpenBoundaries() {
	l.answerOpenCalls()
	if l.stepOpen {
		_ = l.endStep()
	}
	if l.turnOpen {
		_ = l.endTurn()
	}
}

// unwoundCallText is what a call that the turn left open is answered with.
const unwoundCallText = "interrupted: the turn ended before this call returned a result, " +
	"so it may not have run, or may have run in part"

// answerOpenCalls gives every call the turn left unanswered an error result,
// before the step closes and before anything persists.
//
// An assistant message whose tool_use has no tool_result is not a history a
// provider will accept, and the save that follows an abnormal exit used to
// write exactly that over the last good copy: the next turn then failed on its
// own request. The ways out that leave calls open are a listener's refusal at
// ToolsDispatching, a log that refuses an append mid-step, and a panic anywhere
// in the step. A cancelled turn mostly does not: its calls run to a result.
//
// Each call is answered once. A call leaves openCalls when its result is
// appended, so what this sees is only what is still open, and a second call to
// it finds nothing. A refused append stops it: the log is failing the caller
// already, and answering past a hole would leave the history out of order.
func (l *Loop) answerOpenCalls() {
	for len(l.openCalls) > 0 {
		id := l.openCalls[0]
		if _, err := l.log.Append(session.ToolResult, session.ToolResultData{
			ToolCallID: id, Text: unwoundCallText, IsError: true,
		}); err != nil {
			return
		}
		l.openCalls = l.openCalls[1:]
	}
}

// answered records that a call's result is in the log.
func (l *Loop) answered(id llm.CallID) {
	for i, open := range l.openCalls {
		if open == id {
			l.openCalls = append(l.openCalls[:i], l.openCalls[i+1:]...)
			return
		}
	}
}

// announceTurnEnd publishes TurnEnded for the turn TurnStarting announced, once.
func (l *Loop) announceTurnEnd(ctx context.Context) {
	turn := l.announced
	if turn == 0 {
		return
	}
	l.announced = 0
	// Listener errors are dropped by design; see TurnEnded.
	_ = bus.Serial(l.bus, context.WithoutCancel(ctx), &TurnEnded{Turn: turn})
}

// openTurn resets the per-turn state, opens the turn in the log and records
// the prompt that starts it. It returns the turn's number.
func (l *Loop) openTurn(ctx context.Context, prompt llm.Message) (int, error) {
	l.repeats = newRepeatTracker()
	l.progress = newProgressTracker(l.tools)
	l.overflowed = false
	l.openCalls = nil
	turnEvent, err := l.log.Append(session.TurnStart, nil)
	if err != nil {
		return 0, err
	}
	l.turnOpen = true
	currentTurn := turnEvent.Turn
	// Before the first step, so a listener recording the pre-turn tree records
	// it before anything can have changed. Its failure ends the turn: a
	// listener that could not establish a baseline would otherwise have its
	// silence read as "nothing had changed yet", which is the same shape as the
	// defect it exists to fix.
	//
	// Recorded before the dispatch, not after: a listener that started work and
	// then a later one that refused leaves the turn needing its end announced.
	l.announced = currentTurn
	if err := bus.Serial(l.bus, ctx, &TurnStarting{Turn: currentTurn}); err != nil {
		return 0, err
	}
	if l.cfg.SystemPrompt != "" && l.log.SystemPrompt() != l.cfg.SystemPrompt {
		if _, err := l.log.Append(session.SystemPrompt,
			session.SystemPromptData{Text: l.cfg.SystemPrompt}); err != nil {
			return 0, err
		}
	}
	if _, err := l.log.Append(session.UserMessage,
		session.MessageData{Message: prompt}); err != nil {
		return 0, err
	}

	l.effort = l.cfg.Effort
	return currentTurn, nil
}

// enterStep projects the history the step will send and runs the pre-step
// waterfall over it — which is where compaction, and the overflow record when
// compaction cannot make history fit, happen. It opens the step in the log.
func (l *Loop) enterStep(ctx context.Context, turn, step int) ([]llm.Message, error) {
	// History is always projected from the log. There is no other source.
	messages, err := l.log.DeriveMessages()
	if err != nil {
		return nil, err
	}

	pre, err := bus.Waterfall(l.bus, PreStep{
		Ctx: ctx, Turn: turn, Step: step, Messages: messages,
	})
	if err != nil {
		return nil, err
	}
	if pre.Reject != nil {
		return nil, fmt.Errorf("agent: step %d rejected: %w", step, pre.Reject)
	}
	messages = pre.Messages

	if _, err := l.log.Append(session.StepStart, nil); err != nil {
		return nil, err
	}
	// Per step, so a step that ends without a response cannot inherit the
	// previous step's figures. It reads as having had no response until
	// absorbResponse replaces it.
	l.spent = session.StepEndData{NoResponse: true}
	l.stepOpen = true
	return messages, nil
}

// price adds one step's charge to the outcome, or counts the step as unpriced.
// It returns the charge it added and whether the step was priced, which is what
// the step's own log record carries: the outcome and the record are fed by this
// one call, so they cannot disagree.
func (l *Loop) price(out *Outcome, usage llm.Usage) (int64, bool) {
	if l.cfg.Price == nil {
		out.UnpricedSteps++
		return 0, false
	}
	charge, err := l.cfg.Price(usage)
	if err != nil || charge < 0 || out.CostNanoUSD > math.MaxInt64-charge {
		out.UnpricedSteps++
		return 0, false
	}
	out.CostNanoUSD += charge
	return charge, true
}

// finiteRate returns v when it is a finite, non-negative rate and zero — "the
// server did not say" — otherwise.
func finiteRate(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}

// maxMalformedField bounds the model-supplied strings of a malformed call.
const maxMalformedField = 256

// boundField cuts s to at most maxMalformedField runes and says that it cut.
// A tool name or call id comes off the wire from the model's server and is not
// capped by the adapter; it is recorded in the log and repeated on the NDJSON
// stream, so an unbounded one becomes an unbounded record in both.
func boundField(s string) string {
	runes := []rune(s)
	if len(runes) <= maxMalformedField {
		return s
	}
	return string(runes[:maxMalformedField]) + "… (truncated)"
}

// absorbResponse folds one step's response into the outcome: its usage, its
// message, the decoding compensations it needed and whether it hit the output
// cap.
func (l *Loop) absorbResponse(out *Outcome, step int, response llm.Response) error {
	out.Steps = step
	out.StopReason = response.StopReason
	out.Usage.InputTokens += response.Usage.InputTokens
	out.Usage.OutputTokens += response.Usage.OutputTokens
	out.Usage.ReasoningTokens += response.Usage.ReasoningTokens
	out.Usage.CacheReadTokens += response.Usage.CacheReadTokens
	out.Usage.CacheWriteTokens += response.Usage.CacheWriteTokens
	charge, priced := l.price(out, response.Usage)
	// Throughput is a server-reported float, and the log encodes JSON, which
	// has no NaN or Inf: one such value from a server would fail the append and
	// with it an answered, paid step. The record carries a rate only when it is
	// a usable one.
	recorded := response.Usage
	recorded.OutputTokensPerSecond = finiteRate(recorded.OutputTokensPerSecond)
	recorded.PromptTokensPerSecond = finiteRate(recorded.PromptTokensPerSecond)
	l.spent = session.StepEndData{Usage: recorded, CostNanoUSD: charge, Priced: priced}
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
			ArgumentsRepaired:     response.Decoding.ArgumentsRepaired,
		}); err != nil {
			return err
		}
	}

	if l.hitOutputCap(response) {
		out.TruncatedByTokens = true
	}
	return nil
}

// retryNullResponse charges and records one retry of a response that carried
// nothing, and closes the step it spent. attempt is this retry's number within
// the turn.
func (l *Loop) retryNullResponse(out *Outcome, step, attempt int) error {
	// An empty response still consumed a provider attempt and one step.
	out.BudgetSpent++
	if _, err := l.log.Append(session.NullResponseRetried, session.NullResponseData{
		Step: step, Attempt: attempt, Of: maxNullRetries,
	}); err != nil {
		return err
	}
	// The step is over even though the turn continues: leaving its
	// start unclosed made every retried response dangle a step/start
	// in the durable log for the TUI projector, the invariant check,
	// and resume to trip over.
	return l.endStep()
}

// stepTally is what one step's tool calls amounted to, for charging the step.
type stepTally struct {
	// ran counts the calls that actually reached the pipeline, so a step
	// whose every call was refused is not mistaken for one that worked.
	ran        int
	progressed bool
	// circling records that this step refused at least one call because the
	// turn was going round — as a verbatim repeat or for making no
	// observable progress. It is per step rather than per call so a step
	// that refused three calls climbs one rung, not three.
	circling bool
}

// dispatchCalls runs one response's tool calls in order, recording each call,
// what the gate made of it, and its result.
func (l *Loop) dispatchCalls(ctx context.Context, out *Outcome, calls []llm.ToolCallBlock) (stepTally, error) {
	var tally stepTally
	for _, call := range calls {
		if _, err := l.log.Append(session.ToolCall, session.ToolCallData{
			ID: call.ID, Name: call.Name, Arguments: call.Arguments,
		}); err != nil {
			return tally, err
		}

		result := l.runCall(ctx, out, &tally, call)
		out.ToolCalls++

		if err := l.recordCall(out, call, result); err != nil {
			return tally, err
		}
	}
	return tally, nil
}

// runCall runs one tool call, or refuses it because the turn is going in
// circles, and returns the result the model will be shown.
func (l *Loop) runCall(ctx context.Context, out *Outcome, tally *stepTally, call llm.ToolCallBlock) tools.Result {
	var result tools.Result
	repeat := l.repeats.seen(call.Name, call.Arguments)
	switch {
	case !l.allows(call.Name):
		// The registry knows more tools than a step offers: a chat-only run
		// offers none, a read-only or core-only loop offers a filtered set, a
		// dynamic registry offers what is active. A provider that builds a
		// call from prose — the OpenAI-compatible adapter, the local fenced
		// JSON fallback — can name any of them, and the registry would run it.
		// What the model may run is what the request told it about, so the
		// refusal lives here, where every call passes, rather than in each
		// caller that builds a loop.
		//
		// Ahead of the repeat and stall cases so the model is told the true
		// reason, and it carries no Rule for the same reason theirs do: the
		// gate never saw the call, and a PolicyDenied no policy made would be
		// a lie in the evidence trail. It is a plain error result, which the
		// log records as the call's tool result.
		//
		// Circling only when the model names it again: the first mistake is a
		// mistake, but re-naming a tool it was just told it was not offered is
		// the turn going round, and effort escalation should see that.
		if repeat > 1 {
			tally.circling = true
		}
		result = tools.Result{IsError: true, Text: l.notOfferedText(call.Name)}

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
		tally.circling = true
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
		tally.circling = true
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
		tally.ran++
		if l.progress.observe(call.Name, result) {
			tally.progressed = true
		}
		// Recorded here rather than derived later, and only for calls
		// that actually ran: a refused call changed nothing, and a
		// repeat or stall refusal above never reached a handler.
		if l.progress.mutated(call.Name, result) {
			out.Mutated = true
			// A call that changed something and named no path leaves
			// effects nothing downstream can enumerate. Recorded here,
			// beside Mutated, because this is the only place that knows
			// both facts about the same call: by the time the paths are
			// merged into out.Wrote, a shell command that wrote nothing
			// is indistinguishable from a native write that reported.
			if len(result.Wrote) == 0 {
				out.UnenumeratedEffects = true
			}
		}
		out.Wrote, out.WroteTruncated = tools.MergeWrites(out.Wrote, out.WroteTruncated, result.Wrote)
	}
	return result
}

func schemaNames(schemas []llm.ToolSchema) map[string]struct{} {
	names := make(map[string]struct{}, len(schemas))
	for _, s := range schemas {
		names[s.Name] = struct{}{}
	}
	return names
}

// allows reports whether a call to name may run: the step's request offered it,
// or the registry activated it while this response's earlier calls ran.
//
// The second case keeps an activate-then-use response working: calls run in
// order, so by the time the second one is checked the first has changed what is
// active. It is read through schemas, the same filtered selection the request
// came from, so it cannot widen what the loop's own filters exclude — a
// chat-only run selects nothing, and a read-only or core-only loop selects only
// what it would have offered anyway. A name the request's candidates already
// held but a listener withheld stays refused.
func (l *Loop) allows(name string) bool {
	if l.offered == nil {
		return false
	}
	if _, ok := l.offered[name]; ok {
		return true
	}
	if _, withheld := l.candidates[name]; withheld {
		return false
	}
	for _, s := range l.schemas() {
		if s.Name == name {
			return true
		}
	}
	return false
}

// notOfferedText is what the model is told about a call it was not allowed to
// make. A tool that is registered but inactive gets the registry's own hint to
// activate it, but only when the activation tool was itself offered in this step
// and activating it would put it in the request (a read-only or core-only loop
// filters on more than activity, so there the hint could promise what activation
// cannot deliver); otherwise the hint would point at something the model cannot
// call.
func (l *Loop) notOfferedText(name string) string {
	if _, canActivate := l.offered[tools.ActivateToolName]; canActivate && !l.cfg.ReadOnly && !l.cfg.CoreToolsOnly && l.tools != nil {
		if note, ok := l.tools.InactiveNote(name); ok {
			return note
		}
	}
	note := ""
	if len(l.offered) == 0 {
		note = "; this request offered no tools"
	}
	return fmt.Sprintf("tool %q was not offered in this step, so it was not run. Call only the tools listed in this request%s.",
		boundField(name), note)
}

// recordCall records what the gate made of one call, any grant it applied,
// and the result the model is shown.
func (l *Loop) recordCall(out *Outcome, call llm.ToolCallBlock, result tools.Result) error {
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
			return err
		}
	case result.Blocked:
		out.Denied++
		if _, err := l.log.Append(session.PolicyDenied, session.DenialData{
			ToolCallID: call.ID, Tool: call.Name,
			Rule: result.Rule, Severity: result.Severity, Reason: result.Text,
		}); err != nil {
			return err
		}
	case result.Qualified():
		out.Qualified++
		if _, err := l.log.Append(session.PolicyQualified, session.QualificationData{
			ToolCallID: call.ID, Tool: call.Name,
			Rule: result.Rule, Severity: result.Severity, Reason: result.Text,
			Demoted: result.Demoted, Widened: result.Widened, Degraded: result.Degraded,
		}); err != nil {
			return err
		}
	}
	if result.GrantID != "" {
		if _, err := l.log.Append(session.GrantApplied, session.GrantData{
			GrantID: result.GrantID, GrantedBy: result.GrantedBy,
			Rule: result.Rule, Target: call.Name, Reason: result.Text,
		}); err != nil {
			return err
		}
	}

	if _, err := l.log.Append(session.ToolResult, session.ToolResultData{
		ToolCallID: call.ID, Text: result.Text, Content: result.Content, IsError: result.IsError,
	}); err != nil {
		return err
	}
	l.answered(call.ID)
	return nil
}

// chargeStep spends the step's cost from the turn's budget, and raises the
// reasoning tier when the step showed the turn going in circles.
func (l *Loop) chargeStep(out *Outcome, calls int, tally stepTally) {
	// The step is charged here, before any path that leaves the iteration,
	// so no route through the loop can take a step without paying for it.
	cost, noProgress := l.progress.endStep(calls, tally.ran, tally.progressed)
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
	if tally.circling && !tally.progressed {
		if next, ok := l.effortPlan.Next(l.effort); ok {
			l.effort = next
			out.EffortTo = next
			out.EffortRaised++
		}
	}
}

// closeOrBounce handles a step whose response made no tool calls: the turn
// looks finished, and the terminal checkpoint decides whether it is. It
// reports stop when the turn ends here, and false when the checkpoint sent it
// back for another step.
func (l *Loop) closeOrBounce(ctx context.Context, out *Outcome, turn int, response llm.Response) (bool, error) {
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
	//
	// Cancellation short-circuits it. A cancelled turn is the operator
	// saying stop, and a bounce fired on the way out would spend
	// another model call on work nobody is waiting for. Checked before
	// the listeners run rather than after, so a sensor that shells out
	// is not started either.
	if ctx.Err() != nil {
		return true, nil
	}
	checkpoint := &TurnStopping{
		Turn: turn, Steps: out.Steps, Response: response,
		Mutated: out.Mutated, Wrote: out.Wrote,
		WroteTruncated:      out.WroteTruncated,
		UnenumeratedEffects: out.UnenumeratedEffects,
		Truncated:           out.FinalTruncated, Empty: out.FinalEmpty,
		Bounce: out.Bounces, Circling: out.Repeated + out.Stalled,
	}
	if err := bus.Serial(l.bus, ctx, checkpoint); err != nil {
		// The listener failed. It did not ask for another step, and
		// re-sending an identical history is not a recovery — it is
		// the same request with the same answer, charged again. So the
		// turn closes, and closes loudly: a check that was owed and
		// could not be completed is degraded, never a pass.
		out.Sensor = SensorDegraded
		if _, appendErr := l.log.Append(session.VerifyReport, session.VerifyReportData{
			Verdict: string(SensorDegraded),
			Source:  "checkpoint",
			Paths:   out.Wrote,
			Degraded: []string{
				"the terminal checkpoint failed: " + err.Error(),
			},
			Bounce: out.Bounces,
		}); appendErr != nil {
			return false, appendErr
		}
		return true, nil
	}
	if checkpoint.Verdict != "" {
		out.Sensor = checkpoint.Verdict
	}
	inject := strings.TrimSpace(checkpoint.Inject)
	if inject == "" {
		return true, nil
	}
	// Asked again after the listeners rather than only before them. A
	// sensor that shells out is exactly the place a turn gets
	// cancelled, and honouring an inject decided before the cancel
	// would spend the model call the cancel was meant to stop.
	if ctx.Err() != nil {
		return true, nil
	}
	if out.Bounces >= MaxCheckpointBounces {
		// Still asking, out of budget. The turn ends, and the outcome
		// says the check was not satisfied — the alternative is a turn
		// that looks complete because the thing objecting to it ran out
		// of ways to object.
		out.BouncesExhausted = true
		return true, nil
	}
	// The loop appends, not the listener. A user-role message is the
	// only model-visible slot a natural stop leaves — there is no tool
	// call to answer, so an orphan tool result would break projection —
	// and the origin marks it as the harness's own words so no face
	// attributes it to the operator.
	if _, err := l.log.Append(session.UserMessage, session.MessageData{
		Message: llm.Message{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{llm.TextBlock{Text: inject}},
		},
		Origin: session.OriginHarness,
	}); err != nil {
		return false, err
	}
	out.Bounces++
	return false, nil
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
	request, err := l.assembleRequest(ctx, messages)
	if err != nil {
		return llm.Response{}, err
	}
	return l.streamResponse(ctx, request)
}

// assembleRequest builds the request one step sends, shaped by the llm/request
// waterfall and checked before anything goes on the wire.
func (l *Loop) assembleRequest(ctx context.Context, messages []llm.Message) (llm.Request, error) {
	schemas := l.schemas()

	// Provider-private state that this target cannot replay is stripped here,
	// and the removal is logged. Cross-provider handoff is explicitly lossy.
	prepared, drops := llm.PrepareHistoryFor(messages, l.cfg.Provider, l.cfg.Model)
	if len(drops) > 0 {
		if _, err := l.log.Append(session.ProvenanceDropped,
			session.DropData{Drops: drops}); err != nil {
			return llm.Request{}, err
		}
	}

	request := l.newRequest(prepared, schemas, l.effort)

	shaped, err := bus.Waterfall(l.bus, LLMRequest{Ctx: ctx, Request: request})
	if err != nil {
		return llm.Request{}, err
	}
	request = shaped.Request

	// The offered set is read off the request as finally shaped, not off the
	// schemas computed above: a listener that narrowed the tools narrowed what
	// the model may run, and the set enforced must be the set that goes out.
	l.offered = schemaNames(request.Tools)
	l.candidates = schemaNames(schemas)

	// Assembly-time capability check, when a registry is available: an
	// impossible request fails here rather than as a 400 mid-turn.
	if l.cfg.Registry != nil {
		if _, _, err := l.cfg.Registry.Resolve(l.cfg.Provider.Name(), request); err != nil {
			return llm.Request{}, err
		}
	}

	if l.cfg.AssertInvariant {
		if err := session.AssertModelVisible(l.log, request.Messages); err != nil {
			return llm.Request{}, err
		}
	}
	return request, nil
}

// newRequest is the one place a request's fields are assembled from the
// loop's configuration.
func (l *Loop) newRequest(messages []llm.Message, schemas []llm.ToolSchema, effort string) llm.Request {
	return llm.Request{
		Model:     l.cfg.Model,
		System:    l.cfg.SystemPrompt,
		Messages:  messages,
		Tools:     schemas,
		MaxTokens: l.cfg.MaxTokens,
		Effort:    effort,
	}
}

// Draft is the request a turn opened with user would send first, before the
// waterfalls that reshape it: the same model, system prompt, tool schemas and
// output bound. It runs nothing and records nothing.
//
// It exists so a request can be sized before its turn starts — a fan-out is
// admitted against a spend cap on what its children's first requests will
// reserve, and that has to be known while the answer can still be "start
// none".
func (l *Loop) Draft(user llm.Message) llm.Request {
	return l.newRequest([]llm.Message{user}, l.schemas(), l.cfg.Effort)
}

// streamResponse sends the request, records every chunk as it arrives and
// then the assistant message the stream assembled.
func (l *Loop) streamResponse(ctx context.Context, request llm.Request) (llm.Response, error) {
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
	l.openCalls = l.openCalls[:0]
	for _, call := range response.Message.ToolCalls() {
		l.openCalls = append(l.openCalls, call.ID)
	}
	return response, nil
}
