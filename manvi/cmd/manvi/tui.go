package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"manvi/llm/local"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"manvi/agent"
	"manvi/core/bus"
	"manvi/credentials"
	"manvi/dc/devmap"
	"manvi/devcouncil"
	"manvi/flags"
	"manvi/gate"
	"manvi/llm"
	"manvi/prompt"
	"manvi/repomap"
	"manvi/session"
	"manvi/tools"
	"manvi/ui"
	"manvi/ui/term"
	"manvi/ui/tui"
)

// runTUI starts the full-screen face.
func runTUI(reg *flags.Registry, args []string) error {
	host := &harnessHost{
		reg:          reg,
		sessions:     map[string]*tuiSession{},
		firstSession: make(chan string, 1),
	}

	// The credential backstop, armed before the first frame. `manvi run`,
	// `manvi watch` and `manvi probe` each wired one and this face wired none,
	// so the one face that is a live terminal — the one where a leaked key is
	// read by a human and scrolled into their scrollback — was the only one
	// printing provider error bodies unredacted.
	_, scrubber := host.creds()

	runner, err := tui.New(tui.Config{
		Host:         host,
		Title:        "manvi",
		FirstSession: true,
		Scrubber:     scrubber,
	})
	if errors.Is(err, term.ErrNotATerminal) {
		// Named rather than generic. "not a terminal" sends an operator looking
		// for a bug; naming the face that does work sends them somewhere.
		return errors.New("manvi tui needs an interactive terminal on both stdin and stdout; " +
			"use 'manvi watch' for a pipe, or 'manvi watch --json' for CI")
	}
	if err != nil {
		return err
	}
	host.runner = runner

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Started before the loop and cancelled with it. The index is the one piece
	// of startup whose cost scales with the repository rather than with the
	// command, so it runs beside the UI instead of in front of it.
	go host.refreshIndexInBackground(ctx)

	runErr := runner.Run(ctx)

	// Sessions hold leases. A lease that outlives the process is a task no
	// other builder can take until its TTL lapses, so releasing is not
	// best-effort cleanup — it is the last required step, and it runs on a
	// fresh context because the one the UI used is being cancelled.
	releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer releaseCancel()
	if err := host.releaseAll(releaseCtx); err != nil {
		fmt.Fprintf(os.Stderr, "manvi: some sessions did not release cleanly: %v\n", err)
	}
	return runErr
}

// tuiSession is one session's own harness state.
//
// Each gets its own log, gate, and tool pipeline rather than sharing one. A
// shared log would interleave two turns' records into a single history, and
// that history is the only thing a model request is projected from.
type tuiSession struct {
	id    string
	title string

	log      *session.Log
	gate     *gate.Gate
	native   *devcouncil.Registry
	pipeline *tools.Registry
	// subMeter totals what this session's sub-agents have spent, so the usage
	// this face reports is the whole bill rather than the dispatching agent's
	// share of it.
	subMeter *subAgentMeter

	// attachMu guards the resolved-provider block below.
	//
	// One turn per session means a session's own goroutine is the only writer
	// during normal work. A settings change is the exception: `/flags set
	// llm.provider.default` drops the cached provider so the next turn resolves
	// it again, and that drop runs on the command's goroutine while another
	// session's turn may be starting on its own. The window is small and the
	// consequence is not — a torn read here hands the agent loop a provider
	// with someone else's model name.
	attachMu sync.Mutex
	provider llm.Provider
	model    string
	effort   string
	// effortCeiling is how far a turn in this session may raise effort when it
	// stops making progress. Resolved at attach beside effort, because the two
	// are only serviceable together and a ceiling found to be wrong after a
	// prompt has been typed reads as the harness losing the turn.
	effortCeiling string
	registry      *llm.Registry
}

// attached is the resolved provider block, taken as one consistent snapshot.
type attached struct {
	provider      llm.Provider
	registry      *llm.Registry
	model         string
	effort        string
	effortCeiling string
}

// attachment reads the block under the lock. A caller that read the five fields
// separately could pick up a provider from before a settings change and a model
// from after it.
func (s *tuiSession) attachment() attached {
	s.attachMu.Lock()
	defer s.attachMu.Unlock()
	return attached{s.provider, s.registry, s.model, s.effort, s.effortCeiling}
}

// detach drops the resolved provider so the next turn resolves it again.
func (s *tuiSession) detach() {
	s.attachMu.Lock()
	defer s.attachMu.Unlock()
	s.provider, s.registry, s.model, s.effort, s.effortCeiling = nil, nil, "", "", ""
}

type harnessHost struct {
	reg    *flags.Registry
	runner *tui.Runner

	// busyTurns is injected in tests. tui.New refuses to build a runner
	// without a real terminal, so the alternative to a seam here is a guard
	// about concurrent turns that is never exercised by a test. Nil means ask
	// the runner, which is what every production path does.
	busyTurns func(exclude string) []string

	// credOnce guards the two fields below, which are built on first use rather
	// than in a constructor: a harnessHost assembled by a test sets only the
	// fields that test needs, and a nil resolver reached from a turn would be a
	// panic in production code that no test covers.
	credOnce sync.Once
	// resolver is this process's one credential resolver, and scrubber is the
	// backstop armed from it. One of each, shared: a scrubber armed from a
	// resolver that some other part of the process replaced is a backstop that
	// does not hold the values actually in use.
	resolver *credentials.Resolver
	scrubber *credentials.Scrubber

	// firstSession carries the id of the session that opens first. The index
	// refresh runs before any session exists and has to report into a
	// transcript, and reporting into whichever session happens to be on screen
	// when it finishes would put a startup fact into a turn it has nothing to
	// do with. Buffered, and sent to once, so the send never blocks the UI.
	firstSession chan string
	announceOnce sync.Once

	mu       sync.Mutex
	sessions map[string]*tuiSession
	seq      int
}

// Commands lists the slash commands, which are the CLI subcommands.
//
// Same names, same arguments, same code. A TUI with its own parallel command
// implementations is a second surface to keep in step, and the two diverge on
// the first fix applied to only one.
func (h *harnessHost) Commands() []tui.CommandSpec {
	return []tui.CommandSpec{
		{Name: "doctor", Summary: "configuration, store reachability, and weakened gates"},
		{Name: "flags", Args: "[--all] | set KEY VALUE", Summary: "settings, their values, where each came from, and how to move one"},
		{Name: "providers", Summary: "model backends and whether each one is usable"},
		{Name: "local", Args: "[--resolve] [--timeout DURATION]", Summary: "find local model servers and what they serve"},
		{Name: "tools", Summary: "the native DevCouncil tools an agent can call"},
		{Name: "leases", Summary: "show active leases"},
		{Name: "lease", Args: "list|acquire|release", Summary: "manage leases in the store"},
		{Name: "check", Args: "PATH [--task ID]", Summary: "evaluate a write against policy, and say why"},
		{Name: "allow", Args: "PATH --reason TEXT", Summary: "record a human override", Mutating: true},
		{Name: "tool", Args: "NAME [--json ARGS]", Summary: "call a native tool directly", Mutating: true},
		{Name: "map", Args: "[status|build]", Summary: "repository index for navigation and the neighbour rule"},
		{Name: "probe", Args: "PROVIDER [--model NAME] [--effort LEVEL]", Summary: "test a real live request against a provider"},
		{Name: "logo", Args: "[--svg]", Summary: "show the DevCouncil harness logo mark"},
		{Name: "help", Summary: "show keyboard shortcuts and help"},
		{Name: "clear", Summary: "clear the transcript buffer"},
		{Name: "quit", Summary: "leave manvi"},
	}
}

// newSessionState builds one session's harness state: its tool surface, the
// gate that judges it, and its log.
//
// Split out of NewSession so the wiring can be asserted without a terminal.
// The invariant it carries is one line long and was wrong for as long as the
// TUI has existed: the gate a session answers /check and /allow from must be
// the gate its tool surface writes through, not a second one built beside it.
// Two gates are two ledgers and two navigation indexes, and nothing in the
// running system ever prints which of them answered.
func newSessionState(reg *flags.Registry, id string, approver ui.Approver) (*tuiSession, error) {
	// The session's own approval seam is attached to its tools, so a blocked
	// write escalates to this session's card rather than to whichever one
	// happens to be on screen.
	native, pipeline, err := nativeToolsWith(reg, approver)
	if err != nil {
		return nil, err
	}
	return &tuiSession{
		id: id, title: "session " + id,
		log:    session.NewLog(),
		gate:   native.Gate(),
		native: native, pipeline: pipeline,
		subMeter: &subAgentMeter{},
	}, nil
}

