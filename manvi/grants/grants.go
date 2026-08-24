// Package grants is the override seam: how a human or an agent clears a block
// the policy gate raised, without anyone editing the gate.
//
// The design has one governing idea. An override is not a way to make a block
// disappear — it is a way to make it *accountable*. A granted decision still
// reports which rule fired, who cleared it, why, and for how long, and it never
// reports as a clean pass. The evidence report can therefore say "this task
// verified, with two scope exceptions granted by the operator at 14:22", which
// is a different and more useful statement than "this task verified".
//
// Three limits keep the seam from becoming a bypass:
//
//   - Hard rules are never grantable, by anyone. Secret paths, paths outside
//     the repository, and the agent client configs stay closed. An override
//     mechanism that can reach those is not an override mechanism.
//
//   - An agent may only grant rules on the agent-grantable list, only inside
//     its own lease's task, and only for a short TTL. An agent that can clear
//     any block it meets is an agent with no policy.
//
//   - Every grant is durable and reasoned. A grant with no recorded reason
//     cannot be reviewed later, which is the entire point of recording it.
package grants

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"manvi/internal/fnmatch"
	"manvi/policy"
)

// Authority is who issued a grant.
type Authority string

const (
	Human Authority = "human"
	Agent Authority = "agent"
)

// Grantor identifies the issuer.
type Grantor struct {
	Authority Authority `json:"authority"`
	ID        string    `json:"id"`
}

func (g Grantor) String() string {
	if g.ID == "" {
		return string(g.Authority)
	}
	return string(g.Authority) + ":" + g.ID
}

// Scope bounds what a grant covers. An empty field means "any", except TaskID
// for an agent grant, which must name the agent's own task.
type Scope struct {
	TaskID string          `json:"task_id,omitempty"`
	Rules  []policy.RuleID `json:"rules,omitempty"`
	// Paths are fnmatch globs matched against the normalized repo-relative path.
	Paths []string `json:"paths,omitempty"`
	// Once limits the grant to a single use.
	Once bool `json:"once,omitempty"`
}

