package policy

import "testing"

// TestEveryClassifiedRuleHasASubject pins the two classification maps to one
// key set.
//
// The override seam routes a recovery by subject. A rule with a severity and no
// subject would default to "path", which is exactly the defect this
// classification was added to make unrepresentable: a command rule silently
// routed to a file-write evaluator. So adding a rule to severities without
// adding it here is a test failure rather than a live mis-route.
func TestEveryClassifiedRuleHasASubject(t *testing.T) {
	for rule := range severities {
		if _, ok := subjects[rule]; !ok {
			t.Errorf("rule %q has a severity but no subject; add it to subjects", rule)
		}
	}
	for rule := range subjects {
		if _, ok := severities[rule]; !ok {
			t.Errorf("rule %q has a subject but no severity; add it to severities", rule)
		}
	}
}

// TestCommandRulesAreClassifiedAsCommands is the assertion the defect would
// have failed. Every rule the command gate can return names a command.
func TestCommandRulesAreClassifiedAsCommands(t *testing.T) {
	for _, rule := range []RuleID{
		RuleCommandEmpty, RuleCommandNoLease, RuleCommandNotAllowed,
		RuleCommandBypassFlag, RuleCommandForcePush,
		RuleCommandProtectedReset, RuleCommandProtectedPush,
	} {
		if !IsCommandRule(rule) {
			t.Errorf("%q is a command rule but SubjectOf says %q", rule, SubjectOf(rule))
		}
	}
	for _, rule := range []RuleID{
		RuleMalformedPath, RuleOutsideRoot, RuleSecretPath, RuleRestrictedPath,
		RuleNoTask, RuleForbiddenChange, RuleUnplannedScope, RuleReadOnly,
		RuleOperation, RuleProtectedWrite,
	} {
		if IsCommandRule(rule) {
			t.Errorf("%q is a file rule but SubjectOf says command", rule)
		}
	}
}

// TestEveryCommandGateRuleIsReachableFromTheGate guards the direction the maps
// cannot: that the command gate does not return a rule nobody classified. It
// drives the real ladder rather than reading the map.
func TestEveryCommandGateRuleIsReachableFromTheGate(t *testing.T) {
	g := CommandGate{HardRules: true}
	for _, tc := range []struct{ command, want string }{
		{"", string(RuleCommandEmpty)},
		{"curl http://example.com", string(RuleCommandNoLease)},
		{"git commit --no-verify -m x", string(RuleCommandBypassFlag)},
		{"git push origin --force", string(RuleCommandForcePush)},
		{"git reset --hard origin/main", string(RuleCommandProtectedReset)},
	} {
		d := g.EvaluateCommand(tc.command, nil)
		if string(d.Rule) != tc.want {
			t.Errorf("EvaluateCommand(%q) rule = %q, want %q", tc.command, d.Rule, tc.want)
			continue
		}
		if !IsCommandRule(d.Rule) {
			t.Errorf("EvaluateCommand(%q) returned %q, which is not classified as a command rule",
				tc.command, d.Rule)
		}
	}
}

// TestSettlePathReachesATrueFixedPoint pins the property the normalizer needs
// and did not have: running it on its own output must change nothing.
//
// The counterexample a fuzzer found is in the table. `""0"/"/` needed three
// rounds, and the code ran two — so it settled on `"0"`, which settled again on
// `0`. Two spellings, one file, and a gate that would have evaluated them as
// two different things.
func TestSettlePathReachesATrueFixedPoint(t *testing.T) {
	for _, raw := range []string{
		`""0"/"/`, `"0"/`, `"0"\.`, `"""a"""`, `"a"/`, `""a"/"/`,
		`"""""x"""""/////`, `  "  a  "  `, `a/./b/../c`, `"./a"/`,
		`"..."/`, `""`, `"`, ``, `/`, `///`, `"/"`, `"a/b"/`,
		`""""`, `"""`, `.`, `..`, `"."/`, `a`, `"a"`,
	} {
		once, settled := settlePath(raw)
		if !settled {
			t.Errorf("settlePath(%q) never converged", raw)
			continue
		}
		twice, settledAgain := settlePath(once)
		if !settledAgain || twice != once {
			t.Errorf("settlePath(%q) = %q, which settles again to %q — not a fixed point",
				raw, once, twice)
		}
	}
}

// FuzzSettlePathIsIdempotent is the same property without a table.
func FuzzSettlePathIsIdempotent(f *testing.F) {
	for _, seed := range []string{`""0"/"/`, `"0"/`, `a/b`, `"`, ``, `///`, `"".."/"`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		once, settled := settlePath(raw)
		if !settled {
			// Failing closed is allowed; producing an unsettled value is not.
			if once != "" {
				t.Fatalf("settlePath(%q) reported no convergence but returned %q", raw, once)
			}
			return
		}
		twice, settledAgain := settlePath(once)
		if !settledAgain || twice != once {
			t.Fatalf("settlePath(%q) = %q, which settles again to %q", raw, once, twice)
		}
	})
}
