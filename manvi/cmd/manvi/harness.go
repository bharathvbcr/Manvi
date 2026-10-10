package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/repomap"
	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/credentials"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/mcp"
	"github.com/bharathvbcr/Manvi/manvi/tools"
	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// --- native tool surface ---

// nativeTools builds the DevCouncil tool surface over the same gate, store, and
// grant ledger every other command uses. There is no separate wiring for the
// CLI: `manvi tool` reaches exactly the code an agent reaches.
func nativeTools(reg *flags.Registry) (*devcouncil.Registry, *tools.Registry, error) {
	return nativeToolsWith(reg, nil)
}

// nativeToolsWith builds the same surface with an approval seam attached.
//
// A nil approver is the CLI's case and the headless case, and it is not a
// degraded mode: with nothing attached, a soft block is refused exactly as it
// was before this parameter existed.
func nativeToolsWith(reg *flags.Registry, approver ui.Approver) (*devcouncil.Registry, *tools.Registry, error) {
	g, subsystems, err := buildGate(reg)
	if err != nil {
		return nil, nil, err
	}
	root := projectRoot()

	core, err := bootHarnessCore(reg, root)
	if err != nil {
		return nil, nil, err
	}
	subRegistry := agents.NewRegistry()
	subMgr := agents.NewInstanceManager()

	// Built empty and attached later, once a provider and the tool registry
	// below exist. Until something attaches it the tool refuses, which is the
	// behaviour that replaced it reporting completed work it never did.
	subRunner := &subAgentRunner{}

	native, err := devcouncil.New(devcouncil.Deps{
		Store:          storeClient(),
		Gate:           g,
		Root:           root,
		LeaseTTL:       15 * time.Minute,
		VerifierBinary: toolBinary("MANVI_VERIFY_BINARY", "dcverify"),
		CoverageFile:   os.Getenv("MANVI_COVERAGE"),
		Map:            mapClient(root),
		Grep:           grepClient(root),
		// Operator scope only. See operatorFetchHosts: an allowlist the agent
		// could write into the repository would not be one.
		Fetch:            core.fetch,
		Subsystems:       subsystems,
		Approver:         approver,
		QuestionAsker:    questionAsker(approver),
		Artifacts:        core.artifacts,
		MCP:              core.mcp,
		SubagentRegistry: subRegistry,
		SubagentMgr:      subMgr,
		SubAgent:         subRunner,
	})
	if err != nil {
		_ = core.plugins.Close()
		return nil, nil, err
	}
	pipeline := core.tools
	// Armed before any tool can run. Every result the model sees, and every
	// result the session log writes to disk, goes through this.
	toolScrubber := credentials.NewScrubber()
	toolScrubber.WatchAll(credentials.NewResolver())
	pipeline.SetScrubber(toolScrubber.Clean)
	if err := native.Register(pipeline); err != nil {
		_ = core.plugins.Close()
		return nil, nil, err
	}
	caps := harnessCapability{
		CodeMapConfigured: native.CodeMapConfigured(),
		DocLookup:         docLookupAvailable(core.fetch, core.mcp),
	}
	if subsystems != nil {
		caps.Areas = subsystems.Areas()
	}
	// The manager spawns server subprocesses lazily and keeps them for the
	// life of the process. Close lives on the plugin kernel — the one place
	// every tool surface passes through — so servers die with their harness
	// instead of outliving it. CloseAll existed and was tested, but nothing
	// ever called it until this teardown was registered.
	onProcessExit(func() { _ = core.plugins.Close() })
	rememberHarness(pipeline, subRunner, caps, core)
	return native, pipeline, nil
}