// Grant is one recorded exception.
type Grant struct {
	ID        string    `json:"id"`
	Grantor   Grantor   `json:"grantor"`
	Reason    string    `json:"reason"`
	Scope     Scope     `json:"scope"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Consumed  bool      `json:"consumed,omitempty"`
}

// Active reports whether the grant can still be applied at time now.
func (g Grant) Active(now time.Time) bool {
	return !g.Consumed && now.Before(g.ExpiresAt)
}

// Matches reports whether the grant covers a decision.
func (g Grant) Matches(d policy.Decision) bool {
	if g.Scope.TaskID != "" && g.Scope.TaskID != d.TaskID {
		return false
	}
	if len(g.Scope.Rules) > 0 && !containsRule(g.Scope.Rules, d.Rule) {
		return false
	}
	if len(g.Scope.Paths) > 0 && !fnmatch.MatchAny(g.Scope.Paths, d.Target) {
		return false
	}
	return true
}

// Policy is the authority configuration the ledger enforces. It is populated
// from the flag registry so operators tune it in one place.
type Policy struct {
	Enabled       bool
	AgentEnabled  bool
	AgentMaxTTL   time.Duration
	HumanMaxTTL   time.Duration
	RequireReason bool
	// AgentGrantableExtra widens what an agent may clear beyond the default
	// set, for operators who want it. It can only add soft rules: Issue still
	// refuses every hard rule regardless of what appears here.
	AgentGrantableExtra map[policy.RuleID]bool
}

// agentMayGrant reports whether an agent is permitted to clear a rule.
func (p Policy) agentMayGrant(rule policy.RuleID) bool {
	return policy.AgentGrantable(rule) || p.AgentGrantableExtra[rule]
}

// DefaultPolicy is the conservative configuration used when nothing overrides it.
func DefaultPolicy() Policy {
	return Policy{
		Enabled:       true,
		AgentEnabled:  true,
		AgentMaxTTL:   15 * time.Minute,
		HumanMaxTTL:   8 * time.Hour,
		RequireReason: true,
	}
}

// Errors callers branch on.
var (
	ErrDisabled     = errors.New("grants: the override seam is disabled")
	ErrAgentBlocked = errors.New("grants: agent-issued grants are disabled")
	ErrHardRule     = errors.New("grants: hard rules are never grantable")
	ErrNotGrantable = errors.New("grants: rule is not agent-grantable")
	ErrNoReason     = errors.New("grants: a reason is required")
	ErrScope        = errors.New("grants: scope is not permitted for this grantor")
	ErrTTL          = errors.New("grants: requested lifetime exceeds the ceiling for this grantor")
)

// Request asks for a grant to be issued.
type Request struct {
	Grantor Grantor
	Reason  string
	Scope   Scope
	TTL     time.Duration
	// AgentTask is the task the requesting agent holds a lease on. It bounds an
	// agent grant to its own work and is ignored for human grantors.
	AgentTask string
}

// Ledger records grants and answers whether a decision is covered.
//
// It is durable in the sense that matters here: every issued grant stays in the
// ledger after it expires or is consumed, so a later review can see what was
// cleared. Nothing is ever removed to tidy up.
type Ledger struct {
	mu     sync.RWMutex
	policy Policy
	grants []Grant
	seq    int
	// restored holds the IDs reloaded from a previous run's ledger file. They
	// are grants in every other sense — they still clear decisions, and they
	// still appear in All — but they are not things *this* run did, and a run
	// summary that could not tell the difference would replay every grant ever
	// issued in this repository as though it had just been argued.
	restored map[string]bool
	// Now is injected for tests. Nil means time.Now.
	Now func() time.Time
}

// NewLedger returns a ledger governed by p.
func NewLedger(p Policy) *Ledger {
	return &Ledger{policy: p}
}

// SetPolicy replaces the policy this ledger is governed by.
//
// The grants already issued are kept. They were argued for and recorded under
// the policy in force at the time, and discarding them because a setting moved
// would erase the record rather than tighten it — the ledger's whole contract
// is that nothing is ever removed to tidy up. What changes is what may be
// issued next, and what AgentMayGrant tells an agent it can ask for.
//
// This exists because the policy is derived from six grants.* flags that are
// read once, when the gate is built. Without a way to reinstall it, moving one
// of those flags at runtime produced a registry reporting the new value and a
// ledger still enforcing the old one — the two disagreeing, silently, with the
// reassuring one doing the talking.
func (l *Ledger) SetPolicy(p Policy) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.policy = p
}

func (l *Ledger) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// AgentMayGrant reports whether an agent may clear this rule for itself under
// this ledger's policy — the default set plus whatever the operator added.
//
// Exported because the answer is not derivable from policy.AgentGrantable
// alone: grants.agent.allow_commands widens the set at composition time, so a
// caller that consults the package-level default tells an agent it cannot do
// something this ledger would in fact permit. The recovery advice an agent
// reads is built from this, so the advice and the ledger cannot disagree.
func (l *Ledger) AgentMayGrant(rule policy.RuleID) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if !l.policy.Enabled || !l.policy.AgentEnabled {
		return false
	}
	if policy.SeverityOf(rule) == policy.Hard {
		return false
	}
	return l.policy.agentMayGrant(rule)
}

// Issue validates and records a grant.
func (l *Ledger) Issue(req Request) (Grant, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.policy.Enabled {
		return Grant{}, ErrDisabled
	}
	if l.policy.RequireReason && strings.TrimSpace(req.Reason) == "" {
		return Grant{}, ErrNoReason
	}

	// A grant naming no rules would cover hard rules too. Require the rule set
	// to be explicit, and reject any hard rule in it.
	if len(req.Scope.Rules) == 0 {
		return Grant{}, fmt.Errorf("%w: name the rules being granted", ErrScope)
	}
	for _, rule := range req.Scope.Rules {
		if policy.SeverityOf(rule) == policy.Hard {
			return Grant{}, fmt.Errorf("%w: %s", ErrHardRule, rule)
		}
	}

	ceiling := l.policy.HumanMaxTTL
	switch req.Grantor.Authority {
	case Agent:
		if !l.policy.AgentEnabled {
			return Grant{}, ErrAgentBlocked
		}
		for _, rule := range req.Scope.Rules {
			if !l.policy.agentMayGrant(rule) {
				return Grant{}, fmt.Errorf("%w: %s", ErrNotGrantable, rule)
			}
		}
		if req.AgentTask == "" {
			return Grant{}, fmt.Errorf("%w: an agent grant must name the task it holds", ErrScope)
		}
		if req.Scope.TaskID != req.AgentTask {
			return Grant{}, fmt.Errorf("%w: an agent may only grant within its own task %q", ErrScope, req.AgentTask)
		}
		ceiling = l.policy.AgentMaxTTL
	case Human:
		// Humans may grant repository-wide; that is what human authority means.
	default:
		return Grant{}, fmt.Errorf("%w: unknown authority %q", ErrScope, req.Grantor.Authority)
	}

	ttl := req.TTL
	if ttl < 0 {
		// Rounding a negative lifetime up to the ceiling is the wrong direction:
		// a caller that passed one almost certainly computed it — a subtraction
		// against a deadline that has already passed — and the arithmetic being
		// wrong is not a reason to issue the longest grant available.
		return Grant{}, fmt.Errorf("%w: negative lifetime %s", ErrTTL, req.TTL)
	}
	if ttl == 0 {
		ttl = ceiling
	}
	if ttl > ceiling {
		return Grant{}, fmt.Errorf("%w: %s > %s", ErrTTL, ttl, ceiling)
	}

	now := l.now()
	l.seq++
	g := Grant{
		ID:        fmt.Sprintf("GRANT-%04d", l.seq),
		Grantor:   req.Grantor,
		Reason:    strings.TrimSpace(req.Reason),
		Scope:     req.Scope,
		IssuedAt:  now,
		ExpiresAt: now.Add(ttl),
	}
	l.grants = append(l.grants, g)
	return g, nil
}

// Apply clears a decision when an active grant covers it, returning the
// resulting decision and whether a grant was used.
//
// A hard denial is never cleared, even if a grant somehow names it — Issue
// rejects those, and this is the second gate on the same rule. Two independent
// checks on the one boundary that must not leak.
func (l *Ledger) Apply(d policy.Decision) (policy.Decision, bool) {
	if !d.Overridable() {
		return d, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.policy.Enabled {
		return d, false
	}
	now := l.now()
	for i := range l.grants {
		g := &l.grants[i]
		if !g.Active(now) || !g.Matches(d) {
			continue
		}
		if policy.SeverityOf(d.Rule) == policy.Hard {
			continue
		}
		if g.Scope.Once {
			g.Consumed = true
		}
		cleared := d
		cleared.Action = policy.Allow
		cleared.GrantID = g.ID
		cleared.GrantedBy = g.Grantor.String()
		cleared.Reason = fmt.Sprintf("%s (cleared by %s: %s)", d.Reason, g.Grantor, g.Reason)
		return cleared, true
	}
	return d, false
}

// Issued returns the grants this run issued, newest last, excluding anything
// reloaded from a previous run.
func (l *Ledger) Issued() []Grant {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Grant, 0, len(l.grants))
	for _, g := range l.grants {
		if l.restored[g.ID] {
			continue
		}
		out = append(out, g)
	}
	return out
}

// All returns every grant ever issued, newest last.
func (l *Ledger) All() []Grant {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return append([]Grant(nil), l.grants...)
}

// Active returns the grants that can still be applied.
func (l *Ledger) Active() []Grant {
	l.mu.RLock()
	defer l.mu.RUnlock()
	now := l.now()
	var out []Grant
	for _, g := range l.grants {
		if g.Active(now) {
			out = append(out, g)
		}
	}
	return out
}

// Revoke consumes a grant early.
func (l *Ledger) Revoke(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.grants {
		if l.grants[i].ID == id {
			l.grants[i].Consumed = true
			return nil
		}
	}
	return fmt.Errorf("grants: no grant %q", id)
}

// SuggestRequest builds the grant request that would clear a decision. The
// harness hands this to a human prompt or an agent tool so neither has to
// assemble scope by hand — and so the narrowest sufficient scope is the
// default rather than the widest.
func SuggestRequest(d policy.Decision, by Grantor) (Request, error) {
	if policy.SeverityOf(d.Rule) == policy.Hard {
		return Request{}, fmt.Errorf("%w: %s", ErrHardRule, d.Rule)
	}
	if d.Action != policy.Deny {
		return Request{}, fmt.Errorf("grants: decision is not a denial")
	}
	req := Request{
		Grantor: by,
		Scope: Scope{
			TaskID: d.TaskID,
			Rules:  []policy.RuleID{d.Rule},
			// The target is a concrete path, and Scope.Paths are patterns. It
			// is quoted so it covers itself and nothing else: a real file named
			// "a[bc].go" would otherwise yield a grant that also clears
			// "ab.go" and "ac.go", so answering one prompt would silently
			// authorise writes nobody was asked about.
			Paths: []string{fnmatch.QuoteMeta(d.Target)},
			Once:  by.Authority == Agent,
		},
	}
	if by.Authority == Agent {
		req.AgentTask = d.TaskID
	}
	return req, nil
}

// Summary renders the grants applied during a run, for the evidence report.
func Summary(applied []policy.Decision) []string {
	var lines []string
	for _, d := range applied {
		if d.GrantID == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s cleared %s on %s (%s)", d.GrantID, d.Rule, d.Target, d.GrantedBy))
	}
	sort.Strings(lines)
	return lines
}

// IssuedSummary renders grants that were issued but never applied to a
// decision, with the reason each was argued on.
//
// It exists because a grant can now do its work without ever clearing a write.
// Asking for one on an unplanned path also writes that path into the task's
// scope, so the write that follows is authorised by the widened plan and
// reports as widened rather than as granted — and a report assembled only from
// applied grants would show the widening with no record of why anyone asked for
// it. The reason is the whole value of the ledger; it must not go missing
// because the mechanism that recorded it was not the mechanism that allowed the
// write.
func IssuedSummary(issued []Grant, applied []policy.Decision) []string {
	used := make(map[string]bool, len(applied))
	for _, d := range applied {
		if d.GrantID != "" {
			used[d.GrantID] = true
		}
	}
	var lines []string
	for _, g := range issued {
		if used[g.ID] {
			continue
		}
		target := "any path"
		if len(g.Scope.Paths) > 0 {
			target = strings.Join(g.Scope.Paths, ", ")
		}
		rules := make([]string, 0, len(g.Scope.Rules))
		for _, r := range g.Scope.Rules {
			rules = append(rules, string(r))
		}
		lines = append(lines, fmt.Sprintf("%s issued for %s on %s by %s: %s",
			g.ID, strings.Join(rules, ","), target, g.Grantor, g.Reason))
	}
	sort.Strings(lines)
	return lines
}

func containsRule(rules []policy.RuleID, r policy.RuleID) bool {
	for _, candidate := range rules {
		if candidate == r {
			return true
		}
	}
	return false
}

// Restore reloads previously issued grants, so an override survives between
// harness invocations.
//
// Restored grants keep their original IDs, timestamps, and consumed state:
// reissuing them would reset an expiry and resurrect a single-use grant that
// was already spent, which would turn a durable record into a renewable
// permission. The sequence counter is advanced past anything restored so a new
// grant cannot collide with an old one's ID.
//
// Every entry is also re-validated against the policy this ledger enforces
// now, because Restore used to append whatever the file held: a corrupted or
// hand-edited record could name no rules (matching everything), carry a hard
// rule, or hold an expiry no issue path could have minted, and the durable
// override seam ended up trusting its own persistence less than it trusts the
// model. Refused entries come back as reasons so the caller can show them;
// they are never loaded half-way.
func (l *Ledger) Restore(saved []Grant) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	var refused []string
	for _, g := range saved {
		why := l.restoreRefusal(g)
		if why == "" {
			why = l.expiryRefusal(g, now)
		}
		if why != "" {
			refused = append(refused, fmt.Sprintf("%s: %s", g.ID, why))
			continue
		}
		l.grants = append(l.grants, g)
		if l.restored == nil {
			l.restored = make(map[string]bool)
		}
		l.restored[g.ID] = true
		var n int
		if _, err := fmt.Sscanf(g.ID, "GRANT-%d", &n); err == nil && n > l.seq {
			l.seq = n
		}
	}
	return refused
}

// restoreRefusal applies the checks Issue would have applied, minus the TTL
// arithmetic (Restore preserves original expiries; the ceiling itself is
// checked against those separately).
func (l *Ledger) restoreRefusal(g Grant) string {
	if len(g.Scope.Rules) == 0 {
		return "names no rules; a rule-less grant covers every soft rule on every task"
	}
	for _, rule := range g.Scope.Rules {
		if policy.SeverityOf(rule) == policy.Hard {
			return fmt.Sprintf("carries hard rule %s, which no grant may clear", rule)
		}
	}
	if strings.TrimSpace(g.Reason) == "" && l.policy.RequireReason {
		return "has no reason under a policy that requires one"
	}
	switch g.Grantor.Authority {
	case Agent:
		if !l.policy.AgentEnabled {
			return "agent grants are disabled by current policy"
		}
		for _, rule := range g.Scope.Rules {
			if !l.policy.agentMayGrant(rule) {
				return fmt.Sprintf("rule %s is not agent-grantable", rule)
			}
		}
		// The task scoping Issue enforces, enforced again here.
		//
		// Issue refuses an agent grant that does not name the task the agent
		// holds, and refuses one whose scope names a different task — that
		// pairing is the whole of what "an agent may only grant within its own
		// task" means. Restore did neither, so a record with an empty
		// Scope.TaskID came back as a grant covering *every* task and every
		// path, and Issue would have refused the identical shape a moment
		// earlier. A ledger file is not a trusted input: it outlives the run
		// that wrote it, it is editable, and the whole reason these checks
		// exist is that the scope on a grant is what bounds it.
		if strings.TrimSpace(g.Scope.TaskID) == "" {
			return "is an agent grant that names no task; an agent may only grant within its own task"
		}
	case Human:
		// Humans may grant repository-wide; that is what human authority means.
	default:
		return fmt.Sprintf("has unknown authority %q", g.Grantor.Authority)
	}
	return ""
}

// expiryRefusal rejects an expiry no issue path could have produced: beyond
// the ceiling for its own authority means the record was written by something
// other than this policy, and loading it would let that something extend its
// own life indefinitely.
func (l *Ledger) expiryRefusal(g Grant, now time.Time) string {
	if g.ExpiresAt.IsZero() {
		return ""
	}
	ceiling := l.policy.HumanMaxTTL
	if g.Grantor.Authority == Agent {
		ceiling = l.policy.AgentMaxTTL
	}
	// A ceiling of zero is the tightest setting an operator can choose, and it
	// used to be the one that switched this check off: the guard read
	// `ceiling > 0`, so `grants.agent.max_ttl=0s` let a record claiming a
	// hundred-year expiry restore unexamined. Zero is not a sentinel for "no
	// limit" here — Issue under a zero ceiling can only mint a grant that
	// expires the instant it is issued, so any restored record with a future
	// expiry is one this policy could not have produced. A negative ceiling is
	// not a longer one either; it is clamped to zero rather than inverted into
	// permission.
	if ceiling < 0 {
		ceiling = 0
	}
	if g.ExpiresAt.Sub(now) > ceiling {
		return fmt.Sprintf("expires in %s, beyond the %s ceiling for its authority",
			g.ExpiresAt.Sub(now).Round(time.Second), ceiling)
	}
	return ""
}
