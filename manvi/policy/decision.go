// Package policy is the harness's port of DevCouncil's write gate.
//
// The Python engine returns allow/warn/deny plus a prose reason. That is not
// enough to build an override seam on, because a caller cannot tell which
// denials may be overridden and by whom. This port keeps the same decision
// ladder and the same outcomes, and adds two fields the override seam needs:
// a stable RuleID, and a Severity that says whether the rule is negotiable.
//
// The severity classification is not invented. It follows the ladder's own
// structure: the rules that fire before a task is even consulted protect the
// repository and the credentials in it, and no task scope can authorise them.
// The rules that fire after are statements about *this task's declared scope*,
// and DevCouncil's own deny message for the commonest one already points at the
// remedy — "Expand scope with `dev scope update`".
package policy

// Action is the outcome of a policy evaluation.
type Action string

const (
	Allow Action = "allow"
	Warn  Action = "warn"
	Deny  Action = "deny"
)

// Severity says whether a denial is negotiable.
type Severity string

const (
	// Hard denials protect the repository, its credentials, and the gate's own
	// configuration. No grant, from any authority, clears one.
	Hard Severity = "hard"
	// Soft denials are statements about the current task's declared scope. A
	// human — or, for some rules, the agent itself — may grant an exception.
	Soft Severity = "soft"
	// Warn outcomes already allow; severity records that a note was attached.
	WarnSeverity Severity = "warn"
	// None is the severity of an allow.
	None Severity = "none"
)

// RuleID names the rung of the ladder that decided. Stable across releases,
// because grants reference rules by ID.
type RuleID string

const (
	RuleNone            RuleID = ""
	RuleMalformedPath   RuleID = "path.malformed"
	RuleOutsideRoot     RuleID = "path.outside_root"
	RuleSecretPath      RuleID = "path.secret"
	RuleRestrictedPath  RuleID = "path.restricted"
	RuleNoTask          RuleID = "task.absent"
	RuleForbiddenChange RuleID = "task.forbidden_change"
	RuleUnplannedScope  RuleID = "scope.unplanned"
	RuleReadOnly        RuleID = "scope.read_only"
	RuleOperation       RuleID = "scope.operation"
	RuleProtectedWrite  RuleID = "path.protected_write"

	// Command rules.
	RuleCommandEmpty          RuleID = "command.empty"
	RuleCommandNoLease        RuleID = "command.no_lease"
	RuleCommandNotAllowed     RuleID = "command.not_allowed"
	RuleCommandBypassFlag     RuleID = "command.bypass_flag"
	RuleCommandForcePush      RuleID = "command.force_push"
	RuleCommandProtectedReset RuleID = "command.protected_reset"
	RuleCommandProtectedPush  RuleID = "command.protected_branch_push"
	// Substitution and heredoc refusals are structural, not behavioural: the
	// ladder judges a command line by reading it, and both constructs carry
	// code the reading cannot see. They are Hard for the same reason the
	// splitter treats every unquoted operator as a boundary — an allowlist
	// entry matched against a line whose live parts were never judged is an
	// allow that skipped the gate.
	RuleCommandSubstitution RuleID = "command.substitution"
	RuleCommandHeredoc      RuleID = "command.heredoc"
)

// severities is the authoritative classification. A rule absent from this map
// is treated as Hard: a new rule is un-overridable until someone decides
// otherwise, which is the fail-closed direction.
var severities = map[RuleID]Severity{
	RuleMalformedPath:   Hard,
	RuleOutsideRoot:     Hard,
	RuleSecretPath:      Hard,
	RuleRestrictedPath:  Hard,
	RuleNoTask:          Soft,
	RuleForbiddenChange: Soft,
	RuleUnplannedScope:  Soft,
	RuleReadOnly:        Soft,
	RuleOperation:       Soft,
	RuleProtectedWrite:  WarnSeverity,

	// An empty command has nothing to grant, so it is closed rather than
	// negotiable. The three git-safety rules protect the verification gates
	// themselves: a --no-verify commit or a force push can erase the evidence
	// the whole system reasons about, and no scope decision authorises that.
	RuleCommandEmpty:          Hard,
	RuleCommandBypassFlag:     Hard,
	RuleCommandForcePush:      Hard,
	RuleCommandProtectedReset: Hard,
	RuleCommandProtectedPush:  WarnSeverity,
	RuleCommandNoLease:        Soft,
	RuleCommandNotAllowed:     Soft,
	RuleCommandSubstitution:   Hard,
	RuleCommandHeredoc:        Hard,
}

