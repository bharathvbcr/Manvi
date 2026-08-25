package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"manvi/agent"
	"manvi/core/bus"
	"manvi/credentials"
	"manvi/flags"
	"manvi/llm"
	"manvi/session"
	"manvi/tools"
	"manvi/ui"
)

// runHeadless drives one turn with no terminal attached.
//
// The TUI was the only thing that could drive the agent loop, which made the
// harness unusable from a script, a CI job, or a benchmark — the three places
// you most want to point it. Everything below the surface is the same code the
// TUI runs: the same tool pipeline, the same gate, the same loop, the same
// session log. That is deliberate and is the whole point: a headless mode with
// its own quietly different execution path would be a second harness whose
// results say nothing about the first.
//
// Two things it does that the TUI does not have to. It has no operator to ask,
// so a soft block is refused rather than escalated — nativeToolsWith(reg, nil)
// is that decision, and it is the pre-existing behaviour of every non-TUI
// caller rather than a new degraded mode. And it must answer with an exit
// status, because a caller that cannot tell a finished turn from an abandoned
// one will treat both as success.
const runUsage = `manvi run — drive one turn without a terminal

Usage:
  manvi run [-p] PROMPT            Run one turn and print the final message
  echo PROMPT | manvi run          Read the prompt from stdin
  manvi run -c [-p] PROMPT         Continue the most recent session
  manvi run --resume ID [-p] TEXT  Continue a named session (a unique prefix will do)

Options:
  -p, --prompt TEXT    The prompt. Also positional, or on stdin.
  -c, --continue       Resume the most recent saved session instead of starting one.
  --resume ID          Resume that session. A unique prefix of the id is enough.
  --task TASK_ID       Check this task out first, so writes run under its lease
                       and its planned scope.
  --max-steps N        Step ceiling for the turn (default 500, or MANVI_MAX_STEPS
                       / the max_steps setting).
                       Spent as a budget rather than counted: a step that made
                       observable progress costs one, a step whose tool calls
                       changed nothing costs three. No step costs less than one,
                       so N is still a hard ceiling on steps — but a turn going
                       in circles reaches it sooner, ending near N/3 rather than
                       at N. The summary says which of the two happened.
  --timeout DURATION   Wall-clock bound on the whole turn, e.g. 10m (default 30m).
  --json               Emit the event stream as NDJSON on stdout instead of prose.
  --quiet              Print only the model's final message.

Sessions:
  Every run records its session log under <state dir>/sessions and prints the id.
  Resuming replays that log into the same projection a fresh turn is built from,
  so a continued turn costs one prompt's prefill instead of the whole history's.
  A session file that cannot be read is refused rather than resumed empty, and
  the oldest turns are dropped once a session outgrows its size bound — both are
  said out loud.

Exit status:
  0  the turn finished on its own
  1  the run failed
  2  the step ceiling ended the turn — the work is not complete
  3  the output cap cut the final answer off mid-sentence. Look at the answer
     before acting: one that was going somewhere wants a bigger
     llm.local.max_output_tokens, one that was repeating itself was being held
     in check by that cap. Kept apart from 2 because the fix differs.
  4  the turn ended without an answer: the last response carried no text and
     no tool call, so nothing was completed. Neither ceiling was reached, so
     raising one fixes nothing — the model stopped having anything to say.
  5  the turn ended without finishing: there is an answer, and the stream did
     not end on a stop reason that means the work was done — the connection
     dropped mid-response, the server sent a status this adapter does not map,
     or the model refused. Read the answer before acting on it; stderr says
     which of the three it was. Retrying helps a dropped connection and does
     not help a refusal.
`

type runOptions struct {
	prompt   string
	task     string
	maxSteps int
	timeout  time.Duration
	asJSON   bool
	quiet    bool
	// resume names a session to continue, by full id or unique prefix.
	resume string
	// continueLast asks for the most recently saved session.
	continueLast bool
}

