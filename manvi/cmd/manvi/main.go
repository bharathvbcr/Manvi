// Command manvi is the entry point.
//
// It exists to make the pieces observable from a terminal rather than only
// from tests: what the gate decides and why, what the flag registry resolves
// and from where, who holds a lease, and what an override actually records.
// Everything it prints is the same code path the agent loop uses — there is no
// separate "demo" implementation, because a demo that diverges from the real
// path is a demo that lies.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/dcgrep"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/bootstrap"
	"github.com/bharathvbcr/Manvi/manvi/credentials"
)

const usage = `manvi — the DevCouncil execution harness

Usage:
  manvi                             Full-screen face: transcript, composer, approvals, dashboard
  manvi doctor                      Check configuration, store reachability, and weakened gates
  manvi gusset-check                Prove the linked Rust engine is dc-glob on Gusset
  manvi flags [--all]               Show settings, their values, and where each value came from
  manvi flags set KEY VALUE         Move a setting on human authority, for this process
  manvi lease list                  Show who holds what
  manvi lease acquire TASK OWNER    Take a lease (--ttl 15m)
  manvi lease release TASK TOKEN    Release a lease
  manvi check PATH [--task ID]      Evaluate a write against policy, and say why
  manvi allow PATH --reason TEXT    Grant a human override for a blocked write
  manvi tools                       List the native DevCouncil tools an agent can call
  manvi tool NAME [--json ARGS]     Call one natively, the same path an agent takes
  manvi providers                   Show model providers and whether a credential is present
  manvi budget                      Show Anthropic spend against MANVI_ANTHROPIC_BUDGET_USD
  manvi local [--resolve]           Find local model servers; --resolve prints what a run would use
  manvi pull                        Fast-forward the current branch; refuses a dirty tree
  manvi push                        Push the current branch to its configured upstream
  manvi issues                      Report open GitHub issues, newest activity first
  manvi watch [--json]              Render the event stream: a terminal face, or NDJSON for CI
  manvi run [-p] PROMPT             Drive one turn with no terminal: scripts, CI, benchmarks
  manvi tui                         Full-screen face: transcript, composer, approvals, dashboard
  manvi map build                   Index the repository for navigation and the neighbour rule
  manvi map rebuild                 Discard the index and rebuild it, when its analysis is in doubt
  manvi map status                  Report index freshness, areas, and how permissive the rule is
  manvi probe PROVIDER              Make one real request and check the live wire contract
  manvi serve [--posture P]         Serve the policy and local-LLM planes to a host over stdio
  manvi logo [--svg]                Print the mark, or emit it as SVG
  manvi version, --version          Print which build this is: version, revision, toolchain
  manvi help, -h, --help            Show this help

Commands that keep state — the face, run, serve, watch, doctor, tool, allow,
lease, and map build/rebuild — first prepare the repository they are standing
in: the state directory the harness writes into and, inside a git working tree,
the .gitignore rules that keep that directory out of a commit. It is
idempotent, it only ever adds, it never adds a rule that would ignore a tracked
file, and it reports on stderr what it changed. Every other command — help,
version, flags, logo, providers, budget, tools, check, an unknown command, a
missing or unknown subcommand, or '--help' after any command — leaves the
working tree as it found it. 'manvi tui' also brings the navigation index up to date, in the
background, so the first frame does not wait on a build whose cost scales with
the repository. Set MANVI_HARNESS_INIT_ENABLED=false to leave the working tree
untouched by those too.

Exit status:
  'manvi run' documents its own — see 'manvi run --help'. 'manvi check' reports
  the decision it made, because a pre-flight that exits 0 on a refusal is a
  pre-flight that passes everything it was added to catch:
  0  not blocked
  6  blocked by a rule a grant can clear — the 'manvi allow' line is printed
  7  blocked by a hard rule, which no grant clears by any authority
  1  the command itself failed

Options (any command):
  --yolo               Run in yolo posture: every gate off, hard rules included.
                       Credential paths, restricted paths, git safety and the
                       repository boundary are not enforced, and nothing is put
                       to you for approval. Writes can land anywhere this process
                       can write; every result says it was reached this way.

Environment:
  MANVI_STATE_DIR      harness state dir (default: .devcouncil) — the store,
                       the graph, and the grant ledger default to live here
  MANVI_STORE_BINARY   path to dcstore   (default: PATH, then crates/target)
  MANVI_STORE_DB       path to the store (default: <state dir>/state.sqlite)
  MANVI_VERIFY_BINARY  path to dcverify  (default: PATH, then crates/target)
  MANVI_MAP_BINARY     path to devmap    (default: PATH, then crates/target)
  MANVI_GRAPH          code graph path   (default: <state dir>/code_graph.json)
  MANVI_COVERAGE       Go coverprofile or LCOV report for the diff-coverage gate
  MANVI_MODEL          model id for 'manvi tui' (see: manvi providers)
  MANVI_TUI_THEME      dark | light | plain
  MANVI_<FLAG>         any setting from 'manvi flags'

Settings resolve lowest-first: the catalogue default, then
.devcouncil/config.yaml, then MANVI_<FLAG>. The file is a flat mapping of
dotted names to scalars ('llm.local.model: qwen3.8:27b-mlx') and is the one
thing under .devcouncil/ meant to be committed; the environment still wins
over it for a single run. 'manvi flags' shows where each value came from.
`

