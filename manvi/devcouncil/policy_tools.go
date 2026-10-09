package devcouncil

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/fnmatch"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/grants"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
	"github.com/bharathvbcr/Manvi/manvi/tools"
	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// --- guarded mutation ---

// currentTask reads the scope the gate evaluates against. A checked-out task
// whose scope cannot be read is an error rather than an empty scope: an empty
// scope would deny every write and read as a policy decision.
func (r *Registry) currentTask(ctx context.Context) (*dc.Task, error) {
	state := r.sessionFor(ctx).State()
	if err := held(state); err != nil {
		return nil, err
	}
	task, err := r.deps.Store.Task(ctx, state.TaskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("task %s is held but has no record", state.TaskID)
	}
	return task.Domain(), nil
}

func (r *Registry) policyCheck(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Path string `json:"path"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	// The same resolution devcouncil_write_file performs, through the same
	// owner, and deliberately not a stricter one.
	//
	// This tool exists to preview that write, so any state in which the two
	// disagree is a state in which the cheap call lies about the expensive one.
	// It used to call currentTask, which refuses outright when no lease is
	// held, while the write called authorisingTask, which hands the gate a nil
	// task and lets the posture decide. Under the shipped dev posture and under
	// yolo the write therefore succeeded while the preview reported a hard stop
	// for the same path — and under core_tools_only the preview's refusal named
	// devcouncil_checkout_task, a tool that profile had removed. An agent that
	// checked before writing was told the work was impossible; one that skipped
	// the check wrote the file.
	task, refusal := r.authorisingTask(ctx, "checking a write")
	if refusal != nil {
		return *refusal
	}
	decision, err := r.deps.Gate.EvaluateWrite(args.Path, task, dc.OpWrite)
	if err != nil {
		return unavailable("policy decision", err)
	}
	return ok(r.decisionPayload(decision))
}

// decisionPayload renders a decision so an agent can branch without reading
// prose — including, when the block is overridable, the exact call that would
// clear it.
//
// It is a method rather than a free function because naming a recovery needs two
// things the decision does not carry: whether the rule's Target is a path or a
// command, and whether this gate's ledger would let an agent clear that rule.
// Built from the decision alone the advice was wrong in both directions at once
// — it handed a command string to an argument called "path", and it named an
// agent-issued override for rules no agent may issue one for.
func (r *Registry) decisionPayload(d policy.Decision) map[string]any {
	payload := map[string]any{
		"allowed":  !d.Blocked(),
		"action":   string(d.Action),
		"rule":     string(d.Rule),
		"severity": string(d.Severity),
		"reason":   d.Reason,
		"target":   d.Target,
		// What Target names. A consumer that has to infer this is the consumer
		// that handed a command line to a file-write evaluator.
		"subject": string(policy.SubjectOf(d.Rule)),
	}
	if len(d.Degraded) > 0 {
		payload["degraded"] = d.Degraded
	}
	if d.GrantID != "" {
		payload["granted_by"] = d.GrantedBy
		payload["grant_id"] = d.GrantID
		// A granted allow is not a clean pass, and the payload says so.
		payload["clean"] = false
	}
	if d.Demoted != "" {
		payload["demoted"] = d.Demoted
		payload["clean"] = false
	}
	if d.Widened != "" {
		// Named on every decision it authorises, not only on the one that
		// created it. An agent reading this should be able to tell a write the
		// plan authorised from one its own earlier argument did.
		payload["widened"] = d.Widened
		payload["clean"] = false
	}
	if d.Blocked() {
		if !d.Overridable() {
			payload["overridable"] = false
			payload["agent_grantable"] = false
			payload["note"] = "hard rule: no override clears this, by any authority"
			return payload
		}

		// Overridable says a grant of *some* authority could clear this.
		// Whether this agent is that authority is a separate question, and
		// conflating the two is what produced advice an agent could not act on.
		agentMay := r.deps.Gate.AgentCanGrant(d.Rule)
		payload["overridable"] = true
		payload["agent_grantable"] = agentMay

		// Which recovery to name depends on what is actually missing. An agent
		// that holds a task and strayed outside its planned scope should argue
		// for the exception; an agent holding no task at all should take one,
		// and telling it to request an override instead sends it to negotiate
		// about work it has not claimed. Every case here is a soft block, so
		// severity alone cannot distinguish them — the rule can.
		switch {
		case d.Rule == policy.RuleNoTask:
			payload["suggested_tool"] = "devcouncil_checkout_task"
			payload["suggested_arguments"] = map[string]string{
				"task_id": "the task this work belongs to; " +
					"devcouncil_next_task lists what is ready",
			}
			payload["note"] = "no task is checked out; checkout first, " +
				"or request an override if this write belongs to no task"

		case d.Rule == policy.RuleCommandNoLease:
			// The command-gate twin of task.absent, and it gets the same
			// answer. It used to reach the override branch, where the command
			// line was passed as a path to a tool that first demands the very
			// lease this rule says is missing.
			payload["suggested_tool"] = "devcouncil_checkout_task"
			payload["suggested_arguments"] = map[string]string{
				"task_id": "the task this work belongs to; " +
					"devcouncil_next_task lists what is ready",
			}
			payload["note"] = "shell commands need a lease; check out the task first. " +
				"Reading the repository does not: " + ungatedToolSentence

		case policy.IsCommandRule(d.Rule) && agentMay:
			// Reachable only when an operator set grants.agent.allow_commands.
			// The subject is named `command`, because that is what it is.
			payload["suggested_tool"] = "devcouncil_request_override"
			payload["suggested_arguments"] = map[string]string{
				"command": d.Target, "rule": string(d.Rule),
				"reason": "explain why this command is necessary",
			}

		case policy.IsCommandRule(d.Rule):
			// No agent-issued grant clears this, so no agent tool is named. The
			// previous code named one anyway: the agent called it, was told
			// "granted" for a file-scope rule it had not hit, had the command
			// line appended to its task's planned files, and found the command
			// still blocked. Naming nothing beats naming a dead end — and the
			// alternatives below are not a consolation prize, they are how
			// exploration is supposed to work here.
			payload["suggested_action"] = "no agent-issued grant clears a command block. " +
				"Use an ungated read tool, add the command to the task's allowed_commands, " +
				"or ask a human to grant it."
			payload["ungated_alternatives"] = ungatedReadTools
			payload["note"] = ungatedToolSentence +
				" An operator can enable agent-issued command grants with grants.agent.allow_commands."

		default:
			payload["suggested_tool"] = "devcouncil_request_override"
			payload["suggested_arguments"] = map[string]string{
				"path": d.Target, "rule": string(d.Rule),
				"reason": "explain why this write is necessary",
			}
		}
	}
	return payload
}

// escalate puts a blocked decision to an attached human and, if cleared,
// returns the decision as it stands after the grant.
//
// Three refusals are unconditional and none of them consult the approver: no
// approver attached, a rule no authority can clear, and a decision the ledger
// itself declines to grant. A hard rule is never even shown as a question —
// offering an allow that is going to be refused downstream teaches an operator
// that the control is advisory.
//
// subject is the operation that will actually be performed if this is cleared:
// the repository path for a write, the exact command line for an exec. Every
// caller passes it, because decision.Target is not reliably that thing. The
// command gate matches against a normalised form with trailing redirections
// stripped, and the denial carries the normalised string — so the approval card
// asked a human to clear `cat src/calc.go` while `cat src/calc.go > .env.local`
// was what stood ready to run, and the grant issued from it took its scope from
// the same stripped string, re-clearing every redirect variant of that command
// for the grant's whole life. What a human authorises is now exactly what
// executes, and the grant covers exactly what was shown.
func (r *Registry) escalate(ctx context.Context, decision policy.Decision, subject string) (policy.Decision, bool) {
	if r.deps.Approver == nil || !decision.Overridable() {
		return decision, false
	}
	if strings.TrimSpace(subject) == "" {
		// An escalation with nothing concrete to show is not a question a
		// human can answer, and consent to an unnamed operation is not consent.
		return decision, false
	}

	// The decision the human is shown and the decision the grant is issued from
	// are one and the same, and both name the real operation. The original is
	// left untouched so a refusal still reports what the ladder actually said.
	authorised := decision
	authorised.Target = subject

	state := r.sessionFor(ctx).State()
	answer, err := r.deps.Approver.Approve(ctx, ui.Request{
		Rule:      string(authorised.Rule),
		Severity:  string(authorised.Severity),
		Path:      authorised.Target,
		Subject:   string(policy.SubjectOf(authorised.Rule)),
		Reason:    authorised.Reason,
		TaskID:    state.TaskID,
		Grantable: authorised.Overridable(),
	})
	// An error, a refusal, and an allow with no reason are all refusals. A
	// question that could not be answered must never be treated as consent.
	if err != nil || !answer.Allow || strings.TrimSpace(answer.Reason) == "" {
		return decision, false
	}

	grant, err := r.deps.Gate.RequestOverride(authorised,
		grants.Grantor{Authority: grants.Human, ID: grantorID(answer.By)},
		answer.Reason, state.TaskID)
	if err != nil {
		return decision, false
	}

	// Re-evaluated rather than patched. The decision returned here is the one
	// the ledger actually produces for this write, so the qualification that
	// travels onto the result is the real grant rather than one assembled at
	// the call site.
	authorised.Action = policy.Allow
	authorised.GrantID = grant.ID
	authorised.GrantedBy = string(grant.Grantor.Authority)
	return authorised, true
}

func grantorID(by string) string {
	if by == "" {
		return "operator"
	}
	return by
}

// annotate carries a decision's qualifications onto a successful result.
//
// Every way an allow can be reached other than "the rules passed" travels with
// it: the grant that cleared it, the posture or mode that demoted it, the
// checks that could not run. Without this a write permitted by the dev posture
// is indistinguishable in the log from one the plan authorised, and the run
// report would be assembled from results that had already lost the distinction.
// refusal is the one place a blocked decision becomes a model-facing result.
//
// It was four identical literals, one per gated tool, and each of them had to
// remember to carry the decision's rule and severity onto the result. Nothing
// made them agree, and there was no single place to add the verdict itself —
// so the loop that reports the run was left inferring "the gate refused this"
// from Rule and IsError, two fields that allowed results also set. See
// tools.Result.Blocked.
func (r *Registry) refusal(d policy.Decision) tools.Result {
	payload, _ := json.Marshal(r.decisionPayload(d))
	return tools.Result{
		Text: string(payload), IsError: true, Blocked: true,
		Rule: string(d.Rule), Severity: string(d.Severity),
	}
}

func annotate(result tools.Result, d policy.Decision) tools.Result {
	if d.Clean() {
		return result
	}
	result.Blocked = d.Blocked()
	result.Rule = string(d.Rule)
	result.Severity = string(d.Severity)
	result.GrantID = d.GrantID
	result.GrantedBy = d.GrantedBy
	result.Demoted = d.Demoted
	result.Degraded = d.Degraded
	result.Widened = d.Widened
	if d.Demoted != "" {
		result.Text += fmt.Sprintf("\n[%s would have blocked this: %s — allowed by %s]",
			d.Rule, d.Reason, d.Demoted)
	}
	return result
}

// --- the override seam ---

func (r *Registry) requestOverride(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Path    string `json:"path"`
		Command string `json:"command"`
		Rule    string `json:"rule"`
		Reason  string `json:"reason"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(args.Reason) == "" {
		return tools.Errorf("a reason is required: an override nobody can review later is not worth recording")
	}
	state := r.sessionFor(ctx).State()
	if err := held(state); err != nil {
		return tools.Errorf("%v", err)
	}

	task, err := r.currentTask(ctx)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	// An unrecognised rule name is refused rather than interpreted. It cannot
	// be classified, so it would fall to the file gate by default — and the
	// file gate's soft rules are the agent-grantable ones, which means a typo
	// would be answered with a grant for a rule the caller never hit. The
	// blocked payload always carries the exact name; there is nothing to guess.
	if named := strings.TrimSpace(args.Rule); named != "" && !policy.RuleKnown(policy.RuleID(named)) {
		return tools.Errorf("unknown rule %q: name the rule from the block you hit, "+
			"exactly as its payload's \"rule\" field spelled it", named)
	}

	subject, target := overrideSubject(args.Rule, args.Command, args.Path)
	if target == "" {
		return tools.Errorf("nothing to override: give the %s the block named in its \"target\" field", subject)
	}

	// Re-evaluate rather than trusting the rule the model named. An agent that
	// could nominate the rule it is being granted could name a softer one than
	// the block it actually hit.
	var decision policy.Decision
	op := dc.OpWrite
	if subject == policy.SubjectCommand {
		cmdDecision, cmdErr := r.deps.Gate.EvaluateCommand(target, task)
		if cmdErr != nil {
			return unavailable("command policy decision", cmdErr)
		}
		decision = cmdDecision
	} else {
		if args.Rule == string(policy.RuleOperation) || strings.Contains(args.Reason, "delete") || strings.Contains(args.Rule, "delete") {
			op = dc.OpDelete
		}
		writeDecision, writeErr := r.deps.Gate.EvaluateWrite(target, task, op)
		if writeErr != nil {
			return unavailable("policy decision", writeErr)
		}
		if !writeDecision.Blocked() && op != dc.OpDelete {
			if delDec, delErr := r.deps.Gate.EvaluateWrite(target, task, dc.OpDelete); delErr == nil && delDec.Blocked() {
				writeDecision = delDec
			}
		}
		decision = writeDecision
	}
	if !decision.Blocked() {
		return ok(map[string]any{"granted": false, "reason": "not blocked; no override needed"})
	}

	grant, err := r.deps.Gate.RequestOverride(decision,
		grants.Grantor{Authority: grants.Agent, ID: state.Owner},
		args.Reason, state.TaskID)
	if err != nil {
		// A refusal is the seam working. Say which authority could clear it,
		// so the agent asks a human instead of retrying.
		return ok(map[string]any{
			"granted":          false,
			"rule":             string(decision.Rule),
			"severity":         string(decision.Severity),
			"error":            err.Error(),
			"suggested_action": humanPath(decision),
		})
	}
	payload := map[string]any{
		"granted":    true,
		"grant_id":   grant.ID,
		"rule":       string(decision.Rule),
		"target":     decision.Target,
		"expires_at": grant.ExpiresAt.Format(time.RFC3339),
		"note":       "recorded; the write will report as allowed-by-grant, never as a clean pass",
	}
	r.persistWidenedScope(ctx, decision, op, payload)
	return ok(payload)
}

