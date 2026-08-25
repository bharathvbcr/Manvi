package serve

import (
	"encoding/json"
	"errors"
	"fmt"

	"manvi/dc"
	"manvi/gate"
	"manvi/policy"
)

// Posture decides what a policy denial means when there is no DevCouncil task.
//
// The gates were written for a harness that always has one: EvaluateFileChange
// and EvaluateCommand both reach a `task == nil` rung and deny there, with
// RuleNoTask and RuleCommandNoLease. That is right for DevCouncil, where a
// missing lease is a real error, and wrong for an embedding host — an editor,
// an IDE, a desktop app — which has no task model to be missing. Run unchanged
// in such a host, the gates deny every write and every command, and the host's
// only options are to ignore them entirely or to not use them.
//
// So the taskless case is made explicit rather than left to the caller to work
// around. The split is not invented for this: the ladder already separates the
// rules that protect the repository and its credentials, which run *before*
// the task rung and carry Severity Hard, from the rules that describe a task's
// declared scope, which run after and carry Severity Soft. A host without
// tasks wants exactly the first set.
type Posture string

// hostScopeID names the synthesised task a host's declared scope travels as.
//
// It is a fixed, obviously-not-a-task-id string so that a decision carrying it
// is recognisable as a host scope wherever one surfaces — a log line, an audit,
// a grant ledger — rather than being mistaken for a real DevCouncil task that
// nobody can find.
const hostScopeID = "host-scope"

const (
	// PostureDevCouncil is the harness's own posture: a task is required, and
	// its absence is a denial.
	PostureDevCouncil Posture = "devcouncil"
	// PostureHost is for an embedding host with no task model. Hard rules are
	// enforced unchanged; a Soft denial — which is only ever a statement about
	// a task's scope — is demoted to an allow that records why.
	//
	// The demotion is recorded in Decision.Demoted, so it is not the same
	// value as a clean allow: Decision.Clean() stays false, and a host or an
	// audit that summarises a run cannot mistake "no task model here" for "the
	// rules passed".
	PostureHost Posture = "host"
)

// demote resolves a decision under the posture.
//
// Only Soft denials move. A Hard denial is by definition one no task scope
// could authorise — a write to .env, a path outside the root, a force push
// over a protected branch — and those are the rules the host is embedding the
// gate *for*. A Warn already allows and is passed through with its note.
func (p Posture) demote(d policy.Decision, reason string) policy.Decision {
	if p != PostureHost || d.Action != policy.Deny || d.Severity != policy.Soft {
		return d
	}
	demoted := d
	demoted.Action = policy.Allow
	demoted.Demoted = reason
	// Rule and Severity are deliberately preserved. The decision still records
	// which rung fired, so a host that later grows a task model can find every
	// place this posture was carrying it.
	return demoted
}

// FileCheckParams is one file-write evaluation.
type FileCheckParams struct {
	// Root is the project root every path is resolved against. Required: the
	// outside-root rung is meaningless without it, and defaulting it to the
	// process working directory would silently evaluate against whatever
	// directory the host happened to spawn the sidecar from.
	Root string `json:"root"`
	// Path is the file the host is about to write, absolute or root-relative.
	Path string `json:"path"`
	// Op is create, modify, delete, or write. Empty means write — the
	// unspecialised form a tool reports when it does not know which it is.
	Op string `json:"op,omitempty"`
	// Internal exempts the caller from the restricted-path rung, for writes
	// the harness itself makes into its own state directory. A host should
	// leave it false; it is not a general escape hatch.
	Internal bool `json:"internal,omitempty"`
}

