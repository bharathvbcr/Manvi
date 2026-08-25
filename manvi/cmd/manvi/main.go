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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"manvi/agents"
	"manvi/artifacts"
	"manvi/bootstrap"
	"manvi/core/bus"
	"manvi/credentials"
	"manvi/dc"
	"manvi/dc/devmap"
	"manvi/dc/store"
	"manvi/devcouncil"
	"manvi/flags"
	"manvi/gate"
	"manvi/grants"
	"manvi/llm/local"
	"manvi/mcp"
	"manvi/policy"
	"manvi/repomap"
	"manvi/tools"
	"manvi/ui"
)

const usage = `manvi — the DevCouncil execution harness

Usage:
  manvi                             Full-screen face: transcript, composer, approvals, dashboard
  manvi doctor                      Check configuration, store reachability, and weakened gates
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
  manvi local [--resolve]           Find local model servers; --resolve prints what a run would use
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

Every command first prepares the repository it is standing in: the state
directory the harness writes into, and the managed rules in .gitignore. It is
idempotent, it only ever adds, and it reports on stderr what it changed. 'manvi
tui' also brings the navigation index up to date, in the background, so the
first frame does not wait on a build whose cost scales with the repository.
Set MANVI_HARNESS_INIT_ENABLED=false to leave the working tree untouched.

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

func main() {
	err := run(os.Stdout, os.Stderr, os.Args[1:])
	switch {
	case err == nil:
		return
	case errors.Is(err, errUsage):
		// A request for help is not a failure.
		fmt.Fprint(os.Stdout, runUsage)
		return
	case errors.Is(err, errOutputCap):
		// Its own status, for the same reason errTruncated has one: the turn
		// ran and the answer is incomplete. Separate from 2 so a caller can
		// act on it — this one is fixed by a larger output cap, not by more
		// steps. The message was already written where it belongs.
		os.Exit(3)
	case errors.Is(err, errTruncated):
		// Distinct from a failure, and distinct from success. The turn ran and
		// did not finish, and a caller that cannot tell those three apart will
		// commit half-done work. The message was already written where it
		// belongs, so this only sets the status.
		os.Exit(2)
	case errors.Is(err, errUnfinished):
		// The turn ran and produced an answer that did not end on a finished
		// stop reason — a dropped connection, an unmapped status, or a
		// refusal. It exited 0, which is what a completed turn exits, so a
		// benchmark recorded a fragment as a result. The notice naming which
		// of the three it was is already on stderr.
		os.Exit(5)
	case errors.Is(err, errNoAnswer):
		// The turn ran, hit neither ceiling, and produced nothing. It used to
		// exit 0, which is the worst of the four: a benchmark or CI step reads
		// that as the work having been done. The message was already written
		// where it belongs.
		os.Exit(4)
	case errors.Is(err, errCheckBlocked):
		// `manvi check` refused a write, and said why on stdout. It exited 0,
		// which is what an allowed write exits, so `manvi check "$f" && commit`
		// treated every block as a pass. The decision and the `manvi allow`
		// line that clears it are already printed.
		os.Exit(6)
	case errors.Is(err, errCheckHardBlocked):
		// As 6, but no grant clears it. Separate so a caller does not retry
		// after issuing an override that was never going to apply.
		os.Exit(7)
	default:
		fmt.Fprintf(os.Stderr, "manvi: %v\n", err)
		os.Exit(1)
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

	// Before the command runs, and for every command, because both failures
	// this prevents are silent ones. Without the state directory the store
	// cannot be opened, so lease checks refuse for a reason that has nothing to
	// do with the lease; without the ignore rules the index, the grant ledger,
	// and the code graph go into the next commit.
	//
	// It reports rather than returns: scaffolding is not a gate, and a harness
	// that refused to run 'manvi doctor' because it could not write .gitignore
	// would be failing at the diagnosis the operator asked for.
	report, err := scaffold(reg)
	if err != nil {
		return err
	}
	for _, line := range report.Lines() {
		fmt.Fprintf(notes, "manvi: %s\n", line)
	}

	if len(args) == 0 {
		return runTUI(reg, nil)
	}

	switch args[0] {
	case "doctor":
		return doctor(out, reg)
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
	case "local":
		return showLocal(out, reg, args[1:])
	case "watch":
		return watch(reg, args[1:])
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

// --- doctor ---

// doctor reports what the harness would do, and refuses to imply health it has
// not established. A check that could not run is printed as such rather than
// omitted, because an omitted check reads as a passing one.
func doctor(out io.Writer, reg *flags.Registry) error {
	fmt.Fprintln(out, "manvi doctor")
	fmt.Fprintln(out)

	// First, because every other line below describes the configuration of a
	// binary this one names. A report that does not say which build produced it
	// cannot be compared against another run's.
	fmt.Fprintf(out, "  build           %s\n", currentBuild().Summary())

	posture, postureOrigin, err := reg.String(flags.HarnessPosture)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  posture         %s (%s)\n", posture, postureOrigin)
	for _, line := range wrapNotice(flags.DescribePosture(posture).Notice, 60) {
		fmt.Fprintf(out, "                  %s\n", line)
	}

	// The *effective* mode, resolved exactly as the gate resolves it. Printing
	// the raw flag while the posture overrides it would be a report that
	// misleads, which is worse than no report.
	mode, origin, err := flags.EffectiveGateMode(reg, flags.PolicyFileMode)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  file gate       %s (%s)\n", mode, origin)

	cmdMode, cmdOrigin, err := flags.EffectiveGateMode(reg, flags.PolicyCommandMode)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  command gate    %s (%s)\n", cmdMode, cmdOrigin)

	// Resolved, not raw, for the same reason the gate modes are: the yolo
	// posture turns these off without moving the flag, and a doctor that read
	// the flag would print "on" for rules that are not running.
	hard, hardOrigin, err := flags.EffectiveHardRules(reg)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  hard rules      %s (%s)\n", onOff(hard), hardOrigin)
	if !hard {
		fmt.Fprintln(out, "                  credential paths, restricted paths, git safety, and the")
		fmt.Fprintln(out, "                  repository boundary are not enforced; writes can land")
		fmt.Fprintln(out, "                  anywhere, and every decision is recorded as unchecked")
	}

	agentGrants, _, err := reg.Bool(flags.GrantsAgentEnabled)
	if err != nil {
		return err
	}
	ttl, _, err := reg.Duration(flags.GrantsAgentMaxTTL)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  agent overrides %s (max %s)\n", onOff(agentGrants), ttl)

	// Reported as what will actually be sent, for the same reason the gate
	// modes are reported resolved rather than raw. supports_reasoning only
	// permits the reasoning_effort field; llm.effort is what fills it. Set one
	// without the other and both settings read back exactly as written while no
	// reasoning is requested at all — a configuration that looks on and behaves
	// off, which is the one thing doctor exists to catch.
	reasoning, _, err := reg.Bool(flags.LLMLocalSupportsReasoning)
	if err != nil {
		return err
	}
	effort, _, err := reg.String(flags.LLMEffort)
	if err != nil {
		return err
	}
	switch {
	case !reasoning:
		fmt.Fprintf(out, "  reasoning       off (%s=false)\n", flags.LLMLocalSupportsReasoning)
	case effort == "":
		fmt.Fprintln(out, "  reasoning       not requested")
		fmt.Fprintf(out, "                  %s is on, but %s is empty, so no\n",
			flags.LLMLocalSupportsReasoning, flags.LLMEffort)
		fmt.Fprintln(out, "                  reasoning_effort is sent and the model reasons at its own")
		fmt.Fprintf(out, "                  default; set %s to ask for it\n", flags.LLMEffort)
	default:
		fmt.Fprintf(out, "  reasoning       requested at effort %q\n", effort)
		// The ceiling belongs beside the base, not in a separate line item: a
		// turn that goes in circles may climb from one to the other, and an
		// operator reading only the base would be told half the answer.
		ceiling, _, err := reg.String(flags.LLMEffortCeiling)
		if err != nil {
			return err
		}
		if ceiling == "" {
			fmt.Fprintf(out, "                  fixed for the whole turn; set %s to let a\n",
				flags.LLMEffortCeiling)
			fmt.Fprintln(out, "                  turn that goes in circles raise it")
		} else {
			fmt.Fprintf(out, "                  rising to at most %q if calls are refused for\n", ceiling)
			fmt.Fprintf(out, "                  going in circles (%s)\n", flags.LLMEffortCeiling)
		}
	}

	// Named before the settings that came out of it. "Why is this value what it
	// is" is the question doctor exists to answer, and until now the config
	// layer was never one of the possible answers.
	fmt.Fprintf(out, "  config          %s\n", configPath())
	if keys := reg.ConfiguredKeys(); len(keys) > 0 {
		fmt.Fprintf(out, "                  read, %d setting(s): %s\n", len(keys), strings.Join(keys, ", "))
	} else {
		fmt.Fprintln(out, "                  absent — every setting comes from the environment or its default")
	}

	verifier := toolBinary("MANVI_VERIFY_BINARY", "dcverify")
	fmt.Fprintf(out, "  verifier        %s\n", verifier)
	if err := (rigorProbe{verifier}).check(); err != nil {
		fmt.Fprintf(out, "                  UNAVAILABLE: %v\n", err)
		fmt.Fprintln(out, "                  secret_scan, stub_detection, and diff_coverage will report as degraded")
	} else {
		fmt.Fprintln(out, "                  reachable — secret_scan, stub_detection, diff_coverage will run")
	}

	client := storeClient()
	fmt.Fprintf(out, "  store           %s -> %s\n", client.Binary, client.DB)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Available(ctx); err != nil {
		fmt.Fprintf(out, "                  UNAVAILABLE: %v\n", err)
		fmt.Fprintln(out, "                  lease checks cannot run; writes that need one will be refused")
	} else {
		leases, _ := client.ActiveLeases(ctx)
		fmt.Fprintf(out, "                  reachable, %d active lease(s)\n", len(leases))
	}

	reportDevMap(out, ctx, mapClient(projectRoot()))

	fmt.Fprintln(out)
	for _, status := range credentials.NewResolver().Statuses() {
		// Reported below instead, under its own heading. A loopback server
		// needs no key, so "local — no credential" is a true statement about
		// nothing, printed directly above the line that answers the question
		// an operator was actually asking.
		if status.Provider == local.Name {
			continue
		}
		if status.Present {
			fmt.Fprintf(out, "  %-15s credential from %s (%d chars)\n",
				status.Provider, status.Source, status.Length)
		} else {
			fmt.Fprintf(out, "  %-15s no credential — %s\n", status.Provider, status.Detail)
		}
	}

	// Named separately from the credential list above, because the credential
	// list answers a question this provider does not have: a loopback server
	// needs no key, so "no credential" said nothing about whether a local model
	// was available to run against.
	fmt.Fprintf(out, "  %-15s %s\n", "local models", describeLocalReadiness(reg))

	// A green run assembled under relaxed settings is not a green run.
	if weak := reg.Weakened(); len(weak) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "  WEAKENED — results produced under these settings are not strict:")
		for _, v := range weak {
			fmt.Fprintf(out, "    %s = %s (%s)\n", v.Key, v.Raw, v.Origin)
		}
	}
	return nil
}

// --- providers ---

// showProviders reports which model backends could actually be used. It prints
// the source variable and the key's length, never the key: a diagnostic that
// discloses the thing it is diagnosing is a diagnostic nobody can paste into a
// bug report.
func showProviders(out io.Writer, reg *flags.Registry) error {
	resolver := credentials.NewResolver()
	fmt.Fprintln(out, "providers")
	fmt.Fprintln(out)
	for _, status := range resolver.Statuses() {
		// The local provider's readiness is not a credential question. Its
		// servers are on loopback and ignore authorisation, so the credential
		// line reported "unavailable" for a provider that was working — the
		// one report an operator most needed to be right, since local is the
		// provider they are least likely to have configured on purpose.
		if status.Provider == local.Name {
			fmt.Fprintf(out, "  %-12s %s\n", status.Provider, describeLocalReadiness(reg))
			continue
		}
		if status.Present {
			fmt.Fprintf(out, "  %-12s ready — credential from %s (%d chars)\n",
				status.Provider, status.Source, status.Length)
			continue
		}
		fmt.Fprintf(out, "  %-12s unavailable\n", status.Provider)
		fmt.Fprintf(out, "               %s\n", status.Detail)
	}
	return nil
}

// describeLocalReadiness reports whether a local model server can be reached,
// which is what "ready" means for this provider.
//
// It is bounded tightly and never fails the command: `manvi providers` is a
// configuration report, and a scan that could not finish must degrade to
// saying so rather than turn the whole listing into an error.
func describeLocalReadiness(reg *flags.Registry) string {
	// Settings first, before any server is contacted.
	//
	// doctor is the one command whose whole job is "is my configuration
	// sound", and it used to answer "ready" for a configuration no turn could
	// use: MANVI_LLM_LOCAL_TEMPERATURE=0.7x reported two healthy servers and
	// exited 0, leaving the failure to arrive on the first turn instead. A
	// server answering on a port says nothing about whether the sampling
	// settings will encode, so both questions are asked and the one that
	// cannot be satisfied is the one reported.
	//
	// localConfig is the same resolution a real run performs, so this cannot
	// drift from what it is certifying.
	if _, err := localConfig(reg); err != nil {
		return "misconfigured — " + err.Error()
	}

	declared, origin, err := reg.String(flags.LLMLocalBaseURL)
	if err != nil {
		return "unavailable — " + err.Error()
	}
	resolver := credentials.NewResolver()
	resolve := func() (credentials.Secret, error) { return resolver.Resolve(local.Name) }

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// A pinned address is asked through Scan rather than surveyed directly, so
	// that it inherits the one-attempt policy speculative probes use. A
	// readiness report is not a request that has to succeed, and retrying a
	// refused loopback connection four times made `manvi doctor` sit for three
	// seconds to print a line it already knew.
	opts := local.ScanOptions{Credential: resolve}
	if origin != flags.OriginDefault {
		opts.Endpoints = []local.Endpoint{{BaseURL: declared}}
	}
	servers := local.Scan(ctx, opts)
	switch {
	case len(servers) == 0 && origin != flags.OriginDefault:
		return fmt.Sprintf("unavailable — nothing answered %s (%s)", declared, origin)
	case len(servers) == 0:
		return "unavailable — no local server answered; run 'manvi local'"
	case len(servers) == 1:
		return fmt.Sprintf("ready — %s, %d model(s)", servers[0].Describe(), len(servers[0].Models))
	default:
		return fmt.Sprintf("ready — %d servers found; run 'manvi local'", len(servers))
	}
}

// --- watch ---

// watch renders the harness event stream.
//
// It is here to make the structural point executable rather than only
// documented: the terminal is not a privileged consumer. The same events drive
// the JSON face, so anything visible here is visible to a CI job or an editor
// without a second code path to keep in step.
func watch(reg *flags.Registry, args []string) error {
	asJSON := slices.Contains(args, "--json")

	scrubber := credentials.NewScrubber()
	scrubber.WatchAll(credentials.NewResolver())

	var sink ui.Sink
	if asJSON {
		sink = ui.NewJSONSink(os.Stdout, scrubber)
	} else {
		sink = ui.NewRenderer(os.Stdout, scrubber)
	}

	posture, _, err := reg.String(flags.HarnessPosture)
	if err != nil {
		return err
	}
	model, _, err := reg.String(flags.LLMDefaultProvider)
	if err != nil {
		return err
	}
	sink.Emit(ui.Event{Kind: ui.KindSessionStart, At: time.Now().UTC(), Posture: posture, Model: model})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := storeClient()
	if err := client.Available(ctx); err != nil {
		sink.Emit(ui.Event{Kind: ui.KindError, At: time.Now().UTC(),
			Text: fmt.Sprintf("store unreachable: %v", err)})
		sink.Emit(ui.Event{Kind: ui.KindNotice, At: time.Now().UTC(),
			Text: "lease checks cannot run; writes that need one will be refused"})
		return nil
	}
	leases, err := client.ActiveLeases(ctx)
	if err != nil {
		return err
	}
	if len(leases) == 0 {
		sink.Emit(ui.Event{Kind: ui.KindNotice, At: time.Now().UTC(), Text: "no active leases"})
	}
	for _, lease := range leases {
		sink.Emit(ui.Event{
			Kind: ui.KindLease, At: time.Now().UTC(), TaskID: lease.TaskID,
			Text: fmt.Sprintf("%s held by %s until %s", lease.TaskID, lease.Owner, lease.ExpiresAt),
		})
	}

	ready, err := client.ReadyTasks(ctx)
	if err != nil {
		return err
	}
	sink.Emit(ui.Event{Kind: ui.KindReport, At: time.Now().UTC(),
		Text: fmt.Sprintf("%d task(s) ready, %d lease(s) active", len(ready), len(leases))})
	return nil
}

// --- map ---

// mapCommand builds or reports the repo-navigation index.
func mapCommand(out io.Writer, reg *flags.Registry, args []string) error {
	client := mapClient(projectRoot())
	ctx := context.Background()

	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "build":
		return mapBuild(out, client, ctx)
	case "rebuild":
		// The remedy for an index whose analysis cannot be trusted.
		//
		// devmap builds incrementally, and an index committed by an older
		// binary keeps whatever that binary concluded: a build that finds the
		// tree unchanged re-derives nothing, so a wrong dead-code or subsystem
		// result survives every subsequent build. Nothing can detect that from
		// outside — recomputing is the only way to know — so the operator is
		// given the way to discard it rather than being told to delete files by
		// hand. Only the derived index goes; the artifacts are rewritten from
		// the rebuild that follows.
		if err := os.RemoveAll(indexDir()); err != nil {
			return fmt.Errorf("discarding the index at %s: %w", indexDir(), err)
		}
		fmt.Fprintf(out, "discarded %s; rebuilding from nothing\n", indexDir())
		return mapBuild(out, client, ctx)
	case "status", "":
		return mapStatus(out, client, ctx, nil)
	default:
		return fmt.Errorf("unknown map subcommand %q (build, rebuild, status)", sub)
	}
}

// mapBuild rebuilds the index and reports what it did and did not cover.
func mapBuild(out io.Writer, client *devmap.Client, ctx context.Context) error {
	report, err := refreshIndex(ctx, client)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, describeIndex(report))
	// Kept so the status report below does not repeat what this one just said.
	// The two describe the same artifact from two directions — what the build
	// wrote, and what is on disk now — and a fact stated twice in one command's
	// output reads as a rendering bug rather than as emphasis.
	said := map[string]bool{}
	for _, line := range report.Degraded() {
		said[line] = true
		fmt.Fprintf(out, "                %s\n", line)
	}
	fmt.Fprintf(out, "wrote %s\n", graphArtifactPath())
	return mapStatus(out, client, ctx, said)
}

// refreshIndex rebuilds the index and rewrites the artifacts read from it.
//
// The two steps are one function because they carry one fact between them: the
// gate's neighbour rule reads the code graph, not the index, so a build without
// the manifest leaves the gate deciding from the previous repository while
// every navigation tool answers about the current one.
func refreshIndex(ctx context.Context, client *devmap.Client) (*devmap.BuildReport, error) {
	report, err := client.Build(ctx, 15*time.Minute)
	if err != nil {
		return nil, &refreshError{stage: stageIndex, err: err}
	}
	manifest, err := client.Manifest(ctx, repoMapArtifactPath(), graphArtifactPath())
	if err != nil {
		return nil, &refreshError{stage: stageArtifacts, err: err}
	}
	// One artifact, one account of it: the two commands are a single operation
	// to the caller, so what either of them left out belongs to one report.
	report.Merge(manifest.Notices)
	report.Disagreements = append(report.Disagreements, manifest.Disagreements...)
	report.Disagreements = append(report.Disagreements,
		artifactDisagreements(graphArtifactPath(), manifest.GenerationID)...)
	report.Adopted = append(report.Adopted, manifest.Adopted...)
	return report, nil
}

// refreshStage names which half of a refresh failed.
//
// The two halves are two commands writing two things with two lifetimes, and
// they degrade different capabilities: `devmap build` advances the index the
// navigation tools query, `devmap manifest` writes the code graph the gate's
// scope rung reads. A failure in the second used to be reported as "the
// repository index could not be built" — a sentence about an index that had
// just been built successfully, naming a capability that was in fact working
// and not naming the one that was not. An operator following it went to look
// at the index.
type refreshStage int

const (
	stageIndex refreshStage = iota
	stageArtifacts
)

// refreshError is a refresh failure that knows which command produced it.
type refreshError struct {
	stage refreshStage
	err   error
}

func (e *refreshError) Unwrap() error { return e.err }

func (e *refreshError) Error() string {
	switch e.stage {
	case stageArtifacts:
		return "the repository index was rebuilt, but the artifacts read from it could not be written: " +
			e.err.Error()
	default:
		return "the repository index could not be built: " + e.err.Error()
	}
}

// degraded names what will answer wrongly until the failure is dealt with.
//
// Split by stage for the same reason the message is. After a failed build
// nothing has current data. After a failed manifest the index is current and
// the tools that query it are fine; it is the file on disk — the one the gate
// reads, and the one the next session loads — that is stale or absent.
func (e *refreshError) degraded() []string {
	if e.stage == stageArtifacts {
		return []string{
			"navigation tools answer from the rebuilt index, but the scope rung decides neighbour " +
				"writes from the code graph on disk, which is whatever was there before",
		}
	}
	return []string{"navigation tools and the neighbour rule will report unavailable"}
}

// refreshFailure renders a refresh error for the transcript: what happened, and
// what is degraded while it stands.
//
// It accepts a plain error because refreshIndex is not the only thing that can
// fail here and an error from somewhere else must still be reported rather than
// swallowed for not being the expected type.
func refreshFailure(err error) (string, []string) {
	var staged *refreshError
	if errors.As(err, &staged) {
		return staged.Error(), staged.degraded()
	}
	return "the repository index could not be refreshed: " + err.Error(),
		[]string{"navigation tools and the neighbour rule will report unavailable"}
}

// artifactDisagreements reads back the graph the manifest just wrote and
// compares what the file says about itself with what the command said it
// rendered.
//
// This is the third check on one operation, and each catches something the
// others cannot. Manifest verifies a file is there afterwards — but a manifest
// that failed to overwrite leaves the *previous* file there, which passes that
// check with room to spare. Its generation comparison verifies what devmap
// intended to render. Only reading the bytes back verifies what landed.
func artifactDisagreements(graphPath string, rendered int) []string {
	m, err := repomap.LoadIfPresent(graphPath)
	if err != nil {
		return []string{fmt.Sprintf(
			"the graph artifact was written but could not be read back, so the scope rung will "+
				"start the next session without it: %v", err)}
	}
	if m == nil {
		return []string{fmt.Sprintf(
			"no graph artifact is at %s after writing it, so the scope rung has nothing to read", graphPath)}
	}
	out := m.Degraded()
	if rendered > 0 {
		if p := m.Provenance(); p.Stamped && p.GenerationID != rendered {
			out = append(out, fmt.Sprintf(
				"devmap reported rendering generation %d but the file at %s carries generation %d, "+
					"so the write did not land and the scope rung would keep reading the older graph",
				rendered, graphPath, p.GenerationID))
		}
	}
	return out
}

// describeIndex renders what a build indexed, in one line.
//
// The refusal count rides on this line rather than only in Degraded, because
// this string is the headline the TUI shows and what an operator reads as the
// outcome. "indexed 173 files" is a true sentence about a build that skipped
// the 174th, and a summary that cannot be read as complete coverage unless it
// is complete is the whole point.
//
// Both payload shapes are rendered. devmap answers a build whose tree is
// byte-for-byte the indexed one with `{"unchanged":true,"files":N}` and no
// counts at all, and this function used to format those absent fields straight
// into the line: an operator running `manvi map build` twice was told `indexed
// <nil> files, <nil> symbols, <nil> edges` the second time. A field the
// producer did not report is not a number, and printing it as one taught the
// reader to distrust the line that carries the refusal count.
func describeIndex(report *devmap.BuildReport) string {
	var line string
	switch {
	case report.Unchanged():
		line = "index already current, nothing to rebuild"
		if files, ok := report.Stat("files"); ok {
			line = fmt.Sprintf("index already current at %d files, nothing to rebuild", files)
		}
	default:
		line = fmt.Sprintf("indexed %s files, %s symbols, %s edges",
			stat(report, "files_indexed"), stat(report, "symbols"), stat(report, "edges"))
	}
	if report.Refused > 0 {
		line += fmt.Sprintf(", refused %d", report.Refused)
	}
	return line
}