// Settings reports the flag catalogue for the settings picker.
//
// Generated from the registry, like `manvi flags` is, so the picker cannot list
// a setting the harness does not have or miss one it does. It is a read: the
// picker arms `/flags set`, and that command is the only thing that moves a
// value.
func (h *harnessHost) Settings() []tui.SettingSpec {
	keys := h.reg.Keys()
	specs := make([]tui.SettingSpec, 0, len(keys))
	for _, key := range keys {
		def, ok := h.reg.Def(key)
		if !ok {
			continue
		}
		value, err := h.reg.Lookup(key)
		if err != nil {
			// Listed with the failure in place of the value rather than
			// dropped. A setting missing from the picker reads as a setting the
			// harness does not have, which is the opposite of what a lookup
			// error means.
			specs = append(specs, tui.SettingSpec{
				Key: key, Value: "unreadable: " + err.Error(),
				Origin: "error", Mutable: string(def.Mutable),
				Safety: def.Safety, Summary: def.Description,
			})
			continue
		}
		specs = append(specs, tui.SettingSpec{
			Key:      key,
			Value:    value.Raw,
			Origin:   string(value.Origin),
			Mutable:  string(def.Mutable),
			Safety:   def.Safety,
			AtSafest: !def.Safety || value.Raw == safestValue(def),
			Choices:  def.Values,
			Summary:  def.Description,
		})
	}
	return specs
}

// NewSession builds a session and its harness state.
func (h *harnessHost) NewSession(ctx context.Context) (string, string, error) {
	h.mu.Lock()
	h.seq++
	id := fmt.Sprintf("S%d", h.seq)
	h.mu.Unlock()

	s, err := newSessionState(h.reg, id, h.runner.Approver(id))
	if err != nil {
		return "", "", err
	}

	sink := h.sinkFor(id)
	// The face reads the log rather than a stream emitted beside it, so what
	// the terminal shows is by construction what the model saw.
	s.log.Observe(ui.ProjectSink(sink))

	h.mu.Lock()
	h.sessions[id] = s
	h.mu.Unlock()
	h.announceOnce.Do(func() { h.firstSession <- s.id })

	posture, _, _ := h.reg.String(flags.HarnessPosture)
	provider, _, _ := h.reg.String(flags.LLMDefaultProvider)
	model, unresolved := h.describeModel(provider)
	sink.Emit(ui.Event{
		Kind: ui.KindSessionStart, At: time.Now().UTC(),
		Posture: posture, Model: model,
	})
	// The banner has room for a word and the reason is a sentence, so the
	// reason goes here rather than being lost to the word. It is emitted per
	// session on the same rule as the weakened-settings notice below: a session
	// that cannot send a turn is not a fact that stops being true because an
	// earlier session already said it.
	if unresolved != "" {
		sink.Emit(ui.Event{
			Kind: ui.KindNotice, At: time.Now().UTC(),
			Text:     "no model is configured for provider " + provider + ": " + unresolved,
			Degraded: []string{"a turn cannot be sent until a model is set"},
		})
	}
	// Weakened settings are announced when the session opens and are also held
	// on the status bar, because a banner in a transcript scrolls away and the
	// fact does not stop being true when it does.
	if weak := h.reg.Weakened(); len(weak) > 0 {
		names := make([]string, 0, len(weak))
		for _, v := range weak {
			names = append(names, fmt.Sprintf("%s=%s (%s)", v.Key, v.Raw, v.Origin))
		}
		sink.Emit(ui.Event{
			Kind: ui.KindNotice, At: time.Now().UTC(),
			Text:     "results produced under these settings are not strict",
			Weakened: names,
		})
	}
	return s.id, s.title, nil
}

// refreshIndexInBackground brings the navigation index up to date while the
// operator is already working.
//
// It reports into the transcript in every outcome, including the one where
// nothing needed doing. The index is what the navigation tools answer from and
// what the gate's neighbour rule decides on, and those two answer very
// differently with a stale index than with none — "no callers, safe to delete"
// and "this build has no data" are the same empty list. An operator who was not
// told which one they are looking at will read either as a fact about their
// code.
//
// Nothing here is fatal. A repository with no devmap on PATH is a repository
// the harness still runs in, with navigation reporting unavailable.
func (h *harnessHost) refreshIndexInBackground(ctx context.Context) {
	on, _, err := h.reg.Bool(flags.HarnessInitEnabled)
	if err != nil || !on {
		return
	}

	var sessionID string
	select {
	case sessionID = <-h.firstSession:
	case <-ctx.Done():
		return
	}
	sink := h.sinkFor(sessionID)

	root := projectRoot()
	client := mapClient(root)

	// The artifact is checked alongside the index because they are two files
	// with two lifetimes and only one of them was ever consulted here. The gate
	// does not read the index; it reads the code graph, which `devmap build`
	// does not write and a failed manifest does not replace.
	status, availErr := client.Available(ctx)
	plan := planIndexRefresh(status, availErr, artifactDivergence(graphArtifactPath(), status))
	sink.Emit(ui.Event{Kind: plan.Kind, At: time.Now().UTC(), Text: plan.Text, Degraded: plan.Degraded})
	if !plan.Build {
		return
	}

	report, err := refreshIndex(ctx, client)
	if err != nil {
		if ctx.Err() != nil {
			// The session is ending; the failure is the shutdown, not the index.
			return
		}
		// The message names which of the two commands failed and degrades only
		// what that one feeds: see refreshError. Reporting a failed artifact
		// write as a failed index build named a capability that was working and
		// left the one that was not unnamed.
		text, degraded := refreshFailure(err)
		sink.Emit(ui.Event{Kind: ui.KindError, At: time.Now().UTC(), Text: text})
		sink.Emit(ui.Event{Kind: ui.KindNotice, At: time.Now().UTC(),
			Text:     "run 'manvi map build' to see the whole error",
			Degraded: degraded})
		return
	}
	// A build that refused files is emitted as a notice rather than a report,
	// on the same rule indexPlan uses below: a report is for an index that
	// needs nothing, a notice for one that is less than it appears.
	kind := ui.KindReport
	if !report.Clean() {
		kind = ui.KindNotice
	}
	sink.Emit(ui.Event{Kind: kind, At: time.Now().UTC(),
		Text:     describeIndex(report),
		Degraded: report.Degraded(),
		// Sessions load the code graph when they open. This one opened first,
		// which is how it got this report, and it is holding whatever the graph
		// said then.
		Detail: "open a new session to navigate and gate against the rebuilt graph",
	})
}

// indexPlan is what the background refresh decided about the index it found,
// and what the transcript is told about it.
type indexPlan struct {
	// Build says the index has to be rebuilt.
	Build bool
	// Kind is how Text is emitted: a report when the index needs nothing, a
	// notice when something about it is less than it appears.
	Kind ui.Kind
	Text string
	// Degraded names what will answer wrongly until the rebuild lands.
	Degraded []string
}

// planIndexRefresh reads the index's self-report and decides.
//
// It is separated from the emitting so it can be tested without a terminal, and
// because the decision is the part with a wrong answer available: a stale index
// and a missing one both need a build, but they degrade different things, and
// telling an operator "unavailable" about an index that is merely old — or
// "stale" about one that was never built — sends them to the wrong problem.
// diverged is what the code graph on disk disagrees with the index about, empty
// when they agree or when there is nothing to compare.
func planIndexRefresh(status *devmap.Status, err error, diverged []string) indexPlan {
	switch {
	case err != nil:
		return indexPlan{
			Build: true, Kind: ui.KindNotice,
			Text:     "building the repository index in the background: " + err.Error(),
			Degraded: []string{"navigation tools and the neighbour rule report unavailable until it lands"},
		}
	case status == nil:
		// No status and no error is a contract the client is not supposed to
		// be able to produce. Building is the answer that cannot mislead.
		return indexPlan{
			Build: true, Kind: ui.KindNotice,
			Text:     "the repository index reported neither a state nor an error; rebuilding it in the background",
			Degraded: []string{"navigation tools and the neighbour rule report unavailable until it lands"},
		}
	case !status.IsFresh:
		return indexPlan{
			Build: true, Kind: ui.KindNotice,
			Text:     "the repository index was built before the current working tree; rebuilding it in the background",
			Degraded: []string{"navigation answers describe the previous state until the rebuild lands"},
		}
	case len(diverged) > 0:
		// The index is current and the file the gate reads is not. This is the
		// state the check missed for as long as it asked only devmap: the
		// index's own freshness says nothing about an artifact written by a
		// different command, and reporting "index current" here sent an
		// operator away from the half that was wrong.
		return indexPlan{
			Build: true, Kind: ui.KindNotice,
			Text: "the code graph the scope rung reads did not come from the current index; " +
				"rebuilding it in the background: " + strings.Join(diverged, "; "),
			Degraded: []string{
				"until it lands, the scope rung decides neighbour writes from an older graph " +
					"while the navigation tools answer from the current index"},
		}
	default:
		return indexPlan{
			Kind: ui.KindReport,
			Text: fmt.Sprintf("repository index current — generation %d, %d symbols, %d edges",
				status.GenerationID, status.NodeCount, status.EdgeCount),
		}
	}
}