func runHeadless(out, notes io.Writer, reg *flags.Registry, args []string) (err error) {
	opts, err := parseRunArgs(reg, args, os.Stdin)
	if err != nil {
		return err
	}

	// Resolved before anything is built, so a mistyped --resume costs nothing
	// and a corrupt session is reported while the operator can still choose a
	// different one — rather than after a provider handshake and a lease.
	store := session.NewStore(sessionsDir())
	sessionID, log, err := openSession(notes, store, opts)
	if err != nil {
		return err
	}
	// Saved on the way out, including when the turn failed or ran out of
	// steps: what the model saw is what a retry has to build on, and a turn
	// whose history was thrown away because it ended badly is the turn a
	// caller most wants to continue.
	appendedFrom := log.Len()
	defer func() {
		if log.Len() <= appendedFrom {
			return
		}
		if saveErr := saveSession(notes, store, sessionID, log); saveErr != nil {
			// Reported, and it takes over the exit status. Status 2 tells a
			// caller "resume this"; if the session did not persist, that is
			// advice the caller cannot act on, and a failure it must see.
			fmt.Fprintf(notes, "manvi: the session did not save: %v\n", saveErr)
			// errNoAnswer belongs here for the same reason as the other two:
			// exit 4's advice is "re-run it", and a caller cannot act on that
			// when the session it would resume from was never written. It was
			// added as a status without being wired in here.
			if err == nil || errors.Is(err, errTruncated) ||
				errors.Is(err, errOutputCap) || errors.Is(err, errNoAnswer) {
				err = saveErr
			}
		}
	}()

	// The pipeline is built with no approver: there is nobody to ask. A soft
	// block is therefore a refusal the model must route around or report, which
	// is exactly what an unattended run should do rather than blocking forever
	// on a prompt no one will answer.
	native, pipeline, err := nativeToolsWith(reg, nil)
	if err != nil {
		return err
	}

	providerName, _, err := reg.String(flags.LLMDefaultProvider)
	if err != nil {
		return err
	}
	resolver := credentials.NewResolver()
	provider, err := buildProvider(providerName, reg, resolver, notes)
	if err != nil {
		return err
	}
	registry := llm.NewRegistry()
	if err := registry.Register(provider); err != nil {
		return err
	}

	// Bounded before the model is contacted, so a wedged server cannot hold the
	// process open indefinitely, and interruptible, so Ctrl-C in a terminal and
	// SIGTERM from a job runner both end the turn through the same path that
	// releases the lease.
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()

	model, _, err := resolveModelFor(ctx, providerName, provider, reg)
	if err != nil {
		return err
	}
	effort, _, err := reg.String(flags.LLMEffort)
	if err != nil {
		return err
	}
	effortCeiling, _, err := reg.String(flags.LLMEffortCeiling)
	if err != nil {
		return err
	}
	if effort != "" || effortCeiling != "" {
		// Checked here rather than at request time for the reason the TUI
		// checks it at attach: an effort the model does not accept is a
		// configuration mistake, and found now it names the setting instead of
		// arriving as a refused request after the run has started.
		if capability, ok := provider.Capability(model); ok {
			if verr := capability.Validate(llm.Request{Model: model, Effort: effort}); verr != nil {
				return verr
			}
			// The same plan the loop builds, from the same function, so a
			// ceiling accepted here cannot be one the turn then refuses.
			if _, verr := agent.PlanEffort(capability, effort, effortCeiling); verr != nil {
				return fmt.Errorf("%s=%s: %w", flags.EnvKey(flags.LLMEffortCeiling), effortCeiling, verr)
			}
		}
	}

	scrubber := credentials.NewScrubber()
	scrubber.WatchAll(resolver)

	var sink ui.Sink
	switch {
	case opts.asJSON:
		sink = ui.NewJSONSink(out, scrubber)
	case opts.quiet:
		sink = ui.SinkFunc(func(ui.Event) {})
	default:
		sink = ui.NewRenderer(out, scrubber)
	}

	// The face reads the log rather than a stream emitted beside it, so what is
	// printed is by construction what the model saw. Restored events are not
	// replayed through it: the caller asked to continue a session, not to have
	// the whole of it reprinted.
	log.Observe(ui.ProjectSink(sink))

	posture, _, _ := reg.String(flags.HarnessPosture)
	sink.Emit(ui.Event{
		Kind: ui.KindSessionStart, At: time.Now().UTC(),
		Posture: posture, Model: providerName + "/" + model,
	})
	if weak := reg.Weakened(); len(weak) > 0 {
		names := make([]string, 0, len(weak))
		for _, v := range weak {
			names = append(names, fmt.Sprintf("%s=%s (%s)", v.Key, v.Raw, v.Origin))
		}
		sink.Emit(ui.Event{
			Kind: ui.KindNotice, At: time.Now().UTC(),
			Text: "results produced under these settings are not strict", Weakened: names,
		})
	}

	// A lease taken here is released here, on a context that outlives the one
	// the turn is cancelled with. A lease that survives the process is a task
	// no other builder can take until its TTL lapses, which is the failure a
	// crash-looping CI job turns into a stuck queue.
	if opts.task != "" {
		if err := checkoutFor(ctx, pipeline, opts.task); err != nil {
			return err
		}
	}
	defer func() {
		if native.Session().TaskID == "" {
			return
		}
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer releaseCancel()
		result := pipeline.Run(releaseCtx, tools.Call{
			ID: "shutdown", Name: "devcouncil_release_task", Arguments: []byte(`{}`),
		})
		if result.IsError {
			fmt.Fprintf(notes, "manvi: the lease did not release cleanly: %s\n", result.Text)
		}
	}()

	coreOnly := coreToolsOnly(reg, provider.Name())
	dynamic := dynamicToolsEnabled(reg, provider.Name())
	if dynamic {
		pipeline.EnableDynamic()
	}
	systemPrompt := assemblePrompt(reg, PromptOptions{
		Provider:         provider.Name(),
		TaskToolsOffered: taskToolsOffered(pipeline, coreOnly),
		DynamicTools:     dynamic,
		ActiveGroups:     pipeline.ActiveGroups(),
	})

	// Give devcouncil_spawn_subagents the ability to actually run a child.
	// Without this the tool refuses, which is honest but is not the capability
	// its schema advertises. The child inherits this turn's provider, model and
	// tool surface, minus the whole sub-agent dispatch group — not merely the
	// one tool that dispatched. devcouncil_invoke_subagent reaches this same
	// runner, so excluding only the spawn tool left the depth bound one name
	// wide; see the floor predicate in subagent.go.
	meter := &subAgentMeter{}
	if runner := subRunners[pipeline]; runner != nil {
		runner.attach(subAgentConfig{
			provider:        provider,
			models:          registry,
			model:           model,
			effort:          effort,
			effortCeiling:   effortCeiling,
			registry:        pipeline,
			native:          native,
			meter:           meter,
			coreToolsOnly:   coreOnly,
			assertInvariant: assertInvariant(reg),
			systemPrompt:    systemPrompt,
			sink:            sink,
		})
	}

	loop, err := agent.NewLoop(agent.Config{
		Provider:        provider,
		Registry:        registry,
		Model:           model,
		Effort:          effort,
		EffortCeiling:   effortCeiling,
		SystemPrompt:    systemPrompt,
		MaxSteps:        opts.maxSteps,
		MaxTokens:       0,
		CoreToolsOnly:   coreOnly,
		AssertInvariant: assertInvariant(reg),
	}, bus.New(), log, pipeline)
	if err != nil {
		return err
	}

	outcome, err := loop.Run(ctx, llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{llm.TextBlock{Text: opts.prompt}},
	})
	if err != nil {
		return fmt.Errorf("the turn failed: %s", scrubber.Clean(err.Error()))
	}

	// In quiet mode this line is the whole deliverable — the sink is a no-op
	// and nothing else reaches the caller — so a write that did not land is the
	// answer lost. Held rather than acted on here because the run still has a
	// usage event and its outcome notices to emit; it is folded into the status
	// below, alongside the same failure happening to a face.
	var answerErr error
	if opts.quiet {
		_, answerErr = fmt.Fprintln(out, strings.TrimSpace(scrubber.Clean(textOf(outcome.Final))))
	}
	// The dispatching agent's spend plus every child's. Reporting the first
	// alone was reporting a fraction as a whole: on a fan-out the children are
	// where the work happens, so the figure a benchmark records was the small
	// part of the bill.
	delegated := meter.Total()
	sink.Emit(ui.Event{
		Kind: ui.KindUsage, At: time.Now().UTC(),
		InputTokens:  outcome.Usage.InputTokens + delegated.InputTokens,
		OutputTokens: outcome.Usage.OutputTokens + delegated.OutputTokens,
	})
	if delegated.Any() {
		fmt.Fprintf(notes, "manvi: of that, %d in / %d out was spent by sub-agents\n",
			delegated.InputTokens, delegated.OutputTokens)
	}
	// One owner decides what a finished turn has to say; this face renders it.
	// The exit-status-bearing ones are mirrored to notes as well, because a
	// caller reading stderr for the reason behind a non-zero status should find
	// it there rather than having to parse the event stream.
	for _, n := range outcomeNotices(outcome, opts.maxSteps) {
		sink.Emit(ui.Event{
			Kind: ui.KindReport, At: time.Now().UTC(),
			Text: n.Text, Degraded: n.Degraded,
		})
		if len(n.Degraded) > 0 {
			fmt.Fprintf(notes, "manvi: %s\n", n.Text)
		}
	}
	return outputStatus(outcomeStatus(outcome), notes, scrubber.Clean, faceFailure(sink), answerErr)
}