// stat renders one count, or says the producer did not report it.
func stat(report *devmap.BuildReport, name string) string {
	if value, ok := report.Stat(name); ok {
		return fmt.Sprintf("%d", value)
	}
	return "an unreported number of"
}

// said names lines a caller has already printed, so one command does not report
// the same fact twice. Nil when nothing has been printed yet.
func mapStatus(out io.Writer, client *devmap.Client, ctx context.Context, said map[string]bool) error {
	fmt.Fprintln(out)
	status, err := client.Available(ctx)
	if err != nil {
		fmt.Fprintf(out, "  index         UNAVAILABLE: %v\n", err)
		fmt.Fprintln(out, "                the neighbour rule cannot run; unplanned writes will record repo_map.unavailable")
		return nil
	}
	fmt.Fprintf(out, "  index         generation %d — %d symbols, %d edges\n",
		status.GenerationID, status.NodeCount, status.EdgeCount)
	if !status.IsFresh {
		fmt.Fprintln(out, "                STALE: built before the current working tree; answers describe older code")
	}

	m, err := repomap.LoadIfPresent(graphArtifactPath())
	if err != nil {
		return err
	}
	if m == nil {
		fmt.Fprintf(out, "  areas         no graph artifact at %s — run `manvi map build`\n", graphArtifactPath())
		return nil
	}
	s := m.Stats()
	p := m.Provenance()
	// Printed on the areas line rather than below it, because the two lines
	// above and below were being read as one report about one graph. They were
	// not: the index stood at generation 4 with 4,249 symbols while this file
	// carried generation 2 and 2,713, and nothing here said so.
	generation := "unstamped"
	if p.Stamped {
		generation = fmt.Sprintf("generation %d", p.GenerationID)
	}
	fmt.Fprintf(out, "  areas         %d over %d files, %d adjacencies from %d edges (%s)\n",
		s.Areas, s.Files, s.Adjacencies, s.Edges, generation)
	for _, note := range m.DisagreementsWith(status.GenerationID, status.NodeCount) {
		fmt.Fprintf(out, "                DIVERGED: %s\n", note)
	}
	for _, note := range m.Degraded() {
		if said[note] {
			continue
		}
		fmt.Fprintf(out, "                %s\n", note)
	}
	if s.AmbiguousSkipped > 0 {
		// Reported because it is the difference between "these areas are not
		// coupled" and "the analyser could not tell".
		fmt.Fprintf(out, "                %d coupling edge(s) excluded as unresolved; adjacency rests only on resolved ones\n",
			s.AmbiguousSkipped)
	}
	if s.Permissive() {
		// Two shapes reach this, and one of them has no widest area to name:
		// an index that holds a single subsystem has nothing to be adjacent
		// to, so degree zero is the *most* permissive reading rather than the
		// least. Printing the ratio there would say `"" neighbours 0 of 1`,
		// which reads as the opposite of what it means.
		if s.MaxDegree == 0 {
			fmt.Fprintf(out, "                PERMISSIVE: the index holds one subsystem, so every indexed file is in the same subsystem as any planned file and the scope rung separates nothing\n")
		} else {
			fmt.Fprintf(out, "                PERMISSIVE: %q neighbours %d of %d areas, so the neighbour rule allows most writes\n",
				s.WidestArea, s.MaxDegree, s.Areas)
		}
	}
	return nil
}