// overrideSubject decides which gate an override request is about, and what the
// target text is.
//
// The rule leads, because the rule is the thing re-evaluated below and the thing
// the blocked payload handed the caller. Classification comes from the policy
// package, which owns the rule taxonomy: a prefix test spelled here would be a
// second, private answer to a question that already has one, and it would go
// stale the first time a command rule is named without the prefix it assumed.
//
// The populated argument only breaks a tie the rule cannot: a caller that named
// no rule at all. Naming neither argument is an error at the call site, not a
// silent evaluation of the empty path.
func overrideSubject(rule, command, path string) (policy.Subject, string) {
	id := policy.RuleID(strings.TrimSpace(rule))
	command = strings.TrimSpace(command)
	path = strings.TrimSpace(path)

	if id != policy.RuleNone && policy.IsCommandRule(id) {
		if command != "" {
			return policy.SubjectCommand, command
		}
		// Tolerated on purpose: the old advice put the command line in `path`,
		// and transcripts replayed from before that was fixed still do. Reading
		// it as a command is the correct evaluation of what the caller meant,
		// and it is what stops the file gate from ever seeing a command again.
		return policy.SubjectCommand, path
	}
	if id != policy.RuleNone {
		if path != "" {
			return policy.SubjectPath, path
		}
		return policy.SubjectPath, command
	}
	if command != "" {
		return policy.SubjectCommand, command
	}
	return policy.SubjectPath, path
}