// faceFailure asks a face whether it failed to write, for the faces that can
// answer. Both of them can; a caller does not have to know which one a run was
// given, and a sink that cannot answer is treated as having nothing to report
// rather than as having succeeded — those are the same here only because the
// one sink that cannot answer, the quiet no-op, writes nothing at all.
func faceFailure(sink ui.Sink) error {
	if answerer, ok := sink.(interface{ Err() error }); ok {
		return answerer.Err()
	}
	return nil
}

// outputStatus is the one owner of what a run's output failing to land does to
// that run's exit status.
//
// It is the JSON sink's own rule one rung up. Inside a line, an event that
// would not marshal is answered with a line that says so; here the thing that
// failed is the writing itself, so there is no line left to say it in and it
// has to come back as the status. A caller holding a short transcript, or an
// empty answer from --quiet, is holding an account of the run with an unmarked
// end, and must not be handed the status a whole one gets.
//
// causes are taken in order and the first that is set wins; they are the ways
// the same thing goes wrong — a face that could not write its lines, and the
// direct write of the quiet answer, which goes to no face at all.
//
// The turn's own sentinel wins over all of them when there is one: those map to
// specific exit statuses and each says something more specific about the same
// run. It does not get to bury this, so the fact goes to notes instead — and
// only in that case, so the caller is told exactly once.
// errOutputLost is what an incomplete-output failure is built from, so that a
// caller can recognise one it has already been told about. Both rungs of this
// check — a command that knows exactly which of its writes was lost, and the
// composition root backstopping every write it does not know about — report
// through outputStatus, and without this the two would say the same thing to
// stderr twice for one failure.
var errOutputLost = errors.New("the run's output is incomplete")