// rigorProbe asks the verifier to identify itself, so doctor reports a fact
// rather than the absence of an error.
type rigorProbe struct{ binary string }

func (p rigorProbe) check() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return devcouncil.VerifierAvailable(ctx, p.binary)
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

// wrapNotice breaks a sentence into lines of at most width columns. An empty
// notice wraps to nothing, so a posture that relaxes nothing prints nothing
// rather than a blank continuation line.
func wrapNotice(notice string, width int) []string {
	var lines []string
	var line string
	for _, word := range strings.Fields(notice) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// --- flags ---

// flagsCommand is the whole `flags` surface: the report, and the one way to
// move a setting.
//
// It returns the key it changed, or "" for a read. The caller is what knows
// whether anything has already read the old value — a CLI process is about to
// exit and has nothing to reload, an attended session has a gate and a provider
// that do — so the reload is the caller's to run and this reports rather than
// performs it.
func flagsCommand(out io.Writer, reg *flags.Registry, args []string, from surface) (string, error) {
	if len(args) > 0 && args[0] == "set" {
		return setFlag(out, reg, args[1:], from)
	}
	return "", showFlags(out, reg, args)
}

// surface is who is running the command, because "what happens next" has
// different true answers for each.
//
// A shell invocation is a process that is about to exit: nothing in it has
// cached the old value, so nothing reloads, and telling the operator that a
// reload happened would be the same class of untruth this command exists to
// remove from the flag table. An attended session has a gate and a provider
// built from the old value, and does reload them.
type surface int

const (
	surfaceShell surface = iota
	surfaceSession
)

// setFlag moves one setting on human authority.
//
// Human authority is the literal truth of both callers: a command line is a
// person at a keyboard, and the TUI reaches this only from a slash command the
// operator typed. Nothing the model emits can arrive here — the agent's tool
// surface has no path to it — which is what keeps the "an agent that can switch
// off its own write gate has no write gate" rule intact now that the seam is
// reachable at all.
func setFlag(out io.Writer, reg *flags.Registry, args []string, from surface) (string, error) {
	if len(args) < 2 {
		return "", errors.New("usage: manvi flags set KEY VALUE   (manvi flags --all lists every key)")
	}
	if len(args) > 2 {
		// Refused rather than joined or ignored. A value with a space in it is
		// not something this catalogue has, and silently dropping the tail
		// would set a flag to a prefix of what was typed.
		return "", fmt.Errorf("flags set takes exactly one value; got %d — quote it if it contains spaces", len(args)-1)
	}
	key, raw := args[0], args[1]

	def, ok := reg.Def(key)
	if !ok {
		return "", fmt.Errorf("unknown setting %q — run 'manvi flags --all' for the catalogue%s", key, nearestFlag(reg, key))
	}
	before, err := reg.Lookup(key)
	if err != nil {
		return "", err
	}
	if err := reg.Set(flags.Human, key, raw); err != nil {
		return "", err
	}
	after, err := reg.Lookup(key)
	if err != nil {
		return "", err
	}

	fmt.Fprintf(out, "%s  %s (%s) → %s (%s)\n", key, before.Raw, before.Origin, after.Raw, after.Origin)
	// Said out loud, because "it was already that" and "it changed" look
	// identical in the line above and lead to different next actions — an
	// operator who believes they moved something is an operator who stops
	// looking for why the behaviour did not change.
	switch {
	case before == after:
		fmt.Fprintln(out, "\nnothing changed: it was already this value, from this layer")
	case before.Raw == after.Raw:
		fmt.Fprintln(out, "\nthe value did not change; only the layer it now comes from did")
	}
	if def.Safety {
		// Which direction it moved decides the sentence. Printing the weakened
		// warning for a flag just returned to its strictest value would teach
		// an operator to read the warning as noise, which is the reading that
		// makes it useless when it is the true one.
		if after.Raw == safestValue(def) {
			fmt.Fprintf(out, "\n! %s is a safety flag, and this is its safest value.\n", key)
		} else {
			fmt.Fprintf(out, "\n! %s is a safety flag and is no longer at its safest value (%s). "+
				"Every run that used it reports the value, so a result reached under this setting "+
				"cannot be read as a strict one.\n", key, safestValue(def))
		}
	}
	if from == surfaceSession {
		// Only where it is true. In a shell there is no session holding a copy
		// of the old value, so both of these would describe work nobody did.
		switch flags.ReachOf(def) {
		case flags.ReachNewSession:
			fmt.Fprintln(out, "\nsessions already open keep what they built at start; this applies to sessions opened from now on")
		case flags.ReachReload:
			fmt.Fprintln(out, "\nwhatever had already read the old value is reloaded before the next turn")
		}
	}
	// Last, and unconditional. The override layer lives in this process and
	// dies with it, so from a shell this command changes nothing that outlives
	// the exit — a trap worth spending three lines on, because the operator who
	// falls into it believes the setting is applied.
	fmt.Fprintf(out, "\nthis override lasts as long as this process. To make it durable, "+
		"export %s=%s, or write %s: %s into %s\n",
		flags.EnvKey(key), after.Raw, key, after.Raw, configPath())
	return key, nil
}

// safestValue is the value of a safety flag that represents the strictest
// posture. Def.Safest is empty for every flag whose Default is already that
// value, which is all of them but harness.posture.
func safestValue(def flags.Def) string {
	if def.Safest != "" {
		return def.Safest
	}
	return def.Default
}

// nearestFlag suggests a key when one is mistyped, or returns "".
//
// A mistyped key is refused rather than guessed at — this only decorates the
// refusal. The prefix match is deliberately dumb: a namespace typed correctly
// with the leaf wrong is the mistake that actually happens with dotted keys.
func nearestFlag(reg *flags.Registry, key string) string {
	stem := key
	if i := strings.LastIndexByte(key, '.'); i > 0 {
		stem = key[:i+1]
	}
	var near []string
	for _, k := range reg.Keys() {
		if strings.HasPrefix(k, stem) {
			near = append(near, k)
		}
	}
	if len(near) == 0 || len(near) > 8 {
		return ""
	}
	return "\ndid you mean: " + strings.Join(near, ", ")
}

// effectiveFlagValue resolves what a flag will actually be obeyed as, for the
// three settings the posture can overrule.
//
// The flag table used to print Value.Raw for every key, which made it byte
// identical under strict and under yolo for exactly the settings an operator
// reads it to learn: `! policy.file.mode enforce default` on a run where the
// gate enforces and on a run where it is off. EffectiveGateMode exists to stop
// that — its own doc says a report of "enforce" while the gate does not run is
// worse than no report — and doctor honours it while this table did not. Two
// commands reading the same registry must not disagree about what the gate will
// do, so both now ask the same resolver.
func effectiveFlagValue(reg *flags.Registry, key string) (flags.Value, error) {
	switch key {
	case flags.PolicyFileMode, flags.PolicyCommandMode:
		mode, origin, err := flags.EffectiveGateMode(reg, key)
		if err != nil {
			return flags.Value{}, err
		}
		return flags.Value{Key: key, Raw: mode, Origin: origin}, nil
	case flags.PolicyHardRules:
		on, origin, err := flags.EffectiveHardRules(reg)
		if err != nil {
			return flags.Value{}, err
		}
		return flags.Value{Key: key, Raw: strconv.FormatBool(on), Origin: origin}, nil
	default:
		return reg.Lookup(key)
	}
}

func showFlags(out io.Writer, reg *flags.Registry, args []string) error {
	all := slices.Contains(args, "--all")
	for _, key := range reg.Keys() {
		def, _ := reg.Def(key)
		value, err := effectiveFlagValue(reg, key)
		if err != nil {
			return err
		}
		if !all && value.Origin == flags.OriginDefault && !def.Safety {
			continue
		}
		marker := " "
		if def.Safety {
			marker = "!"
		}
		// The value column is what is in force. When the posture decided it,
		// the setting's own value is named after it rather than dropped: an
		// operator who typed `policy.file.mode: enforce` and is reading why the
		// gate is off needs to see both halves of that answer on one line.
		note := ""
		if set, err := reg.Lookup(key); err == nil && set.Raw != value.Raw {
			note = fmt.Sprintf("  (set to %s/%s)", set.Raw, set.Origin)
		}
		fmt.Fprintf(out, "%s %-34s %-14s %-9s %s%s\n", marker, key, value.Raw, value.Origin, def.Mutable, note)
	}
	if !all {
		fmt.Fprintln(out, "\n(showing safety flags and anything moved off its default; --all for everything)")
	}
	fmt.Fprintln(out, "\n! = safety flag: moving it is reported on every run that used it")
	fmt.Fprintln(out, "the last column is who may move it: 'human' means 'flags set KEY VALUE' from here, "+
		"'startup' means it is fixed for the life of this process")
	return nil
}

// --- lease ---

func lease(out io.Writer, args []string) error {
	if len(args) == 0 {
		return errors.New("lease needs a subcommand: list, acquire, release")
	}
	client := storeClient()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	switch args[0] {
	case "list":
		leases, err := client.ActiveLeases(ctx)
		if err != nil {
			return err
		}
		if len(leases) == 0 {
			fmt.Fprintln(out, "no active leases")
			return nil
		}
		for _, l := range leases {
			fmt.Fprintf(out, "%-14s %-16s expires %s\n", l.TaskID, l.Owner, l.ExpiresAt)
		}
		return nil

	case "acquire":
		if len(args) < 3 {
			return errors.New("usage: manvi lease acquire TASK OWNER [--ttl 15m]")
		}
		ttl := 15 * time.Minute
		if raw := flagValue(args, "--ttl"); raw != "" {
			parsed, err := time.ParseDuration(raw)
			if err != nil {
				return fmt.Errorf("--ttl %q: %w", raw, err)
			}
			ttl = parsed
		}
		l, err := client.Acquire(ctx, store.AcquireRequest{
			TaskID: args[1], Owner: args[2], TTL: ttl,
		})
		var conflict *store.Conflict
		if errors.As(err, &conflict) {
			// Contention is routable information, not a crash.
			fmt.Fprintf(out, "held by %s — take another task (%s)\n", conflict.Holder, conflict.Code())
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "acquired %s\ntoken %s\nexpires %s\n", l.TaskID, l.Token, l.ExpiresAt)
		return nil

	case "release":
		if len(args) < 3 {
			return errors.New("usage: manvi lease release TASK TOKEN")
		}
		released, err := client.Release(ctx, args[1], args[2])
		if err != nil {
			return err
		}
		if !released {
			fmt.Fprintln(out, "nothing to release")
			return nil
		}
		fmt.Fprintln(out, "released")
		return nil
	}
	return fmt.Errorf("unknown lease subcommand %q", args[0])
}

// --- check / allow ---

// buildGate composes the one gate: the flag registry, the repository root, and
// the navigation index the neighbour rule consults.
//
// The index used to be loaded only by nativeToolsWith, so a gate built here had
// Subsystems nil and one built there did not. Those two gates answer the
// neighbour rule differently by construction, and `manvi check PATH` was built
// here while the write it was predicting was judged there — a prediction from a
// gate that is not the one deciding is not a prediction.
func buildGate(reg *flags.Registry) (*gate.Gate, *repomap.Map, error) {
	root := projectRoot()
	// One repo-navigation service, three consumers: the gate's neighbour rule,
	// the navigation tools, and graph_context's report of a file's
	// neighbourhood. They read the same map so they cannot disagree about what
	// is in scope. It is returned as well as installed because Deps wants the
	// concrete map and the gate holds it behind an interface.
	m, err := repomap.LoadIfPresent(graphArtifactPath())
	if err != nil {
		return nil, nil, err
	}
	// Assigned through the nil check rather than directly. A nil *repomap.Map
	// stored in a policy.SubsystemMap is an interface that is not nil, and the
	// policy layer tests that interface to decide whether it has a map to
	// consult — so passing the pointer straight through would turn "no index"
	// into "an index that answers nothing", silently, for every neighbour
	// decision.
	var subsystems policy.SubsystemMap
	if m != nil {
		subsystems = m
	}
	g, err := gate.New(reg, root, subsystems)
	if err != nil {
		return nil, nil, err
	}
	if err := loadGrants(g); err != nil {
		return nil, nil, err
	}
	// Every grant this gate issues becomes durable, whoever issued it. A grant
	// that exists only for the life of one process is a grant nobody can review
	// later, which is the whole reason the ledger is written down.
	g.OnIssue = func(grants.Grant) {
		if err := saveGrants(g); err != nil {
			fmt.Fprintf(os.Stderr, "manvi: the grant ledger could not be written: %v\n", err)
		}
	}
	return g, m, nil
}

// check evaluates a write against the caller's gate.
//
// The gate is passed in rather than built here so an attended session evaluates
// against its own — the one holding its grants and its approval seam. Building a
// fresh one would answer a question about a different gate than the one that
// will actually decide the write.
func check(out io.Writer, g *gate.Gate, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: manvi check (PATH | --cmd COMMAND) [--task ID]")
	}
	cmdStr := flagValue(args, "--cmd")
	task := demoTask(flagValue(args, "--task"))

	var decision policy.Decision
	var err error
	if cmdStr != "" {
		decision, err = g.EvaluateCommand(cmdStr, task)
	} else {
		cwd, _ := os.Getwd()
		targetPath := resolveCLIPath(g.Root, cwd, args[0])
		decision, err = g.EvaluateWrite(targetPath, task, dc.OpWrite)
	}
	if err != nil {
		return err
	}
	printDecision(out, decision)
	return checkStatus(decision)
}

