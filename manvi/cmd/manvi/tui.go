package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/gate"
	"github.com/bharathvbcr/Manvi/manvi/agent"
	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/credentials"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/term"
	"github.com/bharathvbcr/Manvi/manvi/ui/tui"
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
	// storeID is the identifier this session is persisted under, which is not
	// the same string as id.
	//
	// id is a tab label — "S1", "S2" — assigned in the order tabs open, and it
	// is neither stable across runs nor a valid session identifier: the store
	// requires lowercase hex, and it refused every one of these. Separating the
	// two is what lets the label stay legible while the file stays resumable by
	// the same `manvi run --resume` the headless face uses.
	storeID string

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
	// tiers is the checked tier profile children of this session are placed
	// on, resolved at attach with the provider it was checked against; nil off
	// anthropic.
	tiers agents.TierProfile
}

// persist writes a session's log to the same store the headless face resumes
// from.
//
// Called at the end of every turn rather than at shutdown, because a face whose
// durability depends on exiting cleanly is not durable: the case that loses the
// work is the crash, the kill and the closed laptop, and none of them run a
// shutdown hook. Writing per turn also matches what the store is built for —
// checksummed generations, an atomic link, and the oldest whole turns dropped
// at the size bound.
func (h *harnessHost) persist(s *tuiSession) error {
	if s == nil || s.log == nil {
		return nil
	}
	if s.storeID == "" {
		// A session that never got an identifier cannot be written anywhere.
		// Reported rather than skipped: silently not saving is the behaviour
		// being replaced.
		return errors.New("this session has no durable identifier")
	}
	if _, err := h.sessionStore().Save(s.storeID, s.log); err != nil {
		return err
	}
	return nil
}

// attached is the resolved provider block, taken as one consistent snapshot.
type attached struct {
	provider      llm.Provider
	registry      *llm.Registry
	model         string
	effort        string
	effortCeiling string
	tiers         agents.TierProfile
}

// attachment reads the block under the lock. A caller that read the five fields
// separately could pick up a provider from before a settings change and a model
// from after it.
func (s *tuiSession) attachment() attached {
	s.attachMu.Lock()
	defer s.attachMu.Unlock()
	return attached{s.provider, s.registry, s.model, s.effort, s.effortCeiling, s.tiers}
}

// detach drops the resolved provider so the next turn resolves it again.
func (s *tuiSession) detach() {
	s.attachMu.Lock()
	defer s.attachMu.Unlock()
	s.provider, s.registry, s.model, s.effort, s.effortCeiling = nil, nil, "", "", ""
	s.tiers = nil
}

type harnessHost struct {
	reg    *flags.Registry
	runner *tui.Runner

	// busyTurns is injected in tests. tui.New refuses to build a runner
	// without a real terminal, so the alternative to a seam here is a guard
	// about concurrent turns that is never exercised by a test. Nil means ask
	// the runner, which is what every production path does.
	busyTurns func(exclude string) []string
	// sinks is injected in tests, for the same reason: without a runner every
	// event a command or a turn reports goes nowhere, so what the transcript
	// would have said could not be asserted at all. Nil means ask the runner.
	sinks func(sessionID string) ui.Sink

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

	// store persists every session this process runs. One for the process,
	// not one per save: a Store remembers the generation it last wrote, and
	// refuses to replace a generation it has no record of as another run's
	// work — so a store built fresh for each turn refused every save after the
	// first. Built on first use, against the directory current at that moment.
	storeMu  sync.Mutex
	store    *session.Store
	storeDir string
}