// CommandCheckParams is one shell-command evaluation.
type CommandCheckParams struct {
	// Command is the command line as it would be run.
	Command string `json:"command"`
	// Root is the project root a redirection target is resolved against.
	//
	// Required only when the command actually redirects, and refused rather
	// than defaulted when it does. A command line writes files through its
	// redirections — `cmd > .env` is a write to .env whatever the allowlist
	// says about cmd — and judging those needs a root, because the secret,
	// restricted and outside-root rungs are all statements about a path's
	// position in a repository.
	//
	// The absence of a root is answered with E_BAD_REQUEST rather than with a
	// decision carrying a "not checked" marker. A marker is only as good as the
	// host's willingness to read it, and a host that ignores it would receive
	// `echo x > /etc/sudoers` as an ordinary allow. Refusing puts the fault
	// where it can be fixed — in the request — and cannot be overlooked.
	//
	// A command with no redirection never consults it, so a host that only
	// checks plain commands keeps sending exactly what it always sent.
	Root string `json:"root,omitempty"`
	// AllowedCommands is the host's own allowlist, in fnmatch form.
	AllowedCommands []string `json:"allowed_commands,omitempty"`
	// EnforceAllowlist keeps "not in any allowlist" a denial under
	// PostureHost, instead of demoting it.
	//
	// The allowlist reaches the ladder as a synthesised task, not as
	// CommandGate.GlobalAllowedCommands. That field looks like the right one
	// and is not: the ladder consults it only *after* the `task == nil` rung
	// has already denied, so with no task a global allowlist can never match.
	// That is not an oversight to route around — the parity fixture pins
	// `ALLOW_NOTASK npm install deny` against DevCouncil's own engine with
	// `npm install` as the global allowlist, so the incumbent behaves the same
	// way and manvi is faithful to it. What a host declares here is a scope,
	// which is what a task *is*, so it is passed as one.
	//
	// It defaults to false, and that default is a deliberate statement about
	// what this gate is for in a host. A host that runs shell commands at all
	// already has some model of what it will run; dropping manvi's allowlist
	// on top of it would refuse commands the host's own UI offers, which reads
	// as a broken feature rather than as a safety decision. What the host gains
	// with this off is the *hard* command rules — force push, protected-branch
	// reset, --no-verify — which nothing in a typical host checks and which
	// destroy the evidence other gates reason about.
	//
	// A host that wants the full ladder sets this true and supplies a real
	// allowlist. That is a tightening, so it is opt-in rather than default.
	EnforceAllowlist bool `json:"enforce_allowlist,omitempty"`
}

// errCommandRootMissing is what the redirect rung's write evaluator reports
// when the command redirects and the host named no root. It travels as an error
// rather than as a denial because it is a defect in the request, not a decision
// about the command: a host that gets a deny would reasonably tell its user the
// command is unsafe, when what actually happened is that nobody could tell.
var errCommandRootMissing = errors.New(
	"policy.check.command requires a root when the command redirects to a file; " +
		"without one the outside-root rung cannot run and `> /etc/sudoers` would read as an ordinary write")

// checkFile evaluates one write.
func (s *Server) checkFile(raw json.RawMessage) (any, *Error) {
	var p FileCheckParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, badRequest("policy.check.file params: %v", err)
	}
	if p.Root == "" {
		return nil, badRequest("policy.check.file requires a root; without one the outside-root rung cannot run")
	}
	if p.Path == "" {
		return nil, badRequest("policy.check.file requires a path")
	}

	op, err := parseOperation(p.Op)
	if err != nil {
		return nil, badRequest("%v", err)
	}

	return s.evaluateHostWrite(p.Root, p.Path, op, p.Internal), nil
}

// evaluateHostWrite judges one path for a host with no task model.
//
// One function rather than one per call site: policy.check.file and a
// redirection target reached through policy.check.command are the same write,
// and a host that was told a path is refused must not be told the command that
// writes it is fine.
func (s *Server) evaluateHostWrite(root, path string, op dc.Operation, internal bool) policy.Decision {
	fileGate := policy.FileGate{
		Root: root,
		// Subsystems is nil: the neighbour rung needs a repo map the host has
		// not built, and the gate already marks the decision Degraded when it
		// cannot run rather than pretending it did.
		Subsystems:     nil,
		AllowNeighbors: s.allowNeighbors,
		// The same-directory fallback needs planned files to measure against
		// and this surface has no task at all, so the ladder stops at
		// task.absent long before it could run. Named rather than defaulted, so
		// the reason it is off is on the page beside the rung it belongs to.
		AllowSameDir: false,
		HardRules:    s.hardRules,
	}
	d := fileGate.EvaluateFileChange(path, nil, op, internal)
	return s.posture.demote(d, "serve.posture=host: no task model in the embedding host")
}

