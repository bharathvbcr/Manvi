package gate

import (
	"testing"

	"manvi/dc"
	"manvi/flags"
	"manvi/grants"
	"manvi/policy"
)

// A command line has two verdicts about two different subjects, and clearing
// one must never clear the other.
//
// The defect these pin was introduced by the fix for the redirect rung itself.
// Reconciling the command verdict and the file verdict by how firmly each
// blocked made two *soft* denials tie — command.not_allowed and
// scope.read_only both score the same — and the tie kept the command verdict
// and discarded the file one. A grant naming command.not_allowed then cleared
// the only verdict left, and the write landed on a path whose own rule is not
// grantable by anybody.
//
// Three routes reached it: a human grant on command.not_allowed (no flag
// needed — that is the default policy), an agent self-grant with
// grants.agent.allow_commands on, and no grant at all under
// policy.command.mode=advisory with policy.file.mode=enforce.

// readOnlyTask plans docs/notes.md read-only and allows nothing, so a redirect
// into it is refused by scope.read_only while the command is refused by
// command.not_allowed — two soft denials about two subjects.
func readOnlyTask() *dc.Task {
	return &dc.Task{
		ID: "TASK-RO",
		PlannedFiles: []dc.PlannedFile{
			{Path: "src/calc.go", AllowedChange: dc.ChangeModify},
			{Path: "docs/notes.md", AllowedChange: dc.ChangeReadOnly},
		},
		ForbiddenChanges: []string{"src/legacy/**"},
	}
}

// The rule the write actually hits must not be clearable by a grant naming the
// rule the *command* hit. Stated as a property of the ledger rather than of
// this scenario: scope.read_only is not agent-grantable, and a grant for
// command.not_allowed is not a grant for it either.
func TestACommandGrantDoesNotClearAFileRefusal(t *testing.T) {
	// The grant is obtained honestly, on a command whose redirect target the
	// write gate is happy with, so the ledger really does hold a live grant for
	// command.not_allowed. The question is whether that grant reaches a
	// *different* command whose target the write gate refuses.
	const granted = `nope > src/calc.go`
	const attacked = `nope > docs/notes.md`

	for _, tc := range []struct {
		name      string
		overrides map[string]string
		by        *grants.Grantor
	}{
		{
			name: "human grant on command.not_allowed",
			by:   &grants.Grantor{Authority: grants.Human, ID: "operator"},
		},
		{
			name:      "agent self-grant on command.not_allowed",
			overrides: map[string]string{flags.GrantsAgentCommands: "true"},
			by:        &grants.Grantor{Authority: grants.Agent, ID: "executor"},
		},
		{
			name: "command mode advisory, file mode enforcing, no grant at all",
			overrides: map[string]string{
				flags.PolicyCommandMode: flags.ModeAdvisory,
				flags.PolicyFileMode:    flags.ModeEnforce,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGate(t, tc.overrides)
			task := readOnlyTask()

			// The write on its own is refused, by a rule no agent may clear.
			// That is the baseline the redirect must not fall below.
			direct, err := g.EvaluateWrite("docs/notes.md", task, dc.OpWrite)
			if err != nil {
				t.Fatal(err)
			}
			if !direct.Blocked() || direct.Rule != policy.RuleReadOnly {
				t.Fatalf("precondition: a direct write to docs/notes.md must be refused as read_only, got %+v", direct)
			}

			if tc.by != nil {
				clean, cErr := g.EvaluateCommand(granted, task)
				if cErr != nil {
					t.Fatal(cErr)
				}
				if clean.Rule != policy.RuleCommandNotAllowed {
					t.Fatalf("precondition: %q should stop at command.not_allowed, got %q", granted, clean.Rule)
				}
				if _, gErr := g.RequestOverride(clean, *tc.by, "audit regression", task.ID); gErr != nil {
					t.Fatalf("a grant for %s should be issuable here: %v", clean.Rule, gErr)
				}
				// The grant is live: the command it was argued for now passes.
				if after, aErr := g.EvaluateCommand(granted, task); aErr != nil || after.Blocked() {
					t.Fatalf("precondition: the granted command should now pass, got %+v (err %v)", after, aErr)
				}
			}

			d, err := g.EvaluateCommand(attacked, task)
			if err != nil {
				t.Fatal(err)
			}
			if !d.Blocked() {
				t.Fatalf("%q was allowed (rule=%q grant=%q demoted=%q), but it writes docs/notes.md, "+
					"which the write gate refuses as %s — a verdict about the command cleared a verdict about a file",
					attacked, d.Rule, d.GrantID, d.Demoted, policy.RuleReadOnly)
			}
			if d.Rule != policy.RuleReadOnly {
				t.Errorf("refused as %q; the refusal that should survive is the file's own %q",
					d.Rule, policy.RuleReadOnly)
			}
		})
	}
}

// The same property for a path the task explicitly forbids, which is the other
// soft file rule a command grant used to launder.
func TestACommandGrantDoesNotClearAForbiddenChange(t *testing.T) {
	const command = `nope > src/legacy/old.go`
	g := newGate(t, nil)
	task := readOnlyTask()

	clean, err := g.EvaluateCommand(`nope > src/calc.go`, task)
	if err != nil {
		t.Fatal(err)
	}
	if clean.Rule != policy.RuleCommandNotAllowed {
		t.Fatalf("precondition: got rule %q, want command.not_allowed", clean.Rule)
	}
	if _, gErr := g.RequestOverride(clean, grants.Grantor{Authority: grants.Human, ID: "operator"},
		"audit regression", task.ID); gErr != nil {
		t.Fatalf("a grant for %s should be issuable: %v", clean.Rule, gErr)
	}

	after, err := g.EvaluateCommand(command, task)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Blocked() {
		t.Fatalf("%q was allowed (rule=%q grant=%q), but src/legacy/** is in forbidden_changes",
			command, after.Rule, after.GrantID)
	}
}

// The converse, so the fix above is not simply "refuse more". A grant that
// clears the command rule must still work when the redirect target is one the
// write gate is happy with.
func TestACommandGrantStillClearsACommandRefusalWithACleanTarget(t *testing.T) {
	const command = `nope > src/calc.go`
	g := newGate(t, nil)
	task := readOnlyTask()

	d, err := g.EvaluateCommand(command, task)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Blocked() || d.Rule != policy.RuleCommandNotAllowed {
		t.Fatalf("precondition: %q should be refused as command.not_allowed, got %+v", command, d)
	}
	if _, gErr := g.RequestOverride(d, grants.Grantor{Authority: grants.Human, ID: "operator"},
		"audit regression", task.ID); gErr != nil {
		t.Fatalf("a human grant for %s should be issuable: %v", d.Rule, gErr)
	}

	after, err := g.EvaluateCommand(command, task)
	if err != nil {
		t.Fatal(err)
	}
	if after.Blocked() {
		t.Fatalf("%q writes a planned, writable file and the command rule was granted; "+
			"it must now be allowed, got %+v", command, after)
	}
	if after.GrantID == "" {
		t.Error("the allow should carry the grant that produced it, not read as a clean pass")
	}
}