// buildMCP constructs the MCP manager a tool surface will hold.
//
// Both settings it reads had been declared and read by nothing. mcp.enabled
// said "true" and there was no false: servers were discovered and registered on
// every run, and a consumer handed no manager built its own, so switching MCP
// off was not expressible at all. mcp.config named .devcouncil/mcp.json while
// discovery held that path hardcoded alongside two more, so pointing the
// setting anywhere else registered nothing and reported nothing.
//
// Off is carried as a manager that refuses rather than as a nil one, because
// nil is already spoken for and means the opposite — see mcp.NewDisabledManager.
// The refusal names the setting and where its value came from, so an operator
// reading "no servers are available" is told which of the two decided.
//
// The path is only held to existing when the operator typed it. The shipped
// default names a file most repositories do not have, and refusing to start
// over the absence of a file this harness guessed at would break every run that
// has never configured MCP at all.
func buildMCP(reg *flags.Registry, root string) (*mcp.Manager, error) {
	on, origin, err := reg.Bool(flags.MCPEnabled)
	if err != nil {
		return nil, err
	}
	if !on {
		return mcp.NewDisabledManager(root,
			fmt.Sprintf("%s=false (%s)", flags.MCPEnabled, origin)), nil
	}

	path, pathOrigin, err := reg.String(flags.MCPConfig)
	if err != nil {
		return nil, err
	}
	mgr := mcp.NewManager(root)
	// Discover reads declaration files and plugin manifests and never consults
	// its ctx; servers connect lazily, later, on their own contexts. The three
	// callers that reach here holding a ctx say so with a nolint:contextcheck
	// that is only true while that stays the case.
	if err := mgr.Discover(context.Background(), mcp.ConfigSource{
		Path:     path,
		Declared: pathOrigin != flags.OriginDefault,
	}); err != nil {
		return nil, fmt.Errorf("%s=%q (%s): %w", flags.MCPConfig, path, pathOrigin, err)
	}
	return mgr, nil
}

// questionAsker attaches pairing to the same seam the write gate escalates
// through, so devcouncil_ask_question reaches whoever answers this run's
// approvals — in the TUI, the session's own modal card.
//
// Nil when nothing is attached, which is the CLI's case and the headless case.
// That is not a degraded mode with a quiet default: the tool reports that no
// human was asked and names the defaults it assumed, rather than reporting an
// answer. Before this existed nothing in the harness ever set QuestionAsker, so
// every call took that unattended path — including in the attended TUI, where a
// human was sitting at the keyboard while the harness answered on their behalf
// and told the model they had.
func questionAsker(approver ui.Approver) devcouncil.QuestionAsker {
	if approver == nil {
		// Returned as an untyped nil so Deps.QuestionAsker is nil rather than a
		// non-nil interface holding a nil approver, which would pass the
		// handler's attached-check and then refuse every question.
		return nil
	}
	return devcouncil.ApproverAsker{Approver: approver}
}

// subRunners maps a built tool registry to the sub-agent runner wired into the
// devcouncil surface registered on it.
//
// nativeToolsWith already returns two values and both are load-bearing at every
// call site; threading a third through all of them to serve the one caller that
// resolves a provider would be a worse trade than this lookup. A caller that
// never resolves a provider simply never attaches, and the tool goes on
// refusing.
// Guarded because tool surfaces are built per session and the TUI builds them
// off the lock it takes to assign a tab number. Two tabs opening at once — a
// startup that restores several, or a fast hand on the shortcut — is two
// unsynchronised writes to the same map, which in Go is a process-ending panic
// rather than a lost entry. The maps were unguarded before this and the window
// was real; it had simply never been hit.
var harnessMu sync.Mutex

var subRunners = map[*tools.Registry]*subAgentRunner{}

// harnessCapability is what this build can actually do, resolved once where the
// dependencies are assembled.
//
// It is looked up by tool registry for the same reason subRunners is: the
// answers are decided in nativeToolsWith, where the code map, the MCP manager
// and the tool surface all exist together, and they are needed in the two
// faces, where none of them do. Threading three more values through every call
// site to serve two callers is the worse trade.
type harnessCapability struct {
	// CodeMapConfigured is whether a navigator is attached.
	CodeMapConfigured bool
	// DocLookup is whether anything here can fetch documentation.
	DocLookup bool
	// Areas is the repository's own shape, for orienting the model.
	Areas []repomap.AreaSummary
}

var harnessCaps = map[*tools.Registry]harnessCapability{}

var harnessCores = map[*tools.Registry]*harnessCore{}