func outputStatus(status error, notes io.Writer, clean func(string) string, causes ...error) error {
	var cause error
	for _, c := range causes {
		if c != nil {
			cause = c
			break
		}
	}
	if cause == nil {
		return status
	}
	message := cause.Error()
	if clean != nil {
		message = clean(message)
	}
	failure := fmt.Errorf("%w: %s", errOutputLost, message)
	if status == nil {
		return failure
	}
	// Already reported, in either of the two ways a caller can already know.
	// errOutputLost means an inner rung, which knew more about which write was
	// lost than this one does, has said so. Carrying the cause itself means the
	// command returned the very write error this rung is reporting — `manvi
	// logo` does, because writing the mark is the whole of what it does — and
	// the status will be printed with that cause in it. Either way, saying it
	// again is one failure and two lines, and the second reads as a second
	// failure.
	told := errors.Is(status, errOutputLost) || errors.Is(status, cause)
	if notes != nil && !told {
		fmt.Fprintf(notes, "manvi: %v\n", failure)
	}
	return status
}

// outcomeStatus is the one owner of "what did this turn actually end as", as an
// error the exit-status switch in main maps.
//
// It is a function rather than four inline checks because the checks and the
// notices in outcomeNotices are two renderings of the same judgement, and they
// had already drifted once: every notice below marked itself Degraded, while
// only three of the conditions carried an exit status and the fourth exited 0.
// A turn whose stream died mid-answer reported "the answer above may be
// incomplete and nothing else will say so" on stderr and told a script it had
// succeeded.
//
// Order is severity, not chronology: the most specific account of how the turn
// failed to finish wins, because that is the one that tells a caller what to
// change.
func outcomeStatus(outcome agent.Outcome) error {
	if outcome.FinalTruncated {
		return errOutputCap
	}
	if outcome.TruncatedBySteps {
		return errTruncated
	}
	// Last of the three, because the other two say something more specific
	// about the same failure. On its own it means the turn simply stopped
	// having anything to say, which a caller must not read as success.
	if outcome.FinalEmpty {
		return errNoAnswer
	}
	// After the three above, because each of those says something more
	// specific about how a turn ended badly. What is left is a turn that
	// reached its last response and that response did not end in a way this
	// harness recognises as finished: the connection died mid-answer, the
	// server sent a status not in the adapter's mapping, or the model refused.
	//
	// It exited 0 until now, which is the same status a completed turn gets.
	// outcomeNotices has always said so in prose — its own text ends "and
	// nothing else will say so" — but prose on stderr is not what a benchmark
	// or a CI step reads. This harness's cardinal rule is that a check that did
	// not run must never report the same result as one that ran and passed, and
	// an exit status is a check's result.
	if outcome.StopReason == llm.StopOther || outcome.StopReason == llm.StopRefusal {
		return errUnfinished
	}
	return nil
}

