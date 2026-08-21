// Package gate composes the three seams a write must pass: the flag registry
// that says how strict the gate is, the policy ladder that decides, and the
// grant ledger that may clear a soft denial.
//
// It is the consumer in the seam rule — it owns none of the three definitions
// and is the only place that knows all three exist.
package gate

import (
	"fmt"
	"sync"

	"manvi/dc"
	"manvi/flags"
	"manvi/grants"
	"manvi/policy"
)

// Gate decides whether a file operation may proceed.
type Gate struct {
	Flags  *flags.Registry
	Ledger *grants.Ledger
	Root   string
	// Subsystems may be nil; the policy layer reports that as a degradation.
	Subsystems policy.SubsystemMap
	// GlobalAllowedCommands supplement every task's own command allowlist.
	GlobalAllowedCommands []string
	// OnIssue is notified after a grant is issued, so a composition root can
	// make it durable.
	//
	// The hook lives here because this is the one place every grant passes
	// through — the CLI's allow command, an agent's own request_override, and
	// an operator answering an approval card all land on RequestOverride. Left
	// to the callers, persistence is something each has to remember, and the
	// two that forgot issued grants that could never be reviewed afterwards,
	// which is the whole reason the ledger is written down.
	OnIssue func(grants.Grant)

	mu      sync.Mutex
	decided []policy.Decision
}

// New builds a gate from a flag registry.
func New(reg *flags.Registry, root string, subsystems policy.SubsystemMap) (*Gate, error) {
	p, err := grantPolicyFrom(reg)
	if err != nil {
		return nil, err
	}
	return &Gate{
		Flags:      reg,
		Ledger:     grants.NewLedger(p),
		Root:       root,
		Subsystems: subsystems,
	}, nil
}

// ReloadPolicy recomputes the grant policy from the registry and installs it.
//
// Every other flag this gate consults is read at the point of use, so a change
// lands on the next decision without anyone doing anything. The six grants.*
// flags are the exception: they are copied into the ledger's policy when the
// gate is built, which was fine while nothing could move them after boot. Once
// a setting can be moved at runtime, that copy is a second answer to "are agent
// grants enabled" — and the registry, which is what the flag table reports, is
// not the one deciding.
//
// Callers run this after a successful move. The grants already in the ledger
// are kept; see Ledger.SetPolicy.
func (g *Gate) ReloadPolicy() error {
	p, err := grantPolicyFrom(g.Flags)
	if err != nil {
		return err
	}
	g.Ledger.SetPolicy(p)
	return nil
}

// EvaluateWrite runs the full decision path for one file operation.
//
// The order is deliberate. The policy ladder runs first and at full strength,
// so the record always shows what the rules actually said. Only then does a
// grant clear it, and only then does the gate mode demote it. A decision that
// was allowed because enforcement is off still carries the rule that would
// have fired — which is what makes an advisory run useful rather than blank.
func (g *Gate) EvaluateWrite(path string, task *dc.Task, op dc.Operation) (policy.Decision, error) {
	mode, modeOrigin, err := flags.EffectiveGateMode(g.Flags, flags.PolicyFileMode)
	if err != nil {
		return policy.Decision{}, err
	}
	hardRules, _, err := flags.EffectiveHardRules(g.Flags)
	if err != nil {
		return policy.Decision{}, err
	}
	neighbors, _, err := g.Flags.Bool(flags.PolicyNeighborScope)
	if err != nil {
		return policy.Decision{}, err
	}
	sameDir, _, err := g.Flags.Bool(flags.PolicyScopeSameDir)
	if err != nil {
		return policy.Decision{}, err
	}

	fg := policy.FileGate{
		Root:           g.Root,
		Subsystems:     g.Subsystems,
		AllowNeighbors: neighbors,
		AllowSameDir:   sameDir,
		HardRules:      hardRules,
	}
	decision := fg.EvaluateFileChange(path, task, op, false)
	return g.settle(decision, mode, modeOrigin, flags.PolicyFileMode), nil
}