// checkStatus turns a decision into the exit status `manvi check` reports.
//
// Both branches used to print and return nil, so every refusal this command
// exists to produce exited 0. That is the harness's cardinal rule inverted: a
// check that blocked reported the same status as one that passed, and the
// obvious way to use this command — `manvi check "$f" && git commit` in a CI
// pre-flight — therefore committed through every block it was added to catch.
// /etc/passwd, .env and .git/config all denied, and all three exited 0.
//
// Two statuses rather than one, for the reason run's four are distinct: they
// ask the caller for different things. A soft block is cleared by `manvi allow`
// with a recorded reason, and printDecision has just printed the exact command.
// A hard block is cleared by nothing, by any authority, so a caller that retries
// it after issuing a grant will retry for ever — it has to change the approach
// instead. A warn is not a block and keeps status 0: the write was allowed, and
// the qualification is printed above.
func checkStatus(d policy.Decision) error {
	if !d.Blocked() {
		return nil
	}
	if d.Severity == policy.Hard {
		return errCheckHardBlocked
	}
	return errCheckBlocked
}

// errCheckBlocked is the sentinel that maps to exit status 6: the write was
// refused by a rule a grant can clear.
var errCheckBlocked = errors.New("blocked by policy")

// errCheckHardBlocked is the sentinel that maps to exit status 7: the write was
// refused by a hard rule, which no grant clears.
//
// Kept apart from 6 because the answer is different. Six says "decide whether to
// grant this"; seven says "there is nothing to decide, change what you are
// doing" — and a script that cannot tell them apart will sit in a grant-and-retry
// loop against a rule that is never going to move.
var errCheckHardBlocked = errors.New("blocked by a hard rule")