// artifactDivergence reports what the code graph on disk disagrees with the
// index about, for the session-start check.
//
// A status that could not be read yields nothing rather than a complaint: the
// caller already reports the unavailable index, and a second line inferring
// something about the artifact from an index nobody could read would be a
// conclusion drawn from a check that did not run.
func artifactDivergence(graphPath string, status *devmap.Status) []string {
	if status == nil {
		return nil
	}
	m, err := repomap.LoadIfPresent(graphPath)
	if err != nil || m == nil {
		// Absent or unreadable is not divergence; it is the unavailability the
		// gate already records as repo_map.unavailable on every decision.
		return nil
	}
	return m.DisagreementsWith(status.GenerationID, status.NodeCount)
}

// describeModel reports the model that would be used, and — when there is none
// — the resolver's own account of why, for the caller to emit beside the banner.
//
// A nil provider is passed deliberately: this fills a banner at session start,
// and the local adapter's model list is a network round trip to a server that
// may not be up. Configuration alone answers this — MANVI_MODEL, or the
// provider's own setting — and anything it cannot answer is honestly
// "unconfigured" rather than worth blocking the first frame on.
//
// The reason is returned rather than discarded, which is the repair. The
// resolver does not fail vaguely: it answers `set MANVI_MODEL — anthropic
// serves: claude-opus-5, …`, naming the variable to set and the values that
// would work. All of that was being thrown away for the word "unconfigured",
// which tells an operator that something is wrong and nothing about what to do,
// on the one screen where they have not yet done anything else to look at.
func (h *harnessHost) describeModel(provider string) (label, unresolved string) {
	model, _, err := resolveModelFor(context.Background(), provider, nil, h.reg)
	if err != nil {
		return provider + " (unconfigured)", err.Error()
	}
	return model, ""
}

// CloseSession releases whatever the session held.
func (h *harnessHost) CloseSession(ctx context.Context, id string) error {
	h.mu.Lock()
	s := h.sessions[id]
	delete(h.sessions, id)
	h.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.release(ctx)
}

// release gives back the session's lease, if it holds one.
//
// It goes through the tool an agent would call rather than reaching into the
// store, so shutdown exercises the same path — including its policy checks —
// that a release during a turn does.
func (s *tuiSession) release(ctx context.Context) error {
	if s.native.Session().TaskID == "" {
		return nil
	}
	result := s.pipeline.Run(ctx, tools.Call{
		ID: "shutdown", Name: "devcouncil_release_task", Arguments: []byte(`{}`),
	})
	if result.IsError {
		return errors.New(result.Text)
	}
	return nil
}