// EvaluateCommand runs the shell-command gate through the same three seams:
// the policy ladder decides, a grant may clear a soft denial, and the mode flag
// may demote one. Identical composition to EvaluateWrite, so an operator does
// not have to learn two models.
func (g *Gate) EvaluateCommand(command string, task *dc.Task) (policy.Decision, error) {
	mode, modeOrigin, err := flags.EffectiveGateMode(g.Flags, flags.PolicyCommandMode)
	if err != nil {
		return policy.Decision{}, err
	}
	hardRules, _, err := flags.EffectiveHardRules(g.Flags)
	if err != nil {
		return policy.Decision{}, err
	}

	decision := policy.CommandGate{
		GlobalAllowedCommands: g.GlobalAllowedCommands,
		HardRules:             hardRules,
	}.EvaluateCommand(command, task)

	return g.settle(decision, mode, modeOrigin, flags.PolicyCommandMode), nil
}

// settle applies the override ledger and then the gate mode, and records the
// decision. Shared by both gates so the two cannot drift apart.
func (g *Gate) settle(decision policy.Decision, mode string, modeOrigin flags.Origin, modeFlag string) policy.Decision {
	if decision.Blocked() {
		if cleared, used := g.Ledger.Apply(decision); used {
			decision = cleared
		}
	}

	// Mode demotion never touches a hard denial. A gate mode is an operator
	// saying they do not want scope enforcement, and that is a different
	// statement from wanting the credential and git-safety rules gone. Turning
	// those off is its own decision — policy.hard_rules.enabled, or the yolo
	// posture that resolves it — and it takes effect by stopping those rungs
	// from firing at all, not by demoting what they decided. A hard denial that
	// reached this point still stands.
	if decision.Blocked() && decision.Severity != policy.Hard {
		switch mode {
		case flags.ModeAdvisory, flags.ModeOff:
			decision.Action = policy.Allow
			decision.Demoted = fmt.Sprintf("%s=%s (%s)", modeFlag, mode, modeOrigin)
		}
	}

	g.mu.Lock()
	g.decided = append(g.decided, decision)
	g.mu.Unlock()
	return decision
}

// RequestOverride issues a grant that would clear a decision, on the given
// authority. It is the single entry point for both surfaces: a human answering
// a prompt and an agent calling a tool reach the same code and the same limits.
func (g *Gate) RequestOverride(d policy.Decision, by grants.Grantor, reason string, agentTask string) (grants.Grant, error) {
	req, err := grants.SuggestRequest(d, by)
	if err != nil {
		return grants.Grant{}, err
	}
	req.Reason = reason
	if by.Authority == grants.Agent {
		req.AgentTask = agentTask
	}
	grant, err := g.Ledger.Issue(req)
	if err != nil {
		return grant, err
	}
	if g.OnIssue != nil {
		g.OnIssue(grant)
	}
	return grant, nil
}

// AgentCanGrant reports whether an agent may clear this rule for itself, under
// the policy this gate was composed with. The recovery advice handed to an
// agent is built from this, so advice and ledger cannot disagree.
func (g *Gate) AgentCanGrant(rule policy.RuleID) bool {
	return g.Ledger.AgentMayGrant(rule)
}

// Decisions returns every decision this gate made, in order.
func (g *Gate) Decisions() []policy.Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]policy.Decision(nil), g.decided...)
}