// processExitFns holds teardown work registered by the surfaces that own
// child processes, drained when run() returns. A slice under a mutex rather
// than a hook field on each command: the composition root is the one place
// that cannot forget to call it.
var processExitMu sync.Mutex

var processExitFns []func()

func onProcessExit(fn func()) {
	processExitMu.Lock()
	processExitFns = append(processExitFns, fn)
	processExitMu.Unlock()
}

func drainProcessExits() {
	processExitMu.Lock()
	fns := processExitFns
	processExitFns = nil
	processExitMu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// answerWriter is stdout with a memory of the first write that did not land.
//
// A command's answer on stdout is a record: `manvi flags`, `manvi tools`,
// `manvi lease list` and `manvi local --resolve` are all parsed by something,
// this repository's own verify.sh among them. Around a hundred and sixty
// writes produce those answers and none of them checked, so a full disk or a
// revoked handle truncated the answer and the command still exited 0 — a
// caller reading a short answer as the whole one. Checking here rather than at
// each of those writes is the same choice drainProcessExits makes a few lines
// up: the composition root is the one place that cannot forget.
//
// The error is still returned to the caller as well as remembered, so a writer
// that does check — the JSON sink, the terminal renderer, the --quiet answer —
// behaves exactly as it did.
type answerWriter struct {
	to  io.Writer
	err error
}

func (a *answerWriter) Write(p []byte) (int, error) {
	n, err := a.to.Write(p)
	if err != nil && a.err == nil {
		a.err = err
	}
	return n, err
}

// Underlying satisfies ui.Decorator, so colour and terminal detection ask the
// screen rather than this wrapper.
func (a *answerWriter) Underlying() io.Writer { return a.to }

// Err reports the first write that did not land, or nil.
func (a *answerWriter) Err() error { return a.err }

func main() { os.Exit(exitStatus(os.Stdout, os.Stderr, os.Args[1:])) }

// exitStatus runs one invocation and returns the status the process exits with.
//
// It is separate from main so that the composition can be driven by a test:
// the backstop under every command's answer, and the mapping from a finished
// run to an exit status, are the two things here that a caller depends on and
// neither was reachable while they lived inside main. What is left in main is
// one line with nothing in it to get wrong.
func exitStatus(stdout, notes io.Writer, args []string) int {
	answer := &answerWriter{to: stdout}
	err := run(answer, notes, args)

	if errors.Is(err, errUsage) {
		// A request for help is not a failure.
		fmt.Fprint(answer, runUsage)
		err = nil
	}

	// The backstop under every command's answer, and placed after the last of
	// those writes — the usage text above is one of them. A command that knows
	// which of its own writes was lost reports that itself and this rung stays
	// quiet; outputStatus is idempotent for exactly that reason.
	//
	// No scrubber: this error comes from the operating system naming a file
	// handle, and the commands whose output can carry a credential scrub it
	// before it reaches any writer.
	return statusFor(outputStatus(err, notes, nil, answer.Err()), notes)
}

// statusFor maps a finished run to the process's exit status.
//
// It is the counterpart to outcomeStatus, which decides what a turn ended as;
// this decides what a caller is told about it. Split out because the mapping
// is a contract — a benchmark branches on these numbers and the CLI reference
// documents them — and while it lived inline in main nothing could execute it.
func statusFor(err error, notes io.Writer) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errOutputCap):
		// Its own status, for the same reason errTruncated has one: the turn
		// ran and the answer is incomplete. Separate from 2 so a caller can
		// act on it — this one is fixed by a larger output cap, not by more
		// steps. The message was already written where it belongs.
		return 3
	case errors.Is(err, errTruncated):
		// Distinct from a failure, and distinct from success. The turn ran and
		// did not finish, and a caller that cannot tell those three apart will
		// commit half-done work. The message was already written where it
		// belongs, so this only sets the status.
		return 2
	case errors.Is(err, errUnfinished):
		// The turn ran and produced an answer that did not end on a finished
		// stop reason — a dropped connection, an unmapped status, or a
		// refusal. It exited 0, which is what a completed turn exits, so a
		// benchmark recorded a fragment as a result. The notice naming which
		// of the three it was is already on stderr.
		return 5
	case errors.Is(err, errNoAnswer):
		// The turn ran, hit neither ceiling, and produced nothing. It used to
		// exit 0, which is the worst of the four: a benchmark or CI step reads
		// that as the work having been done. The message was already written
		// where it belongs.
		return 4
	case errors.Is(err, errCheckBlocked):
		// `manvi check` refused a write, and said why on stdout. It exited 0,
		// which is what an allowed write exits, so `manvi check "$f" && commit`
		// treated every block as a pass. The decision and the `manvi allow`
		// line that clears it are already printed.
		return 6
	case errors.Is(err, errCheckHardBlocked):
		// As 6, but no grant clears it. Separate so a caller does not retry
		// after issuing an override that was never going to apply.
		return 7
	default:
		fmt.Fprintf(notes, "manvi: %v\n", err)
		return 1
	}
}