// errTruncated is the sentinel that maps to exit status 2.
var errTruncated = errors.New("the turn was truncated by the step ceiling")

// errOutputCap is the sentinel that maps to exit status 3. It is kept apart
// from errTruncated because the two ask the caller for different things.
var errOutputCap = errors.New("the final answer was truncated by the output cap")

// errNoAnswer is the sentinel that maps to exit status 4: the turn ended with
// no answer and no tool call. Separate from the two above because neither the
// step ceiling nor the output cap was reached — raising either fixes nothing.
var errNoAnswer = errors.New("the turn ended without an answer")

// errUnfinished is the sentinel that maps to exit status 5: the turn produced
// an answer and did not end on a stop reason that means it was finished.
//
// Separate from 4, which is "there is no answer to read", and from 1, which is
// "the run failed and there is nothing to act on". This one says an answer
// exists and may be a fragment: look at it before acting on it. Retrying is
// usually right for a dropped connection and usually wrong for a refusal, and
// the notice on stderr names which of the two it was.
var errUnfinished = errors.New("the turn ended without finishing")

// openSession returns the id and the log this run appends to.
//
// A resumed log is rebuilt from the recorded events and projected by
// DeriveMessages exactly as a fresh one is. There is no second way to turn a
// file into model history, which is what keeps the invariant this harness
// claims — that what the model saw is what the log says — true across a resume
// as well as within one turn.
func openSession(notes io.Writer, store *session.Store, opts runOptions) (string, *session.Log, error) {
	ref := opts.resume
	if opts.continueLast {
		latest, err := store.Latest()
		if errors.Is(err, session.ErrNoSessions) {
			// Not a fresh run. The caller asked to continue something, and
			// starting over instead would look identical to having continued
			// a session that had nothing in it.
			return "", nil, fmt.Errorf(
				"--continue: there is no saved session in %s to continue; "+
					"run without it to start one", store.Dir())
		}
		if err != nil {
			return "", nil, err
		}
		ref = latest
	}
	if ref == "" {
		id, err := session.NewID()
		if err != nil {
			return "", nil, err
		}
		fmt.Fprintf(notes, "manvi: session %s\n", id)
		return id, session.NewLog(), nil
	}

	// The error names the flag the caller actually typed. --continue reaches
	// here with an id it resolved itself, and blaming --resume for it would
	// send an operator to look at an argument they did not pass.
	flag := "--resume"
	if opts.continueLast {
		flag = "--continue"
	}
	id, err := store.Resolve(ref)
	if err != nil {
		var ambiguous *session.AmbiguousError
		if errors.As(err, &ambiguous) {
			return "", nil, fmt.Errorf(
				"%s %s matches %d sessions — name one of them: %s",
				flag, ambiguous.Ref, len(ambiguous.Candidates), strings.Join(ambiguous.Candidates, " "))
		}
		var missing *session.NotFoundError
		if errors.As(err, &missing) {
			return "", nil, fmt.Errorf("%s %s: no session in %s has that id or prefix",
				flag, missing.Ref, store.Dir())
		}
		return "", nil, err
	}

	log, report, err := store.Load(id)
	if err != nil {
		return "", nil, err
	}
	fmt.Fprintf(notes, "manvi: resumed session %s — %d event(s) across %d turn(s), last written %s\n",
		report.ID, report.Events, report.Turns, report.UpdatedAt.Format(time.RFC3339))
	if report.TrimmedTurns > 0 {
		fmt.Fprintf(notes,
			"manvi: this session's %d oldest turn(s) were dropped to keep it within its size bound — "+
				"the history below that point is gone\n", report.TrimmedTurns)
	}
	if report.Oversize {
		fmt.Fprintf(notes,
			"manvi: this session's most recent turn alone exceeds the size bound; it was kept whole\n")
	}
	return id, log, nil
}