// rememberHarness records one tool surface's runner, capabilities and plugin
// kernel together, under one lock, because they are written at one point and
// read at one point and splitting the guard would only invite them to diverge.
func rememberHarness(pipeline *tools.Registry, runner *subAgentRunner, caps harnessCapability, core *harnessCore) {
	harnessMu.Lock()
	defer harnessMu.Unlock()
	subRunners[pipeline] = runner
	harnessCaps[pipeline] = caps
	harnessCores[pipeline] = core
}

func harnessCoreFor(pipeline *tools.Registry) *harnessCore {
	harnessMu.Lock()
	defer harnessMu.Unlock()
	return harnessCores[pipeline]
}

// harnessFor returns what was recorded for a tool surface. A surface nothing
// recorded yields the zero values, which is the same answer the bare map
// lookups gave: no runner attached, and no capability claimed.
func harnessFor(pipeline *tools.Registry) (*subAgentRunner, harnessCapability) {
	harnessMu.Lock()
	defer harnessMu.Unlock()
	return subRunners[pipeline], harnessCaps[pipeline]
}

func listTools(out io.Writer, reg *flags.Registry) error {
	_, pipeline, err := nativeTools(reg)
	if err != nil {
		return err
	}
	readOnly := map[string]bool{}
	for _, s := range pipeline.ReadOnlySchemas() {
		readOnly[s.Name] = true
	}
	for _, s := range pipeline.Schemas() {
		access := "write"
		if readOnly[s.Name] {
			access = "read"
		}
		fmt.Fprintf(out, "%-34s %-6s %s\n", s.Name, access, firstSentence(s.Description))
	}
	fmt.Fprintf(out, "\n%d tools, all native — no Python process is involved.\n", len(pipeline.Schemas()))
	return nil
}

// callTool runs one tool through the caller's pipeline.
//
// The pipeline is passed in for the reason the gate is: an attended session's
// tools carry its lease and its approval seam. A pipeline built here would take
// a lease on a registry nothing later releases, and would answer a blocked write
// by refusing it rather than by asking the operator who is sitting there.
//
// The scrubber is passed in for the same reason: a session's is armed from the
// credentials that session resolved.
func callTool(out io.Writer, notes io.Writer, scrubber *credentials.Scrubber, pipeline *tools.Registry, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: manvi tool NAME [--json '{...}']")
	}
	if !pipeline.Has(args[0]) {
		return fmt.Errorf("no native tool %q (see: manvi tools)", args[0])
	}

	payload := flagValue(args, "--json")
	if payload == "" {
		payload = "{}"
	}
	if !json.Valid([]byte(payload)) {
		return fmt.Errorf("--json is not valid JSON: %s", payload)
	}

	result := pipeline.Run(context.Background(), tools.Call{
		ID: "cli", Name: args[0], Arguments: json.RawMessage(payload),
	})

	// A tool result is file contents, grep hits, or a command's stdout: bytes
	// this harness did not write, going to a terminal that executes control
	// sequences rather than showing them. It was printed verbatim, so a file
	// holding "\x1b[2J\x1b[1;1H" cleared the operator's screen and could redraw
	// a prompt over it — the exact scenario Sanitize's doc names, on the one
	// command that echoes untrusted bytes most directly.
	if scrubber == nil {
		scrubber = credentials.NewScrubber()
	}
	text := ui.Sanitize(scrubber.Clean(result.Text))

	if result.IsError {
		fmt.Fprintln(out, text)
		return errors.New("tool reported an error")
	}
	fmt.Fprintln(out, text)
	if result.GrantID != "" {
		// The qualification goes to the notes stream rather than to os.Stderr.
		// On the CLI that is stderr, so piping the tool's JSON stays clean; in
		// the TUI it is a buffer that becomes an event. Writing to os.Stderr
		// unconditionally put these bytes straight onto a terminal the painter
		// believes it owns, which overwrote whatever the frame had there.
		fmt.Fprintf(notes, "\n(allowed by %s from %s — not a clean pass)\n",
			result.GrantID, result.GrantedBy)
	}
	return nil
}

func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	return s
}