// sessionStore returns this process's session store for the current state
// directory.
func (h *harnessHost) sessionStore() *session.Store {
	h.storeMu.Lock()
	defer h.storeMu.Unlock()
	if dir := sessionsDir(); h.store == nil || h.storeDir != dir {
		h.store, h.storeDir = session.NewStore(dir), dir
	}
	return h.store
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
		{Name: "agents", Summary: "sub-agent roles, the tier each runs on, and that tier's model"},
		{Name: "budget", Summary: "Anthropic spend against MANVI_ANTHROPIC_BUDGET_USD"},
		{Name: "local", Args: "[--resolve] [--timeout DURATION]", Summary: "find local model servers and what they serve"},
		{Name: "pull", Summary: "fast-forward the current branch; refuses a dirty tree", Mutating: true},
		{Name: "push", Summary: "push the current branch to its configured upstream", Mutating: true},
		{Name: "issues", Summary: "report open GitHub issues, newest activity first"},
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
		log:    newSessionLog(),
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
	// Minted here rather than derived from the tab label, which is not a valid
	// identifier and is reused by the next run's first tab.
	storeID, err := session.NewID()
	if err != nil {
		return "", "", fmt.Errorf("this session could not be given a durable identifier: %w", err)
	}
	s.storeID = storeID

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
	// changed is the flag a `/flags set` moved, or "". Returned rather than acted
	// on inside runCommand because what a moved flag invalidates is this host's
	// business — the gates and providers it has already built — and none of the
	// command implementations know those exist.
	changed, err := h.runCommand(ctx, s, sessionID, name, fields, &out)

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

// runCommand runs one harness command, writing what it prints to out. It
// returns the flag a `/flags set` moved, or "".
func (h *harnessHost) runCommand(ctx context.Context, s *tuiSession, sessionID, name string,
	fields []string, out *bytes.Buffer) (string, error) {
	switch name {
	case "doctor":
		return "", doctor(out, h.reg)
	case "flags":
		if err := h.refuseFlagMoveDuringATurn(sessionID, fields); err != nil {
			return "", err
		}
		return flagsCommand(out, h.reg, fields, surfaceSession)
	case "providers":
		return "", showProviders(out, h.reg)
	case "agents":
		return "", showAgents(out, h.reg)
	case "budget":
		return "", budgetCommand(out, fields)
	case "local":
		return "", showLocal(out, h.reg, fields)
	case "pull", "push":
		if h.busyTurns != nil {
			if busy := h.busyTurns(sessionID); len(busy) > 0 {
				return "", fmt.Errorf("/%s needs every session idle; active sessions: %s", name, strings.Join(busy, ", "))
			}
		}
		return "", runQuickAction(ctx, out, projectRoot(), name, fields, osQuickCommandRunner{})
	case "issues":
		return "", runQuickAction(ctx, out, projectRoot(), name, fields, osQuickCommandRunner{})
	case "tools":
		return "", listTools(out, h.reg)
	case "leases":
		return "", lease(out, append([]string{"list"}, fields...))
	case "lease":
		if len(fields) == 0 {
			return "", lease(out, []string{"list"})
		}
		return "", lease(out, fields)
	case "check":
		// The session's own gate, so what this reports is what will
		// actually decide the session's next write.
		err := check(out, s.gate, fields)
		if errors.Is(err, errCheckBlocked) || errors.Is(err, errCheckHardBlocked) {
			// A block is this command's answer, not its failure. On the CLI
			// it is a non-zero exit status because a script has nothing else
			// to read; here the decision is already in the transcript above,
			// and reporting it a second time as an error would tell the
			// operator the command did not run.
			return "", nil
		}
		return "", err
	case "allow":
		return "", allow(out, s.gate, fields)
	case "tool":
		// The session's own pipeline: it carries this session's lease, and
		// its approval seam, so a blocked write raises a card here rather
		// than being refused by a registry nobody is watching.
		_, scrubber := h.creds()
		return "", callTool(out, out, scrubber, s.pipeline, fields)
	case "map":
		return "", mapCommand(out, h.reg, fields)
	case "probe":
		return "", probe(out, h.reg, fields)
	case "logo":
		return "", showLogo(out, fields)
	case "quit", "exit", "help", "clear", "cls":
		return "", nil
	}
	return "", fmt.Errorf("unknown command %q", name)
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
	if h.sinks != nil {
		return h.sinks(sessionID)
	}
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

	systemPrompt, subRunner, coreOnly, err := h.prepareTurn(s, sessionID, at)
	if err != nil {
		return err
	}

	// Taken before the turn runs, so what it spends on children can be told
	// apart from what earlier turns of this session already spent.
	before := s.subMeter.ByModel()

	loop, err := newTurnLoop(turnFace{
		maxTokens: tuiMaxTokens(h.reg),
		maxSteps:  maxSteps(h.reg),
		check: &sensor{
			native: s.native, log: s.log, flags: h.reg,
			runner:  subRunner,
			roles:   s.native.Roles(),
			command: operatorVerifyCommand(),
		},
	}, turnDeps{
		provider: at.provider, models: at.registry, model: at.model,
		effort: at.effort, effortCeiling: at.effortCeiling,
		systemPrompt:    systemPrompt,
		coreToolsOnly:   coreOnly,
		assertInvariant: assertInvariant(h.reg),
		log:             s.log, tools: s.pipeline,
	})
	if err != nil {
		return err
	}

	outcome, err := loop.Run(ctx, llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{llm.TextBlock{Text: text}},
	})
	// Persisted whether or not the turn succeeded, and before the error is
	// returned. A turn that failed part-way still produced a log, and that log
	// is the only account of what it did — losing it is losing the evidence for
	// the failure the operator is about to investigate.
	//
	// This face persisted nothing at all until now, while root.go's own comment
	// said "headless and TUI sessions persist". Only the headless path ever
	// called Save: closing the window discarded the whole conversation, and the
	// resume that the CLI advertises had no counterpart here.
	persistErr := h.persist(s)

	sink := h.sinkFor(sessionID)
	if persistErr != nil {
		// Reported, never silent. A session that is not being written to disk
		// looks exactly like one that is, right up to the moment someone needs
		// it back.
		sink.Emit(ui.Event{
			Kind: ui.KindNotice, At: time.Now().UTC(),
			Text: "this session is not being saved: " + persistErr.Error(),
		})
	}
	if err != nil {
		return err
	}
	h.reportTurn(sink, turnUsage(at.provider, at.model, outcome), s.subMeter.Since(before), outcome)
	return nil
}