func (h *harnessHost) releaseAll(ctx context.Context) error {
	h.mu.Lock()
	sessions := make([]*tuiSession, 0, len(h.sessions))
	for _, s := range h.sessions {
		sessions = append(sessions, s)
	}
	h.sessions = map[string]*tuiSession{}
	h.mu.Unlock()

	var failures []string
	for _, s := range sessions {
		if err := s.release(ctx); err != nil {
			failures = append(failures, s.id+": "+err.Error())
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

func (h *harnessHost) session(id string) (*tuiSession, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[id]
	if !ok {
		return nil, fmt.Errorf("no session %s", id)
	}
	return s, nil
}

// Cancel is a no-op beyond the context the runner already cancelled: the agent
// loop stops on its context, and the lease is released by CloseSession rather
// than here, because a cancelled turn's task is still checked out to this
// session and taking it away mid-edit would be worse than holding it.
func (h *harnessHost) Cancel(string) {}

// Command runs a harness command, with its output going to the session's
// transcript instead of stdout.
func (h *harnessHost) Command(ctx context.Context, sessionID, name, args string) error {
	s, err := h.session(sessionID)
	if err != nil {
		return err
	}
	sink := h.sinkFor(sessionID)
	sink.Emit(ui.Event{
		Kind: ui.KindToolStart, At: time.Now().UTC(),
		Tool: "/" + name, Detail: args,
	})

	var out bytes.Buffer
	fields, err := splitArgs(args)
	if err != nil {
		sink.Emit(ui.Event{Kind: ui.KindError, At: time.Now().UTC(), Text: err.Error()})
		return nil
	}
	// changed is the flag a `/flags set` moved, or "". Captured out here rather
	// than returned through the closure because what a moved flag invalidates
	// is this host's business — the gates and providers it has already built —
	// and none of the command implementations know those exist.
	var changed string
	err = func() error {
		switch name {
		case "doctor":
			return doctor(&out, h.reg)
		case "flags":
			if err := h.refuseFlagMoveDuringATurn(sessionID, fields); err != nil {
				return err
			}
			key, err := flagsCommand(&out, h.reg, fields, surfaceSession)
			changed = key
			return err
		case "providers":
			return showProviders(&out, h.reg)
		case "local":
			return showLocal(&out, h.reg, fields)
		case "tools":
			return listTools(&out, h.reg)
		case "leases":
			return lease(&out, append([]string{"list"}, fields...))
		case "lease":
			if len(fields) == 0 {
				return lease(&out, []string{"list"})
			}
			return lease(&out, fields)
		case "check":
			// The session's own gate, so what this reports is what will
			// actually decide the session's next write.
			err := check(&out, s.gate, fields)
			if errors.Is(err, errCheckBlocked) || errors.Is(err, errCheckHardBlocked) {
				// A block is this command's answer, not its failure. On the CLI
				// it is a non-zero exit status because a script has nothing else
				// to read; here the decision is already in the transcript above,
				// and reporting it a second time as an error would tell the
				// operator the command did not run.
				return nil
			}
			return err
		case "allow":
			return allow(&out, s.gate, fields)
		case "tool":
			// The session's own pipeline: it carries this session's lease, and
			// its approval seam, so a blocked write raises a card here rather
			// than being refused by a registry nobody is watching.
			_, scrubber := h.creds()
			return callTool(&out, &out, scrubber, s.pipeline, fields)
		case "map":
			return mapCommand(&out, h.reg, fields)
		case "probe":
			return probe(&out, h.reg, fields)
		case "logo":
			return showLogo(&out, fields)
		case "quit", "exit", "help", "clear", "cls":
			return nil
		}
		return fmt.Errorf("unknown command %q", name)
	}()

	text := strings.TrimRight(out.String(), "\n")
	if err != nil {
		if text != "" {
			sink.Emit(ui.Event{Kind: ui.KindToolResult, At: time.Now().UTC(), Text: text})
		}
		sink.Emit(ui.Event{Kind: ui.KindError, At: time.Now().UTC(), Text: err.Error()})
		// Reported into the transcript and not returned: a command that failed
		// is not a reason to end the session, and the runner would render the
		// same string a second time.
		return nil
	}
	if text == "" {
		text = "(no output)"
	}
	sink.Emit(ui.Event{Kind: ui.KindToolResult, At: time.Now().UTC(), Text: text})
	if name == "flags" && changed == "" {
		// Appended here rather than inside showFlags because /settings is this
		// face's, and the CLI shares that function. A pointer to a command the
		// reader does not have is worse than no pointer.
		sink.Emit(ui.Event{
			Kind: ui.KindNotice, At: time.Now().UTC(),
			Text: "/settings opens this list as a picker you can navigate and filter",
		})
	}
	if changed != "" {
		h.applyFlagChange(changed)
	}
	return nil
}

// refuseFlagMoveDuringATurn holds the rule the flags package states and could
// not enforce: a safety flag may only be moved outside a running turn.
//
// The registry is one object shared by every session, so a setting moved while
// another session is mid-turn changes the rules that turn is being judged by,
// halfway through — and the turn's own report, written at the end, describes
// the settings it finds then rather than the ones it ran under. One session is
// safe by construction, because the runner allows one turn per session and this
// command is that session's turn. More than one is not.
//
// It refuses every set, not only the safety ones. A relaxed grants.* ceiling or
// a switched provider mid-turn is the same kind of surprise, and a rule with
// exceptions is a rule an operator has to remember.
func (h *harnessHost) refuseFlagMoveDuringATurn(sessionID string, fields []string) error {
	if len(fields) == 0 || fields[0] != "set" {
		return nil
	}
	busy := h.busyElsewhere(sessionID)
	if len(busy) == 0 {
		return nil
	}
	return fmt.Errorf("a setting cannot be moved while a turn is running elsewhere: %s. "+
		"Let it finish, or cancel it with ctrl+c in that session — a turn judged by rules "+
		"that changed halfway through cannot report which ones it ran under",
		strings.Join(busy, ", "))
}

// sinkFor is a session's event sink.
//
// Nil runner means nothing is listening — a host built for a test, or one whose
// terminal never started. Returning a sink that discards is the only safe
// answer: Runner.Sink closes over a channel the constructor makes, so calling
// it on a nil runner hands back a sink whose first Emit blocks for ever on a
// nil channel, and a harness that hangs on its first event is worse than one
// that reports into nothing.
func (h *harnessHost) sinkFor(sessionID string) ui.Sink {
	if h.runner == nil {
		return ui.SinkFunc(func(ui.Event) {})
	}
	return h.runner.Sink(sessionID)
}

// busyElsewhere lists the sessions other than this one with a turn in flight.
func (h *harnessHost) busyElsewhere(exclude string) []string {
	if h.busyTurns != nil {
		return h.busyTurns(exclude)
	}
	if h.runner == nil {
		return nil
	}
	return h.runner.Busy(exclude)
}

// reloadPlan says which of the harness's own snapshots a moved flag invalidates.
//
// Most of the catalogue is read at the point of use, so a change lands on the
// next decision with nothing to do here. These two are the exceptions, and each
// is a place the harness copied a value out of the registry into something
// long-lived. TestReloadPlanMatchesTheFlagCatalogue holds this in step with
// flags.ReachOf, so a namespace classified as needing a reload cannot be one
// nothing reloads.
type reloadPlan struct {
	// grantPolicy: gate.grantPolicyFrom copies six grants.* flags into the
	// ledger's policy when the gate is built.
	grantPolicy bool
	// provider: attachProvider resolves the provider, model and effort once and
	// caches them on the session.
	provider bool
}

func (p reloadPlan) any() bool { return p.grantPolicy || p.provider }

func reloadPlanFor(key string) reloadPlan {
	return reloadPlan{
		grantPolicy: strings.HasPrefix(key, "grants."),
		provider:    strings.HasPrefix(key, "llm."),
	}
}

// applyFlagChange reloads what the change invalidated and tells every session
// what the settings are now.
//
// Every session, not the one that typed it: the registry is one object, so a
// safety flag moved in session two is a fact about session one's next turn as
// well. A weakened-settings notice that appeared only where it was typed would
// leave the other transcripts describing a strictness that had stopped being
// true.
//
// Safe to mutate session state from here because refuseFlagMoveDuringATurn has
// already established that no other session has a turn in flight, and this
// session's turn is this command.
func (h *harnessHost) applyFlagChange(key string) {
	plan := reloadPlanFor(key)

	h.mu.Lock()
	ids := make([]string, 0, len(h.sessions))
	for id := range h.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	sessions := make([]*tuiSession, 0, len(ids))
	for _, id := range ids {
		sessions = append(sessions, h.sessions[id])
	}
	h.mu.Unlock()

	var failures []string
	for _, s := range sessions {
		if plan.grantPolicy && s.gate != nil {
			if err := s.gate.ReloadPolicy(); err != nil {
				// Reported, never swallowed. A ledger left on the old policy
				// while the flag table reports the new one is the exact
				// divergence ReloadPolicy exists to close.
				failures = append(failures, fmt.Sprintf("%s: grant policy not reloaded: %v", s.id, err))
			}
		}
		if plan.provider {
			// Dropped rather than re-resolved here: resolving talks to a
			// provider, and doing that inside a settings command would make
			// `/flags set` hang on an unreachable server. The next turn
			// re-attaches, which is the path that already reports its failures
			// where an operator will see them.
			s.detach()
		}
	}

	notice := h.settingsNotice(key, plan)
	for _, id := range ids {
		sink := h.sinkFor(id)
		sink.Emit(notice)
		for _, f := range failures {
			sink.Emit(ui.Event{Kind: ui.KindError, At: time.Now().UTC(), Text: f})
		}
	}
}

// settingsNotice is what every session is told after a setting moves.
//
// It carries the posture and the full weakened list, not just the key that
// changed, because both are facts about the whole harness rather than about one
// transcript. The status bar folds them in: the posture chip answers "right
// now", and the weakened list accumulates, so a session that ran a turn under a
// relaxed setting goes on saying so after the setting is put back.
func (h *harnessHost) settingsNotice(key string, plan reloadPlan) ui.Event {
	posture, _, _ := h.reg.String(flags.HarnessPosture)
	weak := h.reg.Weakened()
	names := make([]string, 0, len(weak))
	for _, v := range weak {
		names = append(names, fmt.Sprintf("%s=%s (%s)", v.Key, v.Raw, v.Origin))
	}
	value, _ := h.reg.Lookup(key)
	text := fmt.Sprintf("%s is now %s", key, value.Raw)
	if plan.any() {
		text += "; this session's " + strings.Join(reloadedWhat(plan), " and ") + " reloaded"
	}
	return ui.Event{
		Kind: ui.KindNotice, At: time.Now().UTC(),
		Text: text, Posture: posture, Weakened: names,
	}
}

func reloadedWhat(p reloadPlan) []string {
	var what []string
	if p.grantPolicy {
		what = append(what, "grant policy")
	}
	if p.provider {
		what = append(what, "model provider")
	}
	return what
}

// splitArgs splits a slash command's arguments the way a shell would.
//
// strings.Fields is the obvious choice and it cannot express the arguments this
// surface actually takes: `--json {"content":"package calc"}` has a space inside
// a value, and splitting on whitespace hands the JSON decoder half an object.
// The CLI never hit this because the shell did the quoting before the process
// started; inside the TUI there is no shell, so the quoting has to happen here.
//
// Single quotes are literal, double quotes allow backslash escapes, and an
// unterminated quote is an error rather than a silently truncated argument.
func splitArgs(s string) ([]string, error) {
	var (
		args    []string
		cur     strings.Builder
		started bool
		quote   rune
	)
	flush := func() {
		if started {
			args = append(args, cur.String())
			cur.Reset()
			started = false
		}
	}

	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote == '\'' && r == '\'':
			quote = 0
		case quote == '"' && r == '"':
			quote = 0
		case quote == '"' && r == '\\' && i+1 < len(runes):
			i++
			cur.WriteRune(runes[i])
			started = true
		case quote != 0:
			cur.WriteRune(r)
			started = true
		case r == '\'' || r == '"':
			quote = r
			// An opened quote starts an argument even if it turns out empty,
			// so `--reason ""` is one empty argument rather than none.
			started = true
		case r == ' ' || r == '\t':
			flush()
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote", quote)
	}
	flush()
	return args, nil
}

// Submit runs one turn.
func (h *harnessHost) Submit(ctx context.Context, sessionID, text string) error {
	s, err := h.session(sessionID)
	if err != nil {
		return err
	}
	if s.attachment().provider == nil {
		if err := h.attachProvider(s); err != nil {
			// Returned rather than swallowed. A harness that accepts a prompt
			// and quietly does nothing because it has no credential is worse
			// than one that refuses: the operator waits for a reply that was
			// never going to come.
			return err
		}
	}
	// One snapshot for the whole turn. Reading the fields individually would
	// let a settings change land between two of them and start a turn with a
	// provider from before it and a model from after.
	at := s.attachment()
	if at.provider == nil {
		return errors.New("no model provider is attached to this session")
	}

	maxTokens := 0
	if name, _, err := h.reg.String(flags.LLMDefaultProvider); err == nil && name == "local" {
		if capTokens, _, err := h.reg.Int(flags.LLMLocalMaxOutputTokens); err == nil && capTokens > 0 {
			maxTokens = capTokens
		}
	}
	if maxTokens <= 0 {
		maxTokens = 8192
	}

	coreOnly := coreToolsOnly(h.reg, at.provider.Name())
	dynamic := dynamicToolsEnabled(h.reg, at.provider.Name())
	if dynamic {
		s.pipeline.EnableDynamic()
	}
	systemPrompt := assemblePrompt(h.reg, PromptOptions{
		Provider:         at.provider.Name(),
		TaskToolsOffered: taskToolsOffered(s.pipeline, coreOnly),
		DynamicTools:     dynamic,
		ActiveGroups:     s.pipeline.ActiveGroups(),
	})

	// Re-attached on every submission rather than once at session start: the
	// provider, model and effort are all switchable mid-session from this face,
	// and a child running under the model the user chose two turns ago would be
	// a surprise nothing reported.
	if runner := subRunners[s.pipeline]; runner != nil {
		runner.attach(subAgentConfig{
			provider:        at.provider,
			models:          at.registry,
			model:           at.model,
			effort:          at.effort,
			effortCeiling:   at.effortCeiling,
			registry:        s.pipeline,
			native:          s.native,
			meter:           s.subMeter,
			coreToolsOnly:   coreOnly,
			assertInvariant: assertInvariant(h.reg),
			systemPrompt:    systemPrompt,
			// The same sink this session's own events go to, so a child's gate
			// refusals appear in the transcript that is meant to account for
			// the turn rather than vanishing with the child.
			sink: h.sinkFor(sessionID),
		})
	}

	// Taken before the turn runs, so what it spends on children can be told
	// apart from what earlier turns of this session already spent.
	before := s.subMeter.Total()

	loop, err := agent.NewLoop(agent.Config{
		Provider:        at.provider,
		Registry:        at.registry,
		Model:           at.model,
		Effort:          at.effort,
		EffortCeiling:   at.effortCeiling,
		SystemPrompt:    systemPrompt,
		MaxSteps:        maxSteps(h.reg),
		MaxTokens:       maxTokens,
		CoreToolsOnly:   coreOnly,
		AssertInvariant: assertInvariant(h.reg),
	}, bus.New(), s.log, s.pipeline)
	if err != nil {
		return err
	}

	outcome, err := loop.Run(ctx, llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{llm.TextBlock{Text: text}},
	})
	if err != nil {
		return err
	}

	sink := h.sinkFor(sessionID)
	// This turn's delegated spend, not the session's: the meter accumulates
	// across every turn of the session, so the figure for one turn is the
	// difference against the snapshot taken before it ran. Reporting the
	// running total per turn would count the first turn's children again in
	// every turn after it.
	delegated := s.subMeter.Total()
	delegated.InputTokens -= before.InputTokens
	delegated.OutputTokens -= before.OutputTokens
	delegated.ReasoningTokens -= before.ReasoningTokens
	delegated.CacheReadTokens -= before.CacheReadTokens
	sink.Emit(ui.Event{
		Kind: ui.KindUsage, At: time.Now().UTC(),
		InputTokens:  outcome.Usage.InputTokens + delegated.InputTokens,
		OutputTokens: outcome.Usage.OutputTokens + delegated.OutputTokens,
	})
	// The same owner the headless face uses. The TUI used to hand-roll its own
	// subset and silently omitted repeats, stalls, malformed calls and every
	// decoding compensation.
	for _, n := range outcomeNotices(outcome, maxSteps(h.reg)) {
		kind := ui.KindReport
		if len(n.Degraded) > 0 {
			kind = ui.KindNotice
		}
		sink.Emit(ui.Event{
			Kind: kind, At: time.Now().UTC(),
			Text: n.Text, Degraded: n.Degraded,
		})
	}
	return nil
}

// creds returns this host's credential resolver and the scrubber armed from it.
//
// WatchAll runs on every call, not only the first. buildProvider registers the
// requirement for the provider it is building, so a credential that was not a
// requirement when the UI started — a provider switched to mid-session — is not
// in the watched set until after it has been resolved once.
func (h *harnessHost) creds() (*credentials.Resolver, *credentials.Scrubber) {
	h.credOnce.Do(func() {
		if h.resolver == nil {
			h.resolver = credentials.NewResolver()
		}
		if h.scrubber == nil {
			h.scrubber = credentials.NewScrubber()
		}
	})
	h.scrubber.WatchAll(h.resolver)
	return h.resolver, h.scrubber
}

// attachProvider resolves the provider, credential, and model for a session.
func (h *harnessHost) attachProvider(s *tuiSession) error {
	name, _, err := h.reg.String(flags.LLMDefaultProvider)
	if err != nil {
		return err
	}
	resolver, _ := h.creds()
	provider, err := buildProvider(name, h.reg, resolver, io.Discard)
	if err != nil {
		return err
	}
	// Re-armed after the build, which is when the provider's own credential
	// requirement became known to the resolver. Before this call the scrubber
	// could not have been watching the key this session is about to use.
	h.creds()

	registry := llm.NewRegistry()
	if err := registry.Register(provider); err != nil {
		return err
	}

	model, _, err := resolveModelFor(context.Background(), name, provider, h.reg)
	if err != nil {
		return err
	}
	effort, err := h.resolveEffort(registry, name, model)
	if err != nil {
		return err
	}
	ceiling, err := h.resolveEffortCeiling(provider, model, effort)
	if err != nil {
		return err
	}
	s.attachMu.Lock()
	s.provider, s.registry, s.model, s.effort = provider, registry, model, effort
	s.effortCeiling = ceiling
	s.attachMu.Unlock()
	return nil
}

// resolveEffortCeiling reads how far a stuck turn may raise the effort above
// what resolveEffort settled, and checks that ladder against the model that
// will receive it.
//
// It is checked at attach for the same reason the tier itself is: a ceiling the
// model cannot reach is a configuration mistake, and one discovered mid-turn
// would surface as a refused request rather than as the setting that is wrong.
// The plan is built by the function the agent loop uses, so there is one
// answer rather than two that can drift.
func (h *harnessHost) resolveEffortCeiling(provider llm.Provider, model, effort string) (string, error) {
	ceiling, _, err := h.reg.String(flags.LLMEffortCeiling)
	if err != nil {
		return "", err
	}
	if ceiling == "" {
		return "", nil
	}
	capability, ok := provider.Capability(model)
	if !ok {
		return "", fmt.Errorf("%s=%s: %s cannot describe model %q, so the ceiling cannot be checked "+
			"against the tiers it accepts", flags.EnvKey(flags.LLMEffortCeiling), ceiling,
			provider.Name(), model)
	}
	if _, err := agent.PlanEffort(capability, effort, ceiling); err != nil {
		return "", fmt.Errorf("%s=%s: %w", flags.EnvKey(flags.LLMEffortCeiling), ceiling, err)
	}
	return ceiling, nil
}

// resolveEffort reads the reasoning tier this session will send, and checks it
// against the model that will receive it.
//
// The check happens here as well as at request assembly because the two
// failures read differently to an operator. A level the model does not accept
// is a configuration mistake, and found at attach it names the setting that is
// wrong; found at assembly it arrives as a refused request after a prompt has
// already been typed, which reads as the harness losing the turn.
//
// Empty stays empty. An unset effort means the field is omitted entirely and
// the provider's own default applies — the harness does not pick a tier on the
// operator's behalf, for the same reason it does not pick a model.
func (h *harnessHost) resolveEffort(registry *llm.Registry, provider, model string) (string, error) {
	effort, _, err := h.reg.String(flags.LLMEffort)
	if err != nil {
		return "", err
	}
	if effort == "" {
		return "", nil
	}
	// Resolve is the same capability gate the agent loop uses before every
	// request, so attach cannot accept a value the turn would then refuse.
	if _, _, err := registry.Resolve(provider, llm.Request{Model: model, Effort: effort}); err != nil {
		return "", fmt.Errorf("%s=%s: %w", flags.EnvKey(flags.LLMEffort), effort, err)
	}
	return effort, nil
}

// resolveModel picks the model for a provider.
//
// There is no default. The catalogues here are transcribed from vendor
// documentation and a model chosen for the operator would be a guess presented
// as a configuration — discovered as a 404 mid-turn rather than as a missing
// setting. So an unset MANVI_MODEL is an error that names the models this
// provider actually serves.
// systemPrompt assembles the prompt for a turn using prompt.Assembler.
func systemPrompt(reg *flags.Registry) string {
	return systemPromptFor(reg, "")
}

// PromptOptions is what the assembled prompt has to match.
type PromptOptions struct {
	// Provider selects provider-specific scaffolding.
	Provider string
	// TaskToolsOffered reports whether the task-lifecycle tools are in the set
	// the model can see. When they are not, the prompt must not tell it to use
	// them: naming a tool that is not offered spends tokens teaching the model
	// to reach for something that does not exist, and a refusal it cannot act
	// on is worse than silence.
	TaskToolsOffered bool
	// DynamicTools reports whether the offered set can grow during the turn.
	//
	// It changes what is true about an unlisted tool, which the tool contract
	// otherwise states flatly: without dynamic loading a tool that is not
	// listed does not exist and naming it wastes a step, and with it the tool
	// exists and there is a call that fetches it. A model told the first while
	// the second is true does not decline — it substitutes the nearest listed
	// tool, which is the failure llm.local.core_tools_only was measured
	// producing (0/32 on tasks needing an omitted tool, never once declining).
	DynamicTools bool
	// ActiveGroups names the tool groups the model can actually reach this
	// turn, from tools.Registry.ActiveGroups. Guidance for a group the model
	// does not hold is context spent on a capability that is not there.
	ActiveGroups []string
}

// systemPromptFor assembles the prompt for a specific provider.
//
// The sections below were written for a model that infers what is unsaid. A
// smaller local model does not: it needs the working directory named, the tool
// contract spelled out, and the stopping condition stated, because none of that
// is derivable from four sentences about policy. The scaffolding is added as an
// ordinary Source so it appears in the assembly report like everything else —
// what the model was told stays answerable.
func systemPromptFor(reg *flags.Registry, provider string) string {
	return assemblePrompt(reg, PromptOptions{Provider: provider, TaskToolsOffered: true})
}

// assemblePrompt builds the system prompt, and refuses to build it quietly
// when a section did not make it in.
//
// package prompt exists so "what did the model actually know" is answerable:
// Assemble returns the text *and* a Report naming what was included, what was
// dropped for budget, and what failed to load. This call site threw all of it
// away — seven `_ = a.Add(...)` and an `assembled, _ := a.Assemble()` — which
// left the package's whole reason for existing unenforced at the only place
// that could enforce it.
//
// Nothing here can fail today: every source is a prompt.Static. That is the
// argument for fixing it rather than leaving it. The day a source reads
// AGENTS.md or the repo map, a missing file would produce a prompt with a
// section silently absent, a model that was never told the tool contract, and
// a run whose transcript gives no hint why.
//
// The signature stays a plain string because both call sites build an
// agent.Config literal around it and cannot take a second return. So the fault
// goes to stderr instead, guarded by the same reasoning as the step-ceiling
// warning above: the alternative is a prompt that is quietly wrong.
func assemblePrompt(reg *flags.Registry, opts PromptOptions) string {
	text, faults := assemblePromptWithFaults(reg, opts)
	for _, fault := range faults {
		fmt.Fprintf(os.Stderr, "manvi: system prompt: %s\n", fault)
	}
	return text
}

// assemblePromptWithFaults is the same assembly with the account kept, so a
// test can assert on it rather than scraping stderr.
func assemblePromptWithFaults(reg *flags.Registry, opts PromptOptions) (string, []string) {
	return assembleSections(promptSources(reg, opts), promptTokenBudget(reg, opts.Provider))
}

// promptTokenBudget is how many estimated tokens the system prompt may spend
// before non-essential sections start being dropped.
//
// It is a share of the model's declared context window rather than a constant,
// because the number only means anything relative to what the model has: 3,000
// tokens of preamble is rounding error on a frontier context and a tenth of a
// small local one. Zero means unbounded, which is what a frontier provider
// gets — there the prompt has never been the scarce resource, and a budget
// that never binds is a number to maintain for nothing.
//
// The share is deliberately generous. This is a backstop against a section
// growing without bound — in practice project-instructions, which is whatever
// AGENTS.md happens to say — not a lever for shaving the fixed sections, which
// is what the compact density is for. A budget tight enough to bite on a
// healthy prompt would drop guidance on every turn and report a fault every
// time, which trains an operator to ignore the report.
func promptTokenBudget(reg *flags.Registry, provider string) int {
	if provider != local.Name {
		return 0
	}
	window, _, err := reg.Int(flags.LLMLocalContextWindow)
	if err != nil || window <= 0 {
		return 0
	}
	return window / promptBudgetShare
}

// promptBudgetShare is the divisor in promptTokenBudget: the system prompt may
// use up to one part in this many of the context window. At the default 32,768
// window that is 4,096 tokens, against a full prompt of roughly 900.
const promptBudgetShare = 8

// promptSources lists the sections a turn's prompt is built from, in the order
// they are contributed. Order within the prompt is the Section.Order field, not
// this slice — see prompt.Assemble.
func promptSources(reg *flags.Registry, opts PromptOptions) []prompt.Source {
	return promptRouter(reg, opts).Sources()
}

// promptRouter builds the routed prompt for a turn.
//
// There is one definition of every section here, and the router picks the
// wording. What stood here before was two branches — one per state of
// llm.local.guidance_router — holding the same twelve sections at two lengths,
// which is the duplication this repo refuses everywhere else. It had already
// drifted: the compact branch dropped mode-guidance entirely, so turning the
// router on silently took away the pair-programming and YOLO guidance rather
// than shortening it. A single list cannot lose a section that way.
func promptRouter(reg *flags.Registry, opts PromptOptions) *prompt.Router {
	provider := opts.Provider
	posture, _, _ := reg.String(flags.HarnessPosture)

	density := prompt.DensityFull
	if guidanceRouterEnabled(reg, provider) {
		density = prompt.DensityCompact
	}
	r := prompt.NewRouter(prompt.RouterConfig{
		Density:      density,
		Provider:     provider,
		ActiveGroups: opts.ActiveGroups,
	})

	// Essential is the line between what the model must be told and what makes
	// it work better. Identity, posture, the task rules, the policy rule and
	// the tool contract are the first: a model that has lost the policy section
	// to a budget is a model that will try the write the gate refused. The
	// working method, the review and hardening guidance, and whatever the
	// project's own instructions file says are the second — worth sending, not
	// worth sending in place of a rule.
	// Identity and the policy rule are the same at either density: both are
	// already as short as they can be said, and shortening a rule is how a rule
	// stops being followed.
	r.Vary("identity", 10, true, func(prompt.RouterConfig) string {
		return "You are a builder agent inside MANVI (DevCouncil harness).\n\n" +
			"Work through the native devcouncil_* tools. They are the only way to read or write\n" +
			"files here, and every write passes a policy gate before it lands."
	})

	r.Vary("posture", 20, true, func(cfg prompt.RouterConfig) string {
		text := fmt.Sprintf("Posture: %s.", posture)
		effect := flags.DescribePosture(posture)
		if !effect.Relaxed {
			return text
		}
		text += "\n" + effect.Notice
		if cfg.Density == prompt.DensityFull {
			text += "\n" +
				"A relaxed posture is not permission to widen what you were asked to do: it\n" +
				"means the operator, not the gate, is the only thing left checking."
		}
		return text
	})

	r.Vary("codebase-review", 22, false, func(cfg prompt.RouterConfig) string {
		if cfg.Density == prompt.DensityCompact {
			return "Systematic Review & Core Reuse:\n" +
				"Review the codebase to understand existing functions. Strictly avoid duplication:\n" +
				"extend existing seams."
		}
		return "Systematic Review & Core Reuse:\n" +
			"Upon initiating any project, systematically review the codebase to comprehend existing\n" +
			"functions, registries, and features. Construct upon them as a deliberate core.\n" +
			"Strictly avoid duplication, parallel abstractions, and bloat: extend existing seams\n" +
			"rather than standing up duplicate implementations."
	})

	r.Vary("devmap-grounding", 24, false, func(cfg prompt.RouterConfig) string {
		if cfg.Density == prompt.DensityCompact {
			return "Grounding & Dev Map Guidance:\n" +
				"Verify current documentation rather than relying on memory, and use the repository\n" +
				"dev map for structured symbol navigation."
		}
		return "Grounding & Dev Map Guidance:\n" +
			"When engaging with languages, libraries, or frameworks, consistently verify current\n" +
			"documentation and online references. Refrain from relying on unverified memory or intuition.\n" +
			"Utilize the repository dev map and code intelligence for structured navigation and exact\n" +
			"symbol relationships."
	})

	// Kept in both densities. Which of these two the model is reading is the
	// difference between it asking before acting and it acting alone, and a
	// shorter prompt is not worth getting that backwards.
	r.Vary("mode-guidance", 25, false, func(cfg prompt.RouterConfig) string {
		if posture == flags.PostureYolo {
			if cfg.Density == prompt.DensityCompact {
				return "YOLO Autonomous Mode:\n" +
					"Execute decisively and autonomously. Explore, test hypotheses, and create structured\n" +
					"artifacts. When options are ambiguous, proceed with the recommended default."
			}
			return "YOLO Autonomous Mode:\n" +
				"Move fast with decisive, autonomous execution. Take initiative to explore, discover files, " +
				"test hypotheses, and create structured artifacts. When options are ambiguous, proceed with the recommended default."
		}
		if cfg.Density == prompt.DensityCompact {
			return "Pair Programming & Guidance:\n" +
				"Collaborate transparently. State the rationale for non-obvious changes, the tradeoffs,\n" +
				"and the verification evidence. Plan before acting on underspecified requirements."
		}
		return "Pair Programming & Guidance:\n" +
			"Collaborate transparently with the user. Clearly communicate rationale for non-obvious changes, " +
			"design tradeoffs, and verification evidence. For underspecified requirements, formulate structured plans."
	})

	r.Vary("tasks", 30, true, func(cfg prompt.RouterConfig) string {
		if !opts.TaskToolsOffered {
			// The task tools are not in this model's set, so there is nothing
			// to check out and nothing to look for.
			return "There is no task to check out here. Read, edit and run commands directly;\n" +
				"the gate judges each write on its own."
		}
		if cfg.Density == prompt.DensityCompact {
			return "Tasks: devcouncil_next_task lists planned work. Check a task out only if it is\n" +
				"listed there — task ids cannot be invented. If the work is not a planned task,\n" +
				"read, edit and run commands directly."
		}
		return "Tasks: devcouncil_next_task lists planned work. Check a task out only if it is\n" +
			"listed there — task ids cannot be invented, and inventing one is refused. If the\n" +
			"work you were asked to do is not a planned task, do not look for one and do not\n" +
			"check one out: read, edit and run commands directly. The gate judges each write\n" +
			"on its own in that case."
	})

	r.Vary("policy", 40, true, func(prompt.RouterConfig) string {
		return "A blocked write is not a reason to try another path to the same file — in\n" +
			"particular, do not write through the shell what the write tool refused. Say what\n" +
			"was blocked and why, and let the operator decide."
	})

	r.Vary("problem-deconstruction", 42, false, func(cfg prompt.RouterConfig) string {
		if cfg.Density == prompt.DensityCompact {
			return "Problem Deconstruction & Hypotheses:\n" +
				"Deconstruct the problem and state hypotheses before implementing.\n" +
				"Break it before you fix it: characterize baseline behavior with tests first."
		}
		return "Problem Deconstruction & Hypotheses:\n" +
			"Before implementation, deconstruct problems and formulate explicit hypotheses.\n" +
			"Verify completeness by identifying and resolving all gaps.\n" +
			"Break it before you fix it: characterize baseline behavior with tests before modifying code,\n" +
			"and maintain clean persistent artifacts under .devcouncil/artifacts/."
	})

	r.Vary("hardening-stress-testing", 43, false, func(cfg prompt.RouterConfig) string {
		if cfg.Density == prompt.DensityCompact {
			return "Hardening & Adversarial Stress-Testing:\n" +
				"Validate boundaries, fail closed, bound timeouts and retries, propagate cancellation,\n" +
				"and adversarially probe edge cases (empty, nil, boundary, concurrent, timeout)."
		}
		return "Hardening & Adversarial Stress-Testing:\n" +
			"Guarantee universality and resilience by applying maximal hardening measures: validate boundaries,\n" +
			"fail closed, bound timeouts and retries, and propagate cancellation. Rigorously stress-test and\n" +
			"adversarially probe the solution (empty, nil, boundary, concurrent, and error inputs) to disrupt\n" +
			"existing logic and confirm robustness prior to deployment."
	})

	r.Vary("inquiry-impact", 44, false, func(cfg prompt.RouterConfig) string {
		if cfg.Density == prompt.DensityCompact {
			return "Inquiry & Decision Impact:\n" +
				"Clarify intent and surface architectural trade-offs before undertaking complex tasks\n" +
				"or designing collaborative UI."
		}
		return "Inquiry & Decision Impact:\n" +
			"Prioritize inquiry and clarify the impact of decisions before undertaking complex tasks\n" +
			"or designing the UI for collaborative applications. Clarify user intent and surface\n" +
			"architectural trade-offs early."
	})

	// Local-only, in both densities. These three answer questions a local model
	// asks and a frontier one does not — what the environment is, what the tool
	// contract requires literally, how to proceed when unsure — and they are
	// the sections whose absence shows up as malformed calls rather than as
	// worse judgement.
	onLocal := prompt.WhenProvider(local.Name)
	r.Vary("environment", 15, true, func(prompt.RouterConfig) string { return environmentSection() }, onLocal)
	r.Vary("tool-contract", 45, true, func(prompt.RouterConfig) string {
		return toolContractSection(opts.DynamicTools)
	}, onLocal)
	if opts.DynamicTools {
		// Essential, and not only for local. The offered set is smaller than
		// the registry whenever this is on, and a model that does not know the
		// rest can be fetched reaches for the nearest listed tool instead of
		// the right one.
		r.Vary("tool-discovery", 46, true, func(prompt.RouterConfig) string {
			return toolDiscoverySection()
		})
	}
	r.Vary("working-method", 50, false, func(prompt.RouterConfig) string { return workingMethodSection() }, onLocal)

	// Guidance that follows capability. These sections cost nothing on a turn
	// where their tools are not loaded, and appear on the turn the model
	// activates the group — which is exactly when it needs them and has not
	// been told anything about how the tools behave.
	//
	// The existence hint lives in tool-discovery, not here, and that split is
	// load-bearing: gating the hint on the same condition would mean a model
	// could only learn a group exists after activating it, which it would
	// never do.
	r.Vary("nav-guidance", 26, false, func(prompt.RouterConfig) string {
		return "Dev map navigation:\n" +
			"  - devcouncil_graph_query and devcouncil_graph_context resolve exact symbol\n" +
			"    relationships. Prefer them to grep for \"who calls this\" and \"what breaks\".\n" +
			"  - The graph is built from an index that can be stale. When it disagrees with\n" +
			"    the file you just read, the file wins."
	}, prompt.WhenGroupActive(tools.GroupNav))

	r.Vary("subagent-guidance", 27, false, func(prompt.RouterConfig) string {
		return "Delegating to sub-agents:\n" +
			"  - Give each child one scoped instruction and the context to act on it; a child\n" +
			"    does not see this conversation.\n" +
			"  - A child cannot dispatch children, so do not plan a tree.\n" +
			"  - Read what each child returned before acting on it. An empty summary is a\n" +
			"    failure, not a silent success."
	}, prompt.WhenGroupActive(tools.GroupSubagent))

	// The project's own instructions are the one section with no bound on their
	// size: AGENTS.md is whatever the repo wrote. It is not Essential for that
	// reason — a file that grew past the budget must lose to the rules, not
	// push them out.
	if pSrc, ok := projectInstructionsSource(); ok {
		r.Add(pSrc)
	}
	return r
}

func projectInstructionsSource() (prompt.Source, bool) {
	for _, name := range []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md"} {
		data, err := os.ReadFile(name)
		if err == nil && len(bytes.TrimSpace(data)) > 0 {
			return prompt.Static("project-instructions", 35, false,
				fmt.Sprintf("Project instructions (%s):\n%s", name, strings.TrimSpace(string(data)))), true
		}
	}
	return nil, false
}