func allow(out io.Writer, g *gate.Gate, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: manvi allow (PATH | --cmd COMMAND) --reason TEXT [--task ID]")
	}
	reason := flagValue(args, "--reason")
	if strings.TrimSpace(reason) == "" {
		// The reason is what makes an override reviewable. Without it there is
		// nothing for a later reader to evaluate.
		return errors.New("--reason is required: a grant nobody can review later is not worth recording")
	}

	task := demoTask(flagValue(args, "--task"))
	cmdStr := flagValue(args, "--cmd")

	var decision policy.Decision
	var err error
	if cmdStr != "" {
		decision, err = g.EvaluateCommand(cmdStr, task)
	} else {
		cwd, _ := os.Getwd()
		targetPath := resolveCLIPath(g.Root, cwd, args[0])
		decision, err = g.EvaluateWrite(targetPath, task, dc.OpWrite)
	}
	if err != nil {
		return err
	}
	if !decision.Blocked() {
		fmt.Fprintln(out, "not blocked — nothing to grant")
		printDecision(out, decision)
		return nil
	}

	grant, err := g.RequestOverride(decision,
		grants.Grantor{Authority: grants.Human, ID: env("USER", "operator")}, reason, "")
	if err != nil {
		// A refusal here is the seam working: hard rules are never grantable.
		return fmt.Errorf("refused: %w", err)
	}
	fmt.Fprintf(out, "granted %s\n  rule    %s\n  target  %s\n  by      %s\n  reason  %s\n  expires %s\n",
		grant.ID, decision.Rule, decision.Target, grant.Grantor, grant.Reason,
		grant.ExpiresAt.Format(time.RFC3339))
	fmt.Fprintln(out, "\nthis grant is recorded; the write will now report as allowed-by-grant, never as a clean pass")
	return nil
}