// Subject says what a rule's Target names: a path on disk, or a shell command.
//
// It exists because the override seam has one entry point and two kinds of
// subject, and the seam had no way to tell them apart. A command block was
// reported with the command text in Target, the recovery advice handed that
// text to the caller under the argument name "path", and the override handler
// evaluated it as a file write. The result was the worst available outcome: an
// agent that asked to be allowed to run a command was told "granted" for
// scope.unplanned — a rule it had not hit — the command string was appended to
// the task's planned files for the rest of its life, and the command was still
// blocked on the retry. Nothing in the exchange said so.
//
// Classifying the subject on the rule is what makes that unrepresentable. The
// rule is the one thing both the decision and the recovery already carry, and
// it is decided here, beside the severity, so a new rule cannot be added with a
// severity and no subject — TestEveryClassifiedRuleHasASubject fails if it is.
type Subject string

const (
	// SubjectPath means Target is a repository-relative path.
	SubjectPath Subject = "path"
	// SubjectCommand means Target is a shell command line.
	SubjectCommand Subject = "command"
)

// subjects classifies every rule that severities classifies. The two maps are
// pinned to the same key set by test, so the pairing cannot drift.
var subjects = map[RuleID]Subject{
	RuleMalformedPath:   SubjectPath,
	RuleOutsideRoot:     SubjectPath,
	RuleSecretPath:      SubjectPath,
	RuleRestrictedPath:  SubjectPath,
	RuleNoTask:          SubjectPath,
	RuleForbiddenChange: SubjectPath,
	RuleUnplannedScope:  SubjectPath,
	RuleReadOnly:        SubjectPath,
	RuleOperation:       SubjectPath,
	RuleProtectedWrite:  SubjectPath,

	RuleCommandEmpty:          SubjectCommand,
	RuleCommandNoLease:        SubjectCommand,
	RuleCommandNotAllowed:     SubjectCommand,
	RuleCommandBypassFlag:     SubjectCommand,
	RuleCommandForcePush:      SubjectCommand,
	RuleCommandProtectedReset: SubjectCommand,
	RuleCommandProtectedPush:  SubjectCommand,
	RuleCommandSubstitution:   SubjectCommand,
	RuleCommandHeredoc:        SubjectCommand,
}

// SubjectOf returns what a rule's Target names.
//
// An unclassified rule is reported as a path, which is the same direction
// SeverityOf defaults in: a rule this code does not know is also Hard, so it is
// never overridable and no recovery is ever routed for it. The parity test is
// what keeps that reasoning true rather than merely stated.
func SubjectOf(rule RuleID) Subject {
	if s, ok := subjects[rule]; ok {
		return s
	}
	return SubjectPath
}

// RuleKnown reports whether this is a rule the engine declares.
//
// It exists so a caller that receives a rule name from outside — the override
// seam takes one from the model — can refuse an unrecognised one instead of
// guessing. Guessing is not neutral: an unknown name falls to the path subject,
// which would hand it to the file gate, and the file gate's own soft rules are
// agent-grantable. That is the whole shape of the defect this classification
// closes, reachable again through a typo.
func RuleKnown(rule RuleID) bool {
	_, ok := severities[rule]
	return ok
}

// IsCommandRule reports whether this rule decided about a shell command.
func IsCommandRule(rule RuleID) bool { return SubjectOf(rule) == SubjectCommand }