// assembleSections registers every source, assembles, and names every source
// whose content did not reach the prompt.
//
// The test is "did this source's content arrive", not "does the Report mention
// it". Report.Omitted also carries the benign case of an essential section
// kept past the budget, so matching on reason strings would make this brittle
// in the direction of crying wolf on a healthy prompt. Comparing what was
// registered against Report.Included answers the question directly, and stays
// correct if the Report grows new reasons.
func assembleSections(sources []prompt.Source, maxTokens int) (string, []string) {
	a := prompt.New()
	a.MaxTokens = maxTokens
	var faults, registered []string
	for _, src := range sources {
		if err := a.Add(src); err != nil {
			// Add refuses a duplicate or unnamed source, which makes the
			// report ambiguous about which contributor produced what. That is
			// a programming error, but it was being discarded seven times over
			// and would have cost a whole section without a word.
			faults = append(faults, fmt.Sprintf("%q was not registered: %v", src.Name(), err))
			continue
		}
		registered = append(registered, src.Name())
	}

	text, report := a.Assemble()

	arrived := make(map[string]bool, len(report.Included))
	for _, in := range report.Included {
		arrived[in.Source] = true
	}
	for _, name := range registered {
		if arrived[name] {
			continue
		}
		faults = append(faults, fmt.Sprintf("%q is missing from the prompt: %s", name, whyMissing(report, name)))
	}
	return text, faults
}