// run dispatches one invocation.
//
// notes is where anything that is not the command's answer goes: what the
// startup scaffolding changed, and any step of it that could not run. It is
// separate from out for the reason callTool's is — 'manvi tool --json' output
// gets piped, and a line about .gitignore in the middle of it is a line some
// script has to learn to skip.
func run(out, notes io.Writer, args []string) error {
	// Every subprocess the process spawned is reaped on the way out, however
	// the run ends — success, error, or a ceiling exit. Registered by the
	// surfaces that own children; drained here because this is the one frame
	// every invocation returns through.
	defer drainProcessExits()

	// Pulled out before the subcommand is read, so --yolo may sit on either
	// side of it. It is stripped rather than passed through because each
	// command parses its own arguments and an unrecognised one is an error in
	// several of them.
	args, yolo := takeYolo(args)

	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		fmt.Fprint(out, usage)
		return nil
	}

	// Alongside help, and for the same reason it is here rather than in the
	// switch below: which build this is cannot depend on the configuration
	// being loadable or the working tree being writable. A CI job asking a
	// half-configured harness which binary it just ran must still get an
	// answer.
	if len(args) > 0 && (args[0] == "--version" || args[0] == "version") {
		writeVersion(out)
		return nil
	}

	// A setting that no longer applies must not look like a setting that did.
	// This harness was called devharness until it was named manvi, and every
	// variable it reads was renamed with it; an environment still exporting the
	// old names would otherwise be silently ignored — including the safety
	// flags, which is the one class of setting whose silent loss is worst.
	if stale := flags.StaleEnv(os.Environ()); len(stale) > 0 {
		var b strings.Builder
		b.WriteString("these environment variables use the old prefix and are no longer read:\n")
		for _, v := range stale {
			fmt.Fprintf(&b, "  %s\t→ %s\n", v.Old, v.New)
		}
		b.WriteString("rename them, or unset them to accept the defaults")
		return errors.New(b.String())
	}

	// The same failure with the current prefix. A key removed from the
	// catalogue stops being read by LoadEnv, which iterates the flags that are
	// defined — so MANVI_LLM_PROVIDER_LOCAL_ENABLED would go from doing nothing
	// silently to doing nothing silently, and the operator who set it would
	// have no way to learn that the setting is gone or what replaced it.
	if retired := flags.RetiredEnv(os.Environ()); len(retired) > 0 {
		var b strings.Builder
		b.WriteString("these environment variables set settings this harness no longer has:\n")
		for _, r := range retired {
			fmt.Fprintf(&b, "  %s\t— %s\n", r.Env, r.Why)
		}
		b.WriteString("unset them")
		return errors.New(b.String())
	}

	reg, err := flags.NewHarnessRegistry(configPath())
	if err != nil {
		return err
	}

	// On human authority, because a command line is a human at a keyboard —
	// and because the posture is human-only precisely so that nothing the
	// model emits can take this path. The registry records the origin as an
	// override, which is what doctor, `manvi flags`, and the WEAKENED list
	// then report.
	if yolo {
		if err := reg.Set(flags.Human, flags.HarnessPosture, flags.PostureYolo); err != nil {
			return err
		}
	}

	// Boot is over: the config file, the environment, and --yolo have all had
	// their say. Sealing freezes the startup flags, which is what the Startup
	// mutability has always claimed and what nothing enforced — Seal had no
	// production caller at all, so 'fixed once Boot completes' was true only of
	// flags nobody could move anyway. Two of the four are safety flags
	// (policy.hard_rules.enabled, log.model_visible_assert), and both become
	// reachable the moment 'flags set' exists. Sealing here is what keeps
	// "hard rules cannot be switched off mid-run" a fact rather than a comment.
	reg.Seal()

	// Before the command runs, for every command that keeps state, because
	// both failures this prevents are silent ones. Without the state directory
	// the store cannot be opened, so lease checks refuse for a reason that has
	// nothing to do with the lease; without the ignore rules the index, the
	// grant ledger, and the code graph go into the next commit.
	//
	// It reports rather than returns: scaffolding is not a gate, and a harness
	// that refused to run 'manvi doctor' because it could not write .gitignore
	// would be failing at the diagnosis the operator asked for.
	if preparesRepository(args) {
		report, err := scaffold(reg)
		if err != nil {
			return err
		}
		for _, line := range report.Lines() {
			fmt.Fprintf(notes, "manvi: %s\n", line)
		}
	}

	if len(args) == 0 {
		return runTUI(reg, nil)
	}

	switch args[0] {
	case "doctor":
		return doctor(out, reg)
	case "gusset-check":
		return gussetCheck(out)
	case "flags":
		_, err := flagsCommand(out, reg, args[1:], surfaceShell)
		return err
	case "lease":
		return lease(out, args[1:])
	case "check":
		return withGate(out, reg, args[1:], check)
	case "allow":
		return withGate(out, reg, args[1:], allow)
	case "tools":
		return listTools(out, reg)
	case "tool":
		_, pipeline, err := nativeTools(reg)
		if err != nil {
			return err
		}
		scrubber := credentials.NewScrubber()
		scrubber.WatchAll(credentials.NewResolver())
		return callTool(out, os.Stderr, scrubber, pipeline, args[1:])
	case "providers":
		return showProviders(out, reg)
	case "budget":
		return showBudget(out)
	case "local":
		return showLocal(out, reg, args[1:])
	case "pull", "push", "issues":
		return runQuickAction(context.Background(), out, projectRoot(), args[0], args[1:], osQuickCommandRunner{})
	case "watch":
		return watch(out, reg, args[1:])
	case "run":
		return runHeadless(out, notes, reg, args[1:])
	case "tui":
		return runTUI(reg, args[1:])
	case "map":
		return mapCommand(out, reg, args[1:])
	case "probe":
		return probe(out, reg, args[1:])
	case "serve":
		return serveCommand(out, reg, args[1:])
	case "logo":
		return showLogo(out, args[1:])
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
	}
}