func printDecision(out io.Writer, d policy.Decision) {
	fmt.Fprintf(out, "%-8s %s\n", strings.ToUpper(string(d.Action)), d.Target)
	if d.Rule != policy.RuleNone {
		fmt.Fprintf(out, "  rule     %s (%s)\n", d.Rule, d.Severity)
	}
	fmt.Fprintf(out, "  reason   %s\n", d.Reason)
	if d.GrantID != "" {
		fmt.Fprintf(out, "  granted  %s by %s\n", d.GrantID, d.GrantedBy)
	}
	if d.Demoted != "" {
		fmt.Fprintf(out, "  demoted  %s\n", d.Demoted)
	}
	if d.Widened != "" {
		fmt.Fprintf(out, "  widened  %s (scope this task appended at runtime, not its plan)\n", d.Widened)
	}
	for _, degraded := range d.Degraded {
		fmt.Fprintf(out, "  degraded %s\n", degraded)
	}
	if d.Blocked() && d.Overridable() {
		// The subject decides the flag. Printed without it, a command block
		// produced `manvi allow grep -rn package src --reason "..."`, and an
		// operator who ran what they were shown got a grant for a *file* named
		// "grep" — human authority, eight-hour ceiling, and the command still
		// blocked. The advice has to name the command as a command.
		fmt.Fprintf(out, "\n  overridable: manvi allow %s --reason \"...\"\n", allowSubjectArg(d))
	}
	if d.Blocked() && d.Severity == policy.Hard {
		fmt.Fprintln(out, "\n  hard rule: no grant clears this, by any authority")
	}
}