// whyMissing recovers the Report's own account of a source that did not
// arrive. A reason the assembler already worked out is more use than one this
// function invents, and the fallback exists only so a source that vanished for
// a reason the Report does not carry is still reported as vanished.
func whyMissing(report prompt.Report, source string) string {
	for _, group := range [][]prompt.Omitted{report.Failed, report.Omitted} {
		for _, o := range group {
			if o.Source == source {
				return o.Reason
			}
		}
	}
	return "the assembly report does not say why"
}

// environmentSection states the facts a model cannot infer and will otherwise
// invent: where it is, and what it is running on.
func environmentSection() string {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		cwd = "(unknown)"
	}
	return fmt.Sprintf(
		"Environment:\n"+
			"  working directory: %s\n"+
			"  platform: %s/%s\n"+
			"Paths you pass to tools are relative to the working directory unless absolute.",
		cwd, runtime.GOOS, runtime.GOARCH)
}

// toolContractSection spells out the call contract.
//
// Every line here is a failure seen from a smaller model: inventing a tool that
// was not offered, describing an edit instead of making it, and re-reading the
// same file because nothing said the result was already in the conversation.
func toolContractSection(dynamic bool) string {
	unlisted := "  - Use only the tools listed for you. A tool that is not listed does not exist,\n" +
		"    and naming one wastes a step.\n"
	if dynamic {
		// Under dynamic loading the flat rule above is false, and stating it
		// anyway is worse than saying nothing: it tells the model its only
		// option is a listed tool, so it picks the closest one rather than
		// fetching the right one. See toolDiscoverySection.
		unlisted = "  - The tools listed for you are a working set, not the whole registry. Do not\n" +
			"    name a tool that is not listed — activate it first, see below.\n"
	}
	return "Calling tools:\n" + unlisted +
		"  - To change a file, call the write or patch tool. Describing the change in prose\n" +
		"    does not change anything on disk.\n" +
		"  - Read a file and consult the dev map before editing it; do not re-read unchanged files.\n" +
		"  - One step may contain several tool calls when they are independent. Calls that\n" +
		"    depend on each other's results belong in separate steps.\n" +
		"  - If a call is refused, read the reason before trying anything else. Repeating\n" +
		"    an identical call returns an identical answer and is refused after a few tries."
}