// --- configuration ---

func storeClient() *store.Client {
	return store.New(toolBinary("MANVI_STORE_BINARY", "dcstore"), storeDBPath())
}

// mapClient builds the repo-navigation client.
func mapClient(root string) *devmap.Client {
	return devmap.New(toolBinary("MANVI_MAP_BINARY", "devmap"), root)
}

// grepClient builds the repository-search client.
func grepClient(root string) *dcgrep.Client {
	return dcgrep.New(toolBinary(dcgrep.BinaryEnv, "dcgrep"), root)
}

// preparesRepository reports whether the command in args keeps state — opens
// the store, writes the grant ledger or the index, or drives a session that
// does — and so needs the repository prepared before it runs.
//
// Everything else is left exactly as it was found: help and the version, which
// return before this is asked; the flag table, the logo, the provider and tool
// listings, 'check' (it reads the ledger and the graph, and writes neither); an
// unknown command; and a subcommand that is missing or unknown. Scaffolding
// used to run for all of them, so an agent probing a checkout with 'manvi
// logo' or a typo appended ignore rules to a .gitignore that was not the
// harness's to edit.
//
// The default is to leave the tree alone. A command added later writes nothing
// until it is listed here, which fails toward a store that says it cannot open
// rather than toward a file nobody asked to have changed.
func preparesRepository(args []string) bool {
	if len(args) == 0 {
		return true // the bare command is the full-screen face
	}
	if len(args) > 1 && (args[1] == "-h" || args[1] == "--help") {
		return false
	}
	switch args[0] {
	case "tui", "run", "serve", "watch", "doctor":
		return true
	case "tool", "allow":
		// Each needs an argument to do anything; without one it is a usage
		// error, and a usage error has nothing to keep.
		return len(args) > 1
	case "lease":
		return len(args) > 1 && (args[1] == "list" || args[1] == "acquire" || args[1] == "release")
	case "map":
		// 'map status' reads the index; only a build writes one.
		return len(args) > 1 && (args[1] == "build" || args[1] == "rebuild")
	}
	return false
}