// checkCommand evaluates one command.
func (s *Server) checkCommand(raw json.RawMessage) (any, *Error) {
	var p CommandCheckParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, badRequest("policy.check.command params: %v", err)
	}

	// Root is optional on the wire; empty keeps the fail-closed behaviour in
	// which no absolute path is treated as this tree's own dev CLI.
	cmdGate := policy.CommandGate{HardRules: s.hardRules, Root: p.Root}

	// The host's declared scope, carried as the task the ladder expects. With
	// no task at all the ladder stops at RuleCommandNoLease and never reaches
	// an allowlist rung, so a host could neither widen nor narrow what it
	// runs. An empty AllowedCommands is a real value here: it means the host
	// declared nothing, and the ladder ends at RuleCommandNotAllowed — Soft,
	// and therefore demoted below unless the host asked for enforcement.
	scope := &dc.Task{
		ID:              hostScopeID,
		Title:           "embedding host scope",
		AllowedCommands: p.AllowedCommands,
	}
	d := cmdGate.EvaluateCommand(p.Command, scope)

	// The files this command line redirects into are judged before the
	// demotion below, for the same reason the harness gate judges them before
	// its own: a refusal that is about to be relaxed must not carry an
	// unexamined write through with it. Under PostureHost every Soft denial is
	// demoted, so without this a host asking about `nope > .env` would be told
	// "allowed" — the allowlist refusal demoted, the write to .env never
	// looked at.
	// An empty command has nothing to run and nothing to grant; it is Hard, so
	// it never reaches this demotion. Stated here because it is the one command
	// outcome a host might expect to be demoted and must not be.
	if !p.EnforceAllowlist {
		d = s.posture.demote(d, "serve.posture=host: allowlist not enforced (enforce_allowlist=false)")
	}
	if d.Blocked() {
		return d, nil
	}

	// The redirect rung, and specifically the harness's own one rather than a
	// copy of it. This plane built policy.CommandGate directly and therefore
	// never ran the rung at all: `git diff > ~/.ssh/authorized_keys` came back
	// action=allow with an empty rule and an empty Demoted, which is a clean
	// allow — indistinguishable, to an audit, from a command the rules actually
	// passed. It runs after the demotion above for the same reason gate.Gate
	// runs it after settle: the demotion is one of the ways a command that the
	// ladder refused ends up permitted, and a rung placed before it never sees
	// those.
	refusal, err := gate.EvaluateRedirects(p.Command, hostScopeID, func(target string) (policy.Decision, error) {
		if p.Root == "" {
			return policy.Decision{}, errCommandRootMissing
		}
		return s.evaluateHostWrite(p.Root, target, dc.OpWrite, false), nil
	})
	if err != nil {
		return nil, badRequest("%v", err)
	}
	if refusal.Refused {
		return refusal.Decision, nil
	}
	return d, nil
}

// parseOperation maps the wire spelling to dc.Operation, refusing anything
// else rather than defaulting.
//
// A silently-defaulted operation is the failure this prevents: "delete"
// misread as "write" evaluates a delete against the create/modify rung and can
// allow it.
func parseOperation(raw string) (dc.Operation, error) {
	switch raw {
	case "", string(dc.OpWrite):
		return dc.OpWrite, nil
	case string(dc.OpCreate):
		return dc.OpCreate, nil
	case string(dc.OpModify):
		return dc.OpModify, nil
	case string(dc.OpDelete):
		return dc.OpDelete, nil
	default:
		return "", fmt.Errorf("unknown file operation %q (want create, modify, delete, or write)", raw)
	}
}