// toolDiscoverySection is contributed only when the offered set can grow.
//
// It is the other half of dynamic loading. Narrowing the set without it does
// not make a model ask for what is missing; it makes it answer with what is
// present, which is how "which task next?" gets answered by list_dir.
func toolDiscoverySection() string {
	return "Loading more tools:\n" +
		"  - The listed tools are a working set. Task lifecycle, code-graph navigation,\n" +
		"    sub-agents, artifacts and MCP tools exist but are not loaded yet.\n" +
		"  - devcouncil_search_tools finds them by keyword or group and costs no context.\n" +
		"  - devcouncil_activate_tools loads them by name or by group — task, nav, subagent,\n" +
		"    artifact, mcp — and they are callable from the next step onward.\n" +
		"  - When no listed tool does what the step needs, search and activate. Do not\n" +
		"    substitute the closest listed tool, and do not report the work impossible."
}

// workingMethodSection gives the shape of a turn and, importantly, its end.
// Not essential: it is guidance rather than contract, so it yields to budget
// before the policy and tool sections do.
func workingMethodSection() string {
	return "Working method:\n" +
		"  - Systematically review existing code and dev map navigation before changing anything.\n" +
		"  - Deconstruct problems, formulate hypotheses, and identify gaps upfront.\n" +
		"  - Characterize baseline behavior with tests first; reuse and extend the core without bloat.\n" +
		"  - Make the smallest change that does the job, then check it.\n" +
		"  - Apply maximal hardening and stress-test edge cases before completing.\n" +
		"  - Clarify decisions and surface impact early on complex or UI workflows.\n" +
		"  - When the task is done, stop and say what you changed. Do not keep calling tools\n" +
		"    to confirm work you have already confirmed.\n" +
		"  - If you cannot finish, say what is blocking you rather than doing something else\n" +
		"    instead."
}