// scaffold prepares the repository this invocation is standing in.
//
// The artifact paths are handed over so their parents are created too: an
// operator who pointed MANVI_STORE_DB or MANVI_GRAPH outside the state
// directory should not meet that decision as a failure from sqlite about a
// directory the harness could have made.
func scaffold(reg *flags.Registry) (bootstrap.Report, error) {
	on, _, err := reg.Bool(flags.HarnessInitEnabled)
	if err != nil {
		return bootstrap.Report{}, err
	}
	if !on {
		return bootstrap.Report{}, nil
	}
	root := projectRoot()
	return bootstrap.Ensure(root, bootstrap.Options{
		StateDir:      stateDir(),
		ArtifactPaths: []string{storeDBPath(), graphArtifactPath(), repoMapArtifactPath(), grantLedgerPath()},
		Gitignore:     true,
	}), nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// --- arg helpers ---

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
	}
	return ""
}

// takeYolo removes every --yolo from args and reports whether one was there.
//
// It returns a fresh slice rather than filtering in place: the caller's
// arguments are os.Args[1:], and a helper that rewrites the process's own
// argument vector is a surprise nobody reading the call site would expect.
func takeYolo(args []string) ([]string, bool) {
	kept := make([]string, 0, len(args))
	found := false
	for _, a := range args {
		if a == "--yolo" {
			found = true
			continue
		}
		kept = append(kept, a)
	}
	return kept, found
}