// saveSession writes the log back and says what the retention bound cost.
func saveSession(notes io.Writer, store *session.Store, id string, log *session.Log) error {
	report, err := store.Save(id, log)
	if err != nil {
		return err
	}
	if report.TrimmedTurns > 0 {
		fmt.Fprintf(notes,
			"manvi: %d oldest turn(s) were dropped from session %s to keep it within its size bound\n",
			report.TrimmedTurns, id)
	}
	if report.Oversize {
		fmt.Fprintf(notes,
			"manvi: session %s exceeds its size bound with a single turn; it was saved whole\n", id)
	}
	fmt.Fprintf(notes, "manvi: session %s saved (%d event(s)) — continue it with: manvi run --resume %s\n",
		id, report.Events, id)
	return nil
}

func checkoutFor(ctx context.Context, pipeline *tools.Registry, task string) error {
	result := pipeline.Run(ctx, tools.Call{
		ID: "checkout", Name: "devcouncil_checkout_task",
		Arguments: []byte(fmt.Sprintf(`{"task_id":%q}`, task)),
	})
	if result.IsError {
		return fmt.Errorf("could not check out %s: %s", task, result.Text)
	}
	return nil
}

func textOf(msg llm.Message) string {
	var b strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.(llm.TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// parseRunArgs reads the prompt and options.
//
// The prompt may arrive three ways because the three callers differ: a human
// types it positionally, a script passes -p, and a pipeline pipes it. An empty
// prompt is refused rather than sent, since a turn with nothing to do still
// costs a full prompt's prefill.
// The registry is a parameter because the step ceiling is a registry setting:
// --max-steps overrides it for one invocation, so the resolved value has to be
// in hand before the arguments are read.
func parseRunArgs(reg *flags.Registry, args []string, stdin *os.File) (runOptions, error) {
	opts := runOptions{maxSteps: maxSteps(reg), timeout: 30 * time.Minute}

	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", arg)
			}
			i++
			return args[i], nil
		}
		var err error
		switch {
		case arg == "-h" || arg == "--help":
			return runOptions{}, errUsage
		case arg == "--json":
			opts.asJSON = true
		case arg == "--quiet":
			opts.quiet = true
		case arg == "-p" || arg == "--prompt":
			opts.prompt, err = next()
		case arg == "-c" || arg == "--continue":
			opts.continueLast = true
		case arg == "--resume":
			opts.resume, err = next()
		case arg == "--task":
			opts.task, err = next()
		case arg == "--max-steps":
			var raw string
			if raw, err = next(); err == nil {
				n, convErr := strconv.Atoi(raw)
				if convErr != nil || n <= 0 {
					return runOptions{}, fmt.Errorf("--max-steps must be a positive integer, got %q", raw)
				}
				opts.maxSteps = n
			}
		case arg == "--timeout":
			var raw string
			if raw, err = next(); err == nil {
				d, convErr := time.ParseDuration(raw)
				if convErr != nil || d <= 0 {
					return runOptions{}, fmt.Errorf("--timeout must be a positive duration like 10m, got %q", raw)
				}
				opts.timeout = d
			}
		case strings.HasPrefix(arg, "-"):
			// Refused rather than ignored: a mistyped flag that is silently
			// dropped runs the turn with settings the caller did not choose.
			return runOptions{}, fmt.Errorf("unknown option %q (see: manvi run --help)", arg)
		default:
			positional = append(positional, arg)
		}
		if err != nil {
			return runOptions{}, err
		}
	}

	if opts.prompt == "" && len(positional) > 0 {
		opts.prompt = strings.Join(positional, " ")
	}
	if opts.prompt == "" {
		piped, err := readPipedPrompt(stdin)
		if err != nil {
			return runOptions{}, err
		}
		opts.prompt = piped
	}
	if strings.TrimSpace(opts.prompt) == "" {
		return runOptions{}, errors.New("manvi run needs a prompt: pass it positionally, with -p, or on stdin")
	}
	// --json and --quiet ask for different single things on stdout, and
	// honouring both would interleave a final message into the NDJSON stream
	// that a parser would then reject.
	if opts.asJSON && opts.quiet {
		return runOptions{}, errors.New("--json and --quiet both claim stdout; choose one")
	}
	// Both name a session to resume, and they can name different ones.
	// Choosing between them silently would resume a conversation the caller
	// did not ask for and report it as success.
	if opts.continueLast && opts.resume != "" {
		return runOptions{}, errors.New("--continue and --resume both choose a session; pass one")
	}
	if opts.resume != "" && strings.TrimSpace(opts.resume) == "" {
		return runOptions{}, errors.New("--resume needs a session id, or a unique prefix of one")
	}
	return opts, nil
}

var errUsage = errors.New("usage")

// readPipedPrompt reads stdin only when it is not a terminal. Reading an
// attached terminal would hang waiting for input the caller never intended to
// give, which reads as the harness having frozen.
func readPipedPrompt(stdin *os.File) (string, error) {
	if stdin == nil {
		return "", nil
	}
	info, err := stdin.Stat()
	if err != nil {
		return "", nil
	}
	if info.Mode()&os.ModeCharDevice != 0 {
		return "", nil
	}
	body, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return "", fmt.Errorf("reading the prompt from stdin: %w", err)
	}
	return string(body), nil
}
