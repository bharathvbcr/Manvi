package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/gate"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/grants"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/repomap"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/safefile"
)

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
	m, err := loadGraph(graphArtifactPath())
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
	// The gate's Matcher stays nil: fnmatch decides every pattern question
	// (DevCouncil's docs/gusset-candidates.md).
	if err := loadGrants(g); err != nil {
		return nil, nil, err
	}
	// Every ledger mutation becomes durable, whoever made it: an issue, a
	// single-use grant spent by a decision, and a revoke. A grant that exists
	// only for the life of one process is a grant nobody can review later, and
	// saving on issue alone let a restart bring back a spent grant and undo a
	// revoke.
	g.OnChange = func(grants.Change) {
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
		_, _ = fmt.Fprintln(out, "not blocked — nothing to grant")
		printDecision(out, decision)
		return nil
	}

	grant, err := g.RequestOverride(decision,
		grants.Grantor{Authority: grants.Human, ID: env("USER", "operator")}, reason, "")
	if err != nil {
		// A refusal here is the seam working: hard rules are never grantable.
		return fmt.Errorf("refused: %w", err)
	}
	_, _ = fmt.Fprintf(out, "granted %s\n  rule    %s\n  target  %s\n  by      %s\n  reason  %s\n  expires %s\n",
		grant.ID, decision.Rule, decision.Target, grant.Grantor, grant.Reason,
		grant.ExpiresAt.Format(time.RFC3339))
	_, _ = fmt.Fprintln(out, "\nthis grant is recorded; the write will now report as allowed-by-grant, never as a clean pass")
	return nil
}

func printDecision(out io.Writer, d policy.Decision) {
	_, _ = fmt.Fprintf(out, "%-8s %s\n", strings.ToUpper(string(d.Action)), d.Target)
	if d.Rule != policy.RuleNone {
		_, _ = fmt.Fprintf(out, "  rule     %s (%s)\n", d.Rule, d.Severity)
	}
	_, _ = fmt.Fprintf(out, "  reason   %s\n", d.Reason)
	if d.GrantID != "" {
		_, _ = fmt.Fprintf(out, "  granted  %s by %s\n", d.GrantID, d.GrantedBy)
	}
	if d.Demoted != "" {
		_, _ = fmt.Fprintf(out, "  demoted  %s\n", d.Demoted)
	}
	if d.Widened != "" {
		_, _ = fmt.Fprintf(out, "  widened  %s (scope this task appended at runtime, not its plan)\n", d.Widened)
	}
	for _, degraded := range d.Degraded {
		_, _ = fmt.Fprintf(out, "  degraded %s\n", degraded)
	}
	if d.Blocked() && d.Overridable() {
		// The subject decides the flag. Printed without it, a command block
		// produced `manvi allow grep -rn package src --reason "..."`, and an
		// operator who ran what they were shown got a grant for a *file* named
		// "grep" — human authority, eight-hour ceiling, and the command still
		// blocked. The advice has to name the command as a command.
		_, _ = fmt.Fprintf(out, "\n  overridable: manvi allow %s --reason \"...\"\n", allowSubjectArg(d))
	}
	if d.Blocked() && d.Severity == policy.Hard {
		_, _ = fmt.Fprintln(out, "\n  hard rule: no grant clears this, by any authority")
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
	data, err := json.MarshalIndent(g.Ledger.All(), "", "  ")
	if err != nil {
		return err
	}
	// Renamed into place rather than truncated and rewritten: a crash mid-write
	// must leave the previous ledger, not half of one that loadGrants refuses.
	return safefile.WriteAtomic(grantLedgerPath(), append(data, '\n'), 0o644)
}