// persistWidenedScope writes a just-granted path into the task's own scope, so
// the argument the agent made survives the grant that recorded it.
//
// Without this the seam is honest but exhausting. A grant lives for minutes by
// design — it is an exception, and an exception with no expiry is a permission —
// while the task it was argued inside lives for as long as the work does. On
// anything longer than the ceiling the agent meets the identical block, writes
// the identical reason, and a second grant appears in the ledger saying what the
// first one already said. Recording the conclusion where the gate reads scope
// from ends that loop without loosening anything: the widened path is still
// bounded by every rung above it, and every write it authorises is marked
// widened rather than clean.
//
// Only scope.unplanned is persisted. It is the one rule that means "the plan did
// not mention this file" — a gap in the plan, which is the thing worth closing.
// The others mean the plan mentioned it and said no, and closing that gap is
// editing someone else's decision rather than completing it.
//
// The human approval path deliberately does not come through here. An operator
// answering "allow this write?" consented to a write, not to an amendment of the
// plan, and taking the second from the first would be answering a question they
// were not asked.
func (r *Registry) persistWidenedScope(ctx context.Context, decision policy.Decision, op dc.Operation, payload map[string]any) {
	if decision.Rule != policy.RuleUnplannedScope {
		return
	}

	// The target is a concrete path and planned files are matched as fnmatch
	// patterns, so it is quoted to cover itself and nothing else. Unquoted, a
	// real file named "a[bc].go" would widen scope to "ab.go" and "ac.go" as
	// well — the same defect the grant ledger quotes its own paths against.
	entry := dc.PlannedFile{
		Path:          fnmatch.QuoteMeta(decision.Target),
		AllowedChange: appendedChangeFor(op),
	}
	holder := r.sessionFor(ctx).State()
	added, err := r.deps.Store.AppendPlannedFiles(ctx, holder.TaskID, holder.Token, []dc.PlannedFile{entry})
	if err != nil {
		// Reported, not fatal. The grant already authorises this write, so
		// failing the call would refuse work that policy has allowed; what is
		// lost is only the durability, and the agent has to know that so it can
		// expect the block again rather than being surprised by it.
		payload["scope_persisted"] = false
		payload["scope_error"] = err.Error()
		payload["note"] = "granted, but this widening could not be written into the task's scope: " +
			"the grant stands and expires, and the same block will return"
		return
	}
	payload["scope_persisted"] = true
	if len(added) == 0 {
		// Nothing to add means the path was already in scope — the grant was
		// for a rung this path no longer reaches.
		payload["scope_note"] = "already in the task's scope; nothing appended"
		return
	}
	payload["scope_appended"] = entry.Path
	payload["scope_note"] = "written into this task's scope for the rest of its life; " +
		"writes it authorises report as widened, never as clean"
}

// appendedChangeFor is the permission an appended entry carries.
//
// It matches the operation that was blocked rather than defaulting to modify.
// An entry appended as modify would refuse a later delete of the same path under
// scope.operation — a rule no agent may grant itself — which would leave the
// agent worse off for having widened its scope than it was before.
func appendedChangeFor(op dc.Operation) dc.AllowedChange {
	if op == dc.OpDelete {
		return dc.ChangeDelete
	}
	return dc.ChangeModify
}

// humanPath names the recovery that remains when an agent may not grant a rule
// itself. It is subject-aware for the same reason the payload is: a command
// block's remedy is the task's allowed_commands, and telling an agent to update
// its planned files sends it to widen the wrong list.
func humanPath(d policy.Decision) string {
	if d.Severity == policy.Hard {
		return "this rule is never grantable; change the approach"
	}
	if policy.IsCommandRule(d.Rule) {
		return "ask a human to grant this (manvi allow --cmd), or add the command to the " +
			"task's allowed_commands. " + ungatedToolSentence
	}
	return "ask a human to grant this, or update the task's planned files"
}