// prepareTurn assembles what one submission's turn runs with — the system
// prompt, recorded with any fault in its assembly, and the sub-agent runner
// re-attached to the session's current provider, model and effort — and
// returns the prompt, the runner and whether the turn is narrowed to the core
// tools.
func (h *harnessHost) prepareTurn(s *tuiSession, sessionID string, at attached) (string, *subAgentRunner, bool, error) {
	coreOnly := coreToolsOnly(h.reg, at.provider.Name())
	dynamic := dynamicToolsEnabled(h.reg, at.provider.Name())
	if dynamic {
		s.pipeline.EnableDynamic()
	}
	subRunner, caps := harnessFor(s.pipeline)
	promptOpts := PromptOptions{
		Provider:           at.provider.Name(),
		TaskToolsOffered:   taskToolsOffered(s.pipeline, coreOnly),
		DynamicTools:       dynamic,
		ActiveGroups:       s.pipeline.ActiveGroups(),
		CodeMapAvailable:   codeMapAvailable(caps, s.pipeline, dynamic, coreOnly),
		DocLookupAvailable: caps.DocLookup,
		Areas:              caps.Areas,
	}
	systemPrompt, promptReport, promptFaults := assemblePromptReported(h.reg, promptOpts)
	// Surfaced in the transcript rather than on stderr. Under the alternate
	// screen stderr goes nowhere a person can read, which is how a prompt
	// missing four of its sections came to look identical to a whole one.
	promptSink := h.sinkFor(sessionID)
	for _, fault := range promptFaults {
		promptSink.Emit(ui.Event{
			Kind: ui.KindNotice, At: time.Now().UTC(),
			Text: "system prompt: " + fault,
		})
	}
	if err := recordPromptAssembly(s.log, promptReport,
		promptTokenBudget(h.reg, at.provider.Name())); err != nil {
		return "", nil, false, err
	}

	// Re-attached on every submission rather than once at session start: the
	// provider, model and effort are all switchable mid-session from this face,
	// and a child running under the model the user chose two turns ago would be
	// a surprise nothing reported.
	if runner := subRunner; runner != nil {
		runner.attach(subAgentConfig{
			provider:        at.provider,
			models:          at.registry,
			model:           at.model,
			effort:          at.effort,
			effortCeiling:   at.effortCeiling,
			tiers:           at.tiers,
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
	return systemPrompt, subRunner, coreOnly, nil
}

// reportTurn puts a finished turn's spend and outcome in front of the
// operator: one usage line covering the turn and the children it delegated to,
// then one line per outcome notice. delegated is what this turn's children
// spent, by model.
func (h *harnessHost) reportTurn(sink ui.Sink, own devcouncil.SubAgentUsage, delegated []devcouncil.SubAgentUsage, outcome agent.Outcome) {
	// This turn's delegated spend, not the session's: the meter accumulates
	// across every turn of the session, so the caller passes the difference
	// against the snapshot taken before the turn ran. Reporting the running
	// total per turn would count the first turn's children again in every turn
	// after it.
	sink.Emit(usageEvent(own, delegated))
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
}

// tuiMaxTokens is the output bound the TUI names for a turn: the operator's
// local cap when the local provider is selected, and 8192 otherwise.
//
// Unlike `manvi run`, which leaves the bound to the adapter, this face names
// one. The difference predates the shared constructor and is kept as it was;
// it is stated here so that it is a decision rather than a drift.
func tuiMaxTokens(reg *flags.Registry) int {
	if name, _, err := reg.String(flags.LLMDefaultProvider); err == nil && name == "local" {
		if capTokens, _, err := reg.Int(flags.LLMLocalMaxOutputTokens); err == nil && capTokens > 0 {
			return capTokens
		}
	}
	return 8192
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
			h.scrubber = newArmedScrubber(h.resolver)
		}
	})
	armScrubber(h.scrubber, h.resolver)
	return h.resolver, h.scrubber
}

