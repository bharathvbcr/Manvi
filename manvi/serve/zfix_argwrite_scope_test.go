package serve

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
)

// gitPulseScope is the scope GitPulse sends for a checkout bound to a task that
// plans one file.
func gitPulseScope() *HostScope {
	return &HostScope{TaskID: "TASK-FENCE", PlannedFiles: []string{"src/planned.rs"}}
}

// The fence has to hold for every command that writes a file through its
// arguments, not only for the redirect and the four (sed -i, tee, cp, mv) the
// harness first read. A write outside the plan is the soft scope.unplanned
// denial, which a declared scope keeps from being demoted; a credential and a
// path outside the root are the hard ones.
func TestServeFenceHoldsForEveryArgumentWriter(t *testing.T) {
	root := t.TempDir()
	commands := map[string]string{
		"rm":       "rm TARGET",
		"truncate": "truncate -s 0 TARGET",
		"touch":    "touch TARGET",
		"dd":       "dd if=seed.txt of=TARGET",
		"perl -i":  "perl -pi -e 's/a/b/' TARGET",
		"install":  "install seed.txt TARGET",
		"ln":       "ln -sf seed.txt TARGET",
		"rsync":    "rsync seed.txt TARGET",
		"patch":    "patch TARGET < x.diff",
		"sed -i":   "sed -i s/a/b/ TARGET",
		"tee":      "printf x | tee TARGET",
		"cp":       "cp seed.txt TARGET",
		"mv":       "mv src/planned.rs TARGET",
	}
	classes := []struct {
		target string
		deny   bool
		rule   policy.RuleID
	}{
		{"src/planned.rs", false, ""},
		{"docs/other.md", true, policy.RuleUnplannedScope},
		{".env", true, policy.RuleSecretPath},
		{"../outside.txt", true, policy.RuleOutsideRoot},
	}
	for name, template := range commands {
		for _, class := range classes {
			command := strings.ReplaceAll(template, "TARGET", class.target)
			d := decodeDecision(t, roundTrip(t, hostOpts(), commandCheckWith(t, CommandCheckParams{
				Command: command, Root: root, Scope: gitPulseScope(),
			}))[0])
			if class.deny {
				if d.Action != policy.Deny || d.Rule != class.rule || d.Severity != policy.SeverityOf(class.rule) {
					t.Errorf("%s: %q = %s/%s/%s (%s), want %s/%s", name, command,
						d.Action, d.Rule, d.Severity, d.Reason, policy.Deny, class.rule)
				}
				continue
			}
			// mv removes its source too, so moving the planned file onto
			// itself is the only in-plan spelling and says nothing new.
			if d.Action == policy.Deny && name != "mv" {
				t.Errorf("%s: %q into the planned file was refused: %s/%s", name, command, d.Rule, d.Reason)
			}
		}
	}
}

// Writes whose targets the command text does not name are refused under a bound
// scope, and only then.
func TestServeFenceRefusesWritesItCannotJudgeOnlyUnderAScope(t *testing.T) {
	root := t.TempDir()
	for _, command := range []string{
		"git apply x.diff",
		"patch -p1 < x.diff",
		"echo docs/other.md | xargs rm",
		"find docs -name other.md -exec rm {} +",
		"find docs -delete",
	} {
		d := decodeDecision(t, roundTrip(t, hostOpts(), commandCheckWith(t, CommandCheckParams{
			Command: command, Root: root, Scope: gitPulseScope(),
		}))[0])
		if d.Action != policy.Deny || d.Rule != policy.RuleCommandSubstitution || !strings.Contains(d.Reason, "declared scope") {
			t.Errorf("%q with a scope = %s/%s (%s), want a command.substitution refusal naming the scope",
				command, d.Action, d.Rule, d.Reason)
		}
		unscoped := decodeDecision(t, roundTrip(t, hostOpts(), commandCheckWith(t, CommandCheckParams{
			Command: command, Root: root,
		}))[0])
		if unscoped.Action == policy.Deny {
			t.Errorf("%q with no scope was refused: %s/%s", command, unscoped.Rule, unscoped.Reason)
		}
	}
	// A scope that plans nothing is still a scope: every write is unplanned.
	d := decodeDecision(t, roundTrip(t, hostOpts(), commandCheckWith(t, CommandCheckParams{
		Command: "xargs rm", Root: root, Scope: &HostScope{TaskID: "TASK-EMPTY"},
	}))[0])
	if d.Action != policy.Deny {
		t.Errorf("empty scope: xargs rm = %s/%s, want a refusal", d.Action, d.Rule)
	}
}
