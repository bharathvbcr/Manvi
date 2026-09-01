package serve

import (
	"testing"

	"manvi/dc"
	"manvi/policy"
)

// A host that declares a task scope must be judged against it — and a refusal
// measured against a real scope must not be demoted by a posture whose stated
// reason is that no such scope exists.

func scopedServer(t *testing.T, posture Posture) *Server {
	t.Helper()
	return &Server{posture: posture, hardRules: true}
}

const (
	plannedFile   = "src/lib/agents/activity.ts"
	unplannedFile = "src/lib/stores/graphStore.ts"
)

func hostScope() *HostScope {
	return &HostScope{
		TaskID:       "TASK-42",
		PlannedFiles: []string{plannedFile},
	}
}

func TestDeclaredScopeReachesTheScopeRungs(t *testing.T) {
	s := scopedServer(t, PostureDevCouncil)

	in := s.evaluateHostWrite(t.TempDir(), plannedFile, dc.OpModify, false, hostScope())
	if in.Blocked() {
		t.Errorf("a planned file was refused: rule=%q reason=%q", in.Rule, in.Reason)
	}

	out := s.evaluateHostWrite(t.TempDir(), unplannedFile, dc.OpModify, false, hostScope())
	if out.Rule != policy.RuleUnplannedScope {
		t.Errorf("unplanned write: rule = %q, want %q", out.Rule, policy.RuleUnplannedScope)
	}
	if !out.Blocked() {
		t.Errorf("unplanned write was not blocked: action=%q", out.Action)
	}
}

// The regression this whole change exists to prevent.
func TestHostPostureDoesNotDemoteARealScopeViolation(t *testing.T) {
	s := scopedServer(t, PostureHost)

	d := s.evaluateHostWrite(t.TempDir(), unplannedFile, dc.OpModify, false, hostScope())

	if d.Rule != policy.RuleUnplannedScope {
		t.Fatalf("rule = %q, want %q", d.Rule, policy.RuleUnplannedScope)
	}
	if !d.Blocked() {
		t.Errorf(
			"a write outside the declared plan came back action=%q; "+
				"posture=host demoted a rung that had run and refused",
			d.Action,
		)
	}
	if d.Demoted != "" {
		t.Errorf(
			"Demoted = %q; the reason 'no task model in the embedding host' is "+
				"false once the host declares one",
			d.Demoted,
		)
	}
}

// ...and the behaviour for hosts that declare nothing is unchanged, which is
// what makes this additive rather than breaking.
func TestHostPostureStillDemotesWhenNoScopeIsDeclared(t *testing.T) {
	s := scopedServer(t, PostureHost)

	d := s.evaluateHostWrite(t.TempDir(), unplannedFile, dc.OpModify, false, nil)

	if d.Rule != policy.RuleNoTask {
		t.Fatalf("rule = %q, want %q", d.Rule, policy.RuleNoTask)
	}
	if d.Blocked() {
		t.Errorf("a host with no task model must still be let through: action=%q", d.Action)
	}
	if d.Demoted == "" {
		t.Error("the demotion must still be recorded, or the allow reads as clean")
	}
}

func TestHardRulesAreNeverDemotedWithOrWithoutAScope(t *testing.T) {
	// The scope machinery must not have widened anything. A credential path is
	// Hard, and no scope, posture or grant clears one.
	for _, name := range []string{"with scope", "without scope"} {
		var scope *HostScope
		if name == "with scope" {
			scope = &HostScope{TaskID: "TASK-42", PlannedFiles: []string{".env"}}
		}
		s := scopedServer(t, PostureHost)
		d := s.evaluateHostWrite(t.TempDir(), ".env", dc.OpModify, false, scope)
		if !d.Blocked() {
			t.Errorf("%s: writing .env was allowed (rule=%q)", name, d.Rule)
		}
		if d.Severity != policy.Hard {
			t.Errorf("%s: severity = %q, want hard", name, d.Severity)
		}
	}
}

func TestForbiddenChangesAreHonoured(t *testing.T) {
	s := scopedServer(t, PostureDevCouncil)
	scope := &HostScope{
		TaskID:           "TASK-42",
		PlannedFiles:     []string{plannedFile},
		ForbiddenChanges: []string{plannedFile},
	}
	d := s.evaluateHostWrite(t.TempDir(), plannedFile, dc.OpModify, false, scope)
	if d.Rule != policy.RuleForbiddenChange {
		t.Errorf("rule = %q, want %q — forbidden must outrank planned", d.Rule, policy.RuleForbiddenChange)
	}
}

func TestAScopeWithNoFilesAuthorisesNothing(t *testing.T) {
	// The fail-closed reading. An empty planned list is a real value: the task
	// authorises no file, so every write is unplanned. Treating it as "no
	// scope" would silently re-enable the demotion.
	s := scopedServer(t, PostureHost)
	d := s.evaluateHostWrite(t.TempDir(), plannedFile, dc.OpModify, false, &HostScope{TaskID: "TASK-42"})
	if !d.Blocked() {
		t.Errorf("an empty plan authorised a write: rule=%q action=%q", d.Rule, d.Action)
	}
}

func TestTheDecisionNamesTheTaskItWasMeasuredAgainst(t *testing.T) {
	// An audit must be able to trace a verdict to the task, not to a
	// placeholder that every host shares.
	s := scopedServer(t, PostureDevCouncil)
	d := s.evaluateHostWrite(t.TempDir(), unplannedFile, dc.OpModify, false, hostScope())
	if d.TaskID != "TASK-42" {
		t.Errorf("TaskID = %q, want TASK-42", d.TaskID)
	}
}

func TestAnIdlessScopeStillNamesSomething(t *testing.T) {
	s := scopedServer(t, PostureDevCouncil)
	d := s.evaluateHostWrite(t.TempDir(), unplannedFile, dc.OpModify, false, &HostScope{})
	if d.TaskID != hostScopeID {
		t.Errorf("TaskID = %q, want the %q placeholder", d.TaskID, hostScopeID)
	}
}