// agentGrantable lists the rules an agent may clear for itself by default,
// within its own lease scope and under the agent TTL ceiling.
//
// Only file-scope rules qualify, and they are the ones DevCouncil already tells
// the agent to resolve by widening scope: touching a file the plan did not
// enumerate. Everything else encodes a decision someone else made on purpose —
// a file the planner marked read-only, a path the plan forbade, work attempted
// with no lease at all — and an agent overruling those is an agent editing its
// own instructions.
//
// RuleCommandNotAllowed is deliberately absent, and that is a divergence from
// the incumbent worth stating: DevCouncil's task model carries
// `agent_appended_allowed_commands`, so an agent there can widen its own
// command allowlist. The asymmetry is intentional. An unplanned file write is
// still bounded by every rung of the ladder above it, and by the verifier that
// reads the diff afterwards. An arbitrary command has no second gate — it is
// the mechanism the other gates are enforced through. Operators who want the
// incumbent's behaviour enable it explicitly with grants.agent.allow_commands,
// which is a flag rather than a default so the choice is visible in the run
// report.
var agentGrantable = map[RuleID]bool{
	RuleUnplannedScope: true,
	RuleProtectedWrite: true,
}

// SeverityOf returns a rule's severity, defaulting to Hard for unknown rules.
func SeverityOf(rule RuleID) Severity {
	if s, ok := severities[rule]; ok {
		return s
	}
	return Hard
}

// AgentGrantable reports whether an agent may clear this rule for itself.
func AgentGrantable(rule RuleID) bool { return agentGrantable[rule] }

// Decision is one policy outcome.
type Decision struct {
	Action   Action   `json:"action"`
	Rule     RuleID   `json:"rule"`
	Severity Severity `json:"severity"`
	Reason   string   `json:"reason"`
	Target   string   `json:"target"`
	TaskID   string   `json:"task_id,omitempty"`

	// Degraded names checks that could not run. A decision reached without the
	// repo map is not the same decision as one reached with it, and the caller
	// must be able to tell. Empty means every check ran.
	Degraded []string `json:"degraded,omitempty"`

	// Demoted records that a gate mode downgraded this outcome, naming the flag
	// responsible. An allow produced by policy.file.mode=advisory is not the
	// same as an allow produced by the rules passing.
	Demoted string `json:"demoted,omitempty"`

	// GrantID and GrantedBy are set when an override cleared a soft block. An
	// overridden decision reports Allow, but never looks like a clean allow.
	GrantID   string `json:"grant_id,omitempty"`
	GrantedBy string `json:"granted_by,omitempty"`

	// Widened names the path pattern that authorised this write when the
	// pattern came from scope an executor appended to its own task, rather than
	// from the plan the task was created with.
	//
	// It exists because the durable half of the override seam would otherwise
	// launder a self-grant into a plan. A grant expires and is visible in the
	// ledger for as long as the process lives; scope written into the task
	// outlives both, and every later write against it would report as an
	// ordinary planned write — in this run and in every run afterwards. The
	// widening is recorded on the decision so that never happens.
	Widened string `json:"widened,omitempty"`
}

// Blocked reports whether the decision stops the operation.
func (d Decision) Blocked() bool { return d.Action == Deny }

// Overridable reports whether a grant could clear this decision.
func (d Decision) Overridable() bool {
	return d.Action == Deny && d.Severity == Soft
}

// Clean reports whether the decision is an ordinary pass: allowed, not
// overridden, not demoted, with every check having run. This is the predicate
// evidence reporting uses, so a green run assembled from overrides and
// degraded checks cannot be summarised as if it were strict.
func (d Decision) Clean() bool {
	return d.Action == Allow && d.GrantID == "" && d.Demoted == "" &&
		d.Widened == "" && len(d.Degraded) == 0
}

func allow(reason, target, taskID string) Decision {
	return Decision{Action: Allow, Rule: RuleNone, Severity: None, Reason: reason, Target: target, TaskID: taskID}
}

func deny(rule RuleID, reason, target, taskID string) Decision {
	return Decision{Action: Deny, Rule: rule, Severity: SeverityOf(rule), Reason: reason, Target: target, TaskID: taskID}
}

func warn(rule RuleID, reason, target, taskID string) Decision {
	return Decision{Action: Warn, Rule: rule, Severity: SeverityOf(rule), Reason: reason, Target: target, TaskID: taskID}
}