// maxSteps bounds a turn. A ceiling in code, not a prompt instruction.
//
// 500, and it is a backstop rather than a work budget.
//
// 24 was measured wrong, not merely tight: against a local Qwen3.8-27B, turns
// were repeatedly cut off at exactly 24 steps while still making progress, so
// the harness was reporting "ran out of steps" for work it had no reason to
// stop. A real change is locate (5-15 searches) plus read (5-15) plus edit
// (5-20) plus two or three build-and-fix rounds of a dozen each — over a
// hundred steps before anything has gone wrong. A ceiling set near the middle
// of legitimate work is not a safety bound, it is a random failure mode, and
// the exit-2 that reports it stops meaning anything.
//
// 500 is where kon, a comparable harness, sits, and no smaller number can be
// justified from evidence here: what was measured is that 24 is too low, not
// where the real distribution ends.
//
// The turn is not held together by this number. Context is bounded by the
// budget and compaction (agent/compaction.go), verbatim churn by RepeatLimit,
// and near-duplicate churn by NoProgressLimit — and since agent.StallCost
// charges a step that changed nothing three units instead of one, a turn that
// never gets anywhere hits the ceiling after about 170 steps rather than 500.
// This is the last line, for the turn that is bounded by nothing else.
// MANVI_MAX_STEPS is the escape hatch, and it used to be read here straight
// from the environment with fmt.Sscanf(v, "%d", &n) — a second, weaker config
// path standing beside the registry every other setting goes through. Sscanf
// stops at the first byte it cannot use and reports success for what it
// consumed, so "12x" became 12 and "1e3" became 1: an operator writing 1e3
// meaning a thousand got a ceiling of one step, and every turn ended after a
// single tool call reporting "ran out of steps". Nothing distinguished that
// from work that genuinely hit the ceiling, and because the key was not in the
// catalogue, `manvi flags --all` could not show it and Lookup could not say
// where it came from.
//
// It is flags.MaxSteps now, so a malformed value is refused at boot by
// LoadEnv, a config file can set it, and its origin is reportable.
//
// What is left here is the one thing the registry's KindInt validation cannot
// express: zero and negatives parse as integers perfectly well and would end
// every turn before it began. Those fall back to the shipped ceiling rather
// than to something smaller — this is the last line for a turn bounded by
// nothing else, and no misconfiguration should be able to tighten it. The
// fallback is said out loud, once: a ceiling that quietly reverted is how the
// original defect stayed invisible, and the resolved value cannot change while
// the harness runs, so there is exactly one thing to say.
var warnMaxSteps sync.Once

func maxSteps(reg *flags.Registry) int {
	n, origin, err := reg.Int(flags.MaxSteps)
	if err != nil || n <= 0 {
		warnMaxSteps.Do(func() {
			fmt.Fprintf(os.Stderr,
				"manvi: %s is not a positive whole number (origin %s); using the default ceiling of %d\n",
				flags.MaxSteps, origin, flags.DefaultMaxSteps)
		})
		return flags.DefaultMaxSteps
	}
	return n
}

// coreToolsOnly reports whether to offer the reduced tool surface. It applies
// to the local provider only: the setting is about a smaller model choosing
// from a long list, and it names that provider for the same reason every other
// llm.local.* setting does.
func coreToolsOnly(reg *flags.Registry, provider string) bool {
	if provider != local.Name {
		return false
	}
	on, _, err := reg.Bool(flags.LLMLocalCoreToolsOnly)
	return err == nil && on
}

// taskCheckoutTool is the task lifecycle's entry point, and the one tool whose
// presence decides which task section the prompt carries.
const taskCheckoutTool = "devcouncil_checkout_task"

// taskToolsOffered reports whether the profile this turn will actually offer
// still contains the task-lifecycle entry point.
//
// It asks the registry rather than reading llm.local.core_tools_only, because
// the flag is a request for a smaller surface, not a statement about what is
// in it. The prompt used to compute `!coreToolsOnly(...)`, which was a second,
// hand-maintained copy of devcouncil's profile rule living in a different
// package from the rule — and it went stale the first time the rule moved.
// Closing the core profile under its own prerequisites made
// devcouncil_checkout_task core, so core_tools_only stopped removing it; the
// prompt went on saying "There is no task to check out here" while the tool
// sat in the model's list, and `manvi run --task T` held a lease while telling
// the model there was no task to hold.
//
// Derived from the same two inputs agent.Loop uses to pick the set, so the
// prompt and the offered tools cannot drift apart again without the loop's own
// selection changing with them.
func taskToolsOffered(pipeline *tools.Registry, coreOnly bool) bool {
	offered := pipeline.Schemas()
	if coreOnly {
		offered = pipeline.CoreSchemas()
	}
	for _, s := range offered {
		if s.Name == taskCheckoutTool {
			return true
		}
	}
	return false
}

func assertInvariant(reg *flags.Registry) bool {
	on, _, err := reg.Bool(flags.LogModelVisibleAssert)
	return err == nil && on
}

func dynamicToolsEnabled(reg *flags.Registry, provider string) bool {
	if provider != local.Name {
		return false
	}
	on, _, err := reg.Bool(flags.LLMLocalDynamicTools)
	return err == nil && on
}

func guidanceRouterEnabled(reg *flags.Registry, provider string) bool {
	if provider != local.Name {
		return false
	}
	on, _, err := reg.Bool(flags.LLMLocalGuidanceRouter)
	return err == nil && on
}