// Report is the run summary that keeps a green result honest: how many writes
// passed cleanly, how many were cleared by grant, how many were let through by
// a demoted gate, and which safety flags were moved off their defaults.
type Report struct {
	// Posture is the development posture the run executed under. It is
	// reported unconditionally because a clean result means something
	// different in each: under dev, the soft rules did not block, so "nothing
	// was denied" is not evidence that nothing would have been.
	Posture string `json:"posture"`
	// HardRules is whether the credential, restricted-path, outside-root, and
	// git-safety rungs actually ran. It is reported even when no decision was
	// made, because "nothing was denied" means something different when the
	// rules that deny were not asked.
	HardRules bool `json:"hard_rules"`
	Total     int  `json:"total"`
	Clean     int  `json:"clean"`
	Granted   int  `json:"granted"`
	Demoted   int  `json:"demoted"`
	Denied    int  `json:"denied"`
	// Widened counts writes authorised by scope an executor appended to its own
	// task. They are reported apart from Granted because the grant that first
	// argued for them expires and this does not: in a later run the appended
	// scope is still there with no grant beside it, and without this counter
	// that run would summarise as though the plan had authorised every write.
	Widened    int      `json:"widened"`
	Degraded   int      `json:"degraded"`
	GrantLines []string `json:"grants,omitempty"`
	// IssuedGrantLines are grants that were issued and never applied. On an
	// unplanned path the grant's real effect is the scope it caused to be
	// written, so this is where the reason for a widening is recorded.
	IssuedGrantLines []string `json:"issued_grants,omitempty"`
	WeakenedFlags    []string `json:"weakened_flags,omitempty"`
}

// Strict reports whether the run both enforced the rules and passed them.
//
// The posture is part of the predicate on purpose. A dev-posture run with no
// violations did not demote anything, so by the narrower reading it "passed on
// the rules alone" — but the soft rules were not enforcing, and a summary that
// cannot tell those two runs apart is the failure this whole layer exists to
// avoid. Strict is the honest, conservative answer to "was this checked?".
func (r Report) Strict() bool {
	return r.Posture == flags.PostureStrict && r.HardRules &&
		r.Granted == 0 && r.Demoted == 0 && r.Widened == 0 && r.Degraded == 0 &&
		len(r.WeakenedFlags) == 0
}

// Report summarises the run.
func (g *Gate) Report() Report {
	decisions := g.Decisions()
	rep := Report{Total: len(decisions)}
	if posture, _, err := g.Flags.String(flags.HarnessPosture); err == nil {
		rep.Posture = posture
	}
	if hard, _, err := flags.EffectiveHardRules(g.Flags); err == nil {
		rep.HardRules = hard
	}
	for _, d := range decisions {
		switch {
		case d.Blocked():
			rep.Denied++
		case d.GrantID != "":
			rep.Granted++
		case d.Demoted != "":
			rep.Demoted++
		case d.Widened != "":
			rep.Widened++
		case d.Clean():
			rep.Clean++
		}
		if len(d.Degraded) > 0 {
			rep.Degraded++
		}
	}
	rep.GrantLines = grants.Summary(decisions)
	rep.IssuedGrantLines = grants.IssuedSummary(g.Ledger.Issued(), decisions)
	for _, v := range g.Flags.Weakened() {
		rep.WeakenedFlags = append(rep.WeakenedFlags, fmt.Sprintf("%s=%s (%s)", v.Key, v.Raw, v.Origin))
	}
	return rep
}

func grantPolicyFrom(reg *flags.Registry) (grants.Policy, error) {
	p := grants.DefaultPolicy()
	var err error
	if p.Enabled, _, err = reg.Bool(flags.GrantsEnabled); err != nil {
		return p, err
	}
	if p.AgentEnabled, _, err = reg.Bool(flags.GrantsAgentEnabled); err != nil {
		return p, err
	}
	if p.RequireReason, _, err = reg.Bool(flags.GrantsRequireReason); err != nil {
		return p, err
	}
	if p.AgentMaxTTL, _, err = reg.Duration(flags.GrantsAgentMaxTTL); err != nil {
		return p, err
	}
	if p.HumanMaxTTL, _, err = reg.Duration(flags.GrantsHumanMaxTTL); err != nil {
		return p, err
	}

	allowCommands, _, err := reg.Bool(flags.GrantsAgentCommands)
	if err != nil {
		return p, err
	}
	if allowCommands {
		p.AgentGrantableExtra = map[policy.RuleID]bool{policy.RuleCommandNotAllowed: true}
	}
	return p, nil
}