// allowSubjectArg renders the subject of a decision as the argument `manvi
// allow` needs to receive it under, quoted so it survives a copy and paste.
func allowSubjectArg(d policy.Decision) string {
	if policy.IsCommandRule(d.Rule) {
		return "--cmd " + shellQuote(d.Target)
	}
	return shellQuote(d.Target)
}

// shellQuote wraps a value so a shell hands it back as one argument.
//
// Unquoted, every target containing a space was advice that silently changed
// meaning when run: a command line split into flags, and a path with a space in
// it named a different file than the one that was refused.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r == '_' || r == '-' || r == '.' || r == '/' || r == '@' || r == '+' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// demoTask builds the task scope a check runs against. With no DevCouncil task
// wired in yet, it declares no planned files, so every write is out of scope —
// which is the honest default: an unplanned write is exactly what the gate is
// there to catch.
// withGate builds the CLI's gate and hands it to a command that needs one.
func withGate(out io.Writer, reg *flags.Registry, args []string,
	fn func(io.Writer, *gate.Gate, []string) error) error {
	g, _, err := buildGate(reg)
	if err != nil {
		return err
	}
	return fn(out, g, args)
}

func demoTask(id string) *dc.Task {
	if id == "" {
		id = "TASK-LOCAL"
	}
	return &dc.Task{ID: id}
}

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

	artStore, _ := artifacts.NewStore(artifactsDir())
	mcpMgr, err := buildMCP(reg, root)
	if err != nil {
		return nil, nil, err
	}
	// The manager spawns server subprocesses lazily and keeps them for the
	// life of the process. Registering the teardown here — the one place every
	// tool surface passes through — is what makes servers die with their
	// harness instead of outliving it; CloseAll existed and was tested, but
	// nothing ever called it.
	onProcessExit(func() { mcpMgr.CloseAll() })
	subRegistry := agents.NewRegistry()
	subMgr := agents.NewInstanceManager()

	// Built empty and attached later, once a provider and the tool registry
	// below exist. Until something attaches it the tool refuses, which is the
	// behaviour that replaced it reporting completed work it never did.
	subRunner := &subAgentRunner{}

	native, err := devcouncil.New(devcouncil.Deps{
		Store:            storeClient(),
		Gate:             g,
		Root:             root,
		LeaseTTL:         15 * time.Minute,
		VerifierBinary:   toolBinary("MANVI_VERIFY_BINARY", "dcverify"),
		CoverageFile:     os.Getenv("MANVI_COVERAGE"),
		Map:              mapClient(root),
		Subsystems:       subsystems,
		Approver:         approver,
		QuestionAsker:    questionAsker(approver),
		Artifacts:        artStore,
		MCP:              mcpMgr,
		SubagentRegistry: subRegistry,
		SubagentMgr:      subMgr,
		SubAgent:         subRunner,
	})
	if err != nil {
		return nil, nil, err
	}
	pipeline := tools.NewRegistry(bus.New())
	// Armed before any tool can run. Every result the model sees, and every
	// result the session log writes to disk, goes through this.
	toolScrubber := credentials.NewScrubber()
	toolScrubber.WatchAll(credentials.NewResolver())
	pipeline.SetScrubber(toolScrubber.Clean)
	if err := native.Register(pipeline); err != nil {
		return nil, nil, err
	}
	subRunners[pipeline] = subRunner
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
var subRunners = map[*tools.Registry]*subAgentRunner{}

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