// attachProvider resolves the provider, credential, and model for a session.
func (h *harnessHost) attachProvider(s *tuiSession) error {
	name, _, err := h.reg.String(flags.LLMDefaultProvider)
	if err != nil {
		return err
	}
	resolver, _ := h.creds()
	provider, err := buildProvider(name, h.reg, resolver, io.Discard, "")
	if err != nil {
		return err
	}
	// Checked at attach, as the effort is: a tier this provider cannot serve is
	// a configuration mistake, and found here it names the tier rather than
	// arriving as a refused request in the middle of a delegated turn.
	tiers, err := checkTiers(h.reg, name)
	if err != nil {
		return err
	}
	// Re-armed after the build, which is when the provider's own credential
	// requirement became known to the resolver. Before this call the scrubber
	// could not have been watching the key this session is about to use.
	h.creds()

	registry, err := registerSessionLLM(s.pipeline, provider)
	if err != nil {
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
	s.tiers = tiers.Profile()
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
	effort, key, _, err := mainEffort(h.reg, provider)
	if err != nil {
		return "", err
	}
	if effort == "" {
		return "", nil
	}
	// Resolve is the same capability gate the agent loop uses before every
	// request, so attach cannot accept a value the turn would then refuse.
	if _, _, err := registry.Resolve(provider, llm.Request{Model: model, Effort: effort}); err != nil {
		return "", fmt.Errorf("%s=%s: %w", flags.EnvKey(key), effort, err)
	}
	return effort, nil
}