// --- grant persistence ---

func loadGrants(g *gate.Gate) error {
	data, err := os.ReadFile(grantLedgerPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved []grants.Grant
	if err := json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("grant ledger %s is unreadable: %w", grantLedgerPath(), err)
	}
	// Refused entries are shown rather than fatal: one hand-edited record must
	// not take every command down, but it is also never loaded quietly.
	for _, why := range g.Ledger.Restore(saved) {
		fmt.Fprintf(os.Stderr, "manvi: grant refused on load — %s\n", why)
	}
	return nil
}

// ledgerWrite serialises writes to the ledger file. Sessions issue grants from
// their own goroutines, and two concurrent writers would interleave into a file
// that parses as neither.
var ledgerWrite sync.Mutex

func saveGrants(g *gate.Gate) error {
	ledgerWrite.Lock()
	defer ledgerWrite.Unlock()
	path := grantLedgerPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(g.Ledger.All(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
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

// reportDevMap states the index and the code graph as the two separate things
// they are.
//
// It was one line: the index's symbol and edge counts printed beside the
// artifact's path. That asserts of one file a number that came from another,
// and the two do diverge — while this repository's index stood at generation 4
// holding 4,249 symbols, the artifact carried generation 2 and 2,713, and
// doctor reported the artifact as holding 4,249.
//
// The line was also printed only when the index answered and held symbols, so a
// repository whose index would not open said nothing about navigation at all.
// The row an operator opens doctor to read is the row that disappeared exactly
// when there was something to read. Both halves now always report, including
// when what they have to report is that they could not look.
func reportDevMap(out io.Writer, ctx context.Context, mc *devmap.Client) {
	status, err := mc.Status(ctx)
	switch {
	case err != nil:
		fmt.Fprintf(out, "  dev map         index UNAVAILABLE: %v\n", err)
		fmt.Fprintln(out, "                  navigation tools will report unavailable; the scope rung falls back to same-directory")
	case status.NodeCount <= 0:
		fmt.Fprintln(out, "  dev map         index holds no symbols — run `manvi map build`")
	default:
		fmt.Fprintf(out, "  dev map         index generation %d (%d symbols, %d edges)%s\n",
			status.GenerationID, status.NodeCount, status.EdgeCount, staleSuffix(status))
	}

	graph := graphArtifactPath()
	m, loadErr := repomap.LoadIfPresent(graph)
	switch {
	case loadErr != nil:
		fmt.Fprintf(out, "                  %s UNREADABLE: %v\n", graph, loadErr)
	case m == nil:
		fmt.Fprintf(out, "                  no %s — the scope rung will record repo_map.unavailable\n", graph)
	default:
		p := m.Provenance()
		generation := "unstamped"
		if p.Stamped {
			generation = fmt.Sprintf("generation %d", p.GenerationID)
		}
		fmt.Fprintf(out, "                  %s (%d symbols over %d files, %s)\n",
			graph, p.Nodes, m.Stats().Files, generation)
		if status != nil {
			for _, note := range m.DisagreementsWith(status.GenerationID, status.NodeCount) {
				fmt.Fprintf(out, "                  DIVERGED: %s\n", note)
			}
		}
	}
}

// staleSuffix marks an index older than the tree it describes.
func staleSuffix(status *devmap.Status) string {
	if status.IsFresh {
		return ""
	}
	return " — STALE, built before the current working tree"
}
