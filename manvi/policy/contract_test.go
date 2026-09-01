package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// Manvi owns the verdict contract. These tests assert that the shared schema in
// contracts/ still describes what this package actually produces.
//
// The rule list and the severity map are read from the *source of truth in this
// package* — the `severities` map and the RuleID constants — rather than from a
// transcription. A hand-listed copy would pass forever while drifting, which is
// the failure mode contract tests exist to prevent.

type verdictSchema struct {
	Defs struct {
		RuleID struct {
			Enum []string `json:"enum"`
		} `json:"ruleId"`
		SeverityByRule struct {
			Properties map[string]struct {
				Const string `json:"const"`
			} `json:"properties"`
		} `json:"severityByRule"`
		Action struct {
			Enum []string `json:"enum"`
		} `json:"action"`
		Severity struct {
			Enum []string `json:"enum"`
		} `json:"severity"`
	} `json:"$defs"`
}

func contractsDir(t *testing.T) string {
	t.Helper()
	// contracts/ sits at the repository root; this package is manvi/policy.
	dir, err := filepath.Abs(filepath.Join("..", "..", "contracts"))
	if err != nil {
		t.Fatalf("resolving contracts dir: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("contracts dir not found at %s: %v", dir, err)
	}
	return dir
}

func loadVerdictSchema(t *testing.T) verdictSchema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contractsDir(t), "verdict.schema.json"))
	if err != nil {
		t.Fatalf("reading verdict.schema.json: %v", err)
	}
	var s verdictSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("parsing verdict.schema.json: %v", err)
	}
	return s
}

// allRuleIDs returns every rule this package can put on a decision, taken from
// the severities map plus the empty rule that a clean allow carries.
//
// severities is documented as "the authoritative classification", and a rule
// absent from it is treated as Hard. That makes it the right source: a rule
// that exists as a constant but is in no map cannot be classified, and would be
// a defect in this package rather than a gap in the contract.
func allRuleIDs() []string {
	out := []string{string(RuleNone)}
	for r := range severities {
		out = append(out, string(r))
	}
	sort.Strings(out)
	return out
}

func TestSchemaEnumeratesExactlyThisPackagesRules(t *testing.T) {
	schema := loadVerdictSchema(t)

	got := allRuleIDs()
	want := append([]string(nil), schema.Defs.RuleID.Enum...)
	sort.Strings(want)

	if len(want) == 0 {
		t.Fatal("schema carries no rule enum")
	}

	if len(got) != len(want) {
		t.Errorf("rule count: package has %d, schema has %d", len(got), len(want))
	}

	inSchema := map[string]bool{}
	for _, r := range want {
		inSchema[r] = true
	}
	for _, r := range got {
		if !inSchema[r] {
			t.Errorf("rule %q exists in this package but not in verdict.schema.json; "+
				"a consumer meeting it would not know how to classify it", r)
		}
	}

	inPackage := map[string]bool{}
	for _, r := range got {
		inPackage[r] = true
	}
	for _, r := range want {
		if !inPackage[r] {
			t.Errorf("rule %q is in verdict.schema.json but this package no longer produces it; "+
				"removing a rule is a breaking change and needs a new schema version", r)
		}
	}
}

func TestSchemaRecordsTheSameSeverityAsThisPackage(t *testing.T) {
	schema := loadVerdictSchema(t)
	props := schema.Defs.SeverityByRule.Properties

	if len(props) == 0 {
		t.Fatal("schema carries no severityByRule map")
	}

	for rule, sev := range severities {
		entry, ok := props[string(rule)]
		if !ok {
			t.Errorf("rule %q has severity %q here but no entry in the schema's severityByRule",
				rule, sev)
			continue
		}
		if entry.Const != string(sev) {
			// This one matters more than it looks: severity is what decides
			// whether a denial is demotable or grantable at all.
			t.Errorf("rule %q: this package says %q, schema says %q",
				rule, sev, entry.Const)
		}
	}

	for rule := range props {
		if _, ok := severities[RuleID(rule)]; !ok {
			t.Errorf("schema records a severity for %q, which this package does not classify", rule)
		}
	}
}

func TestSchemaEnumeratesTheActionsAndSeveritiesThisPackageEmits(t *testing.T) {
	schema := loadVerdictSchema(t)

	actions := map[string]bool{}
	for _, a := range schema.Defs.Action.Enum {
		actions[a] = true
	}
	for _, a := range []Action{Allow, Warn, Deny} {
		if !actions[string(a)] {
			t.Errorf("action %q is emitted here but absent from the schema", a)
		}
	}
	if len(actions) != 3 {
		t.Errorf("schema lists %d actions, this package has 3", len(actions))
	}

	sevs := map[string]bool{}
	for _, s := range schema.Defs.Severity.Enum {
		sevs[s] = true
	}
	for _, s := range []Severity{Hard, Soft, WarnSeverity, None} {
		if !sevs[string(s)] {
			t.Errorf("severity %q is emitted here but absent from the schema", s)
		}
	}
	if len(sevs) != 4 {
		t.Errorf("schema lists %d severities, this package has 4", len(sevs))
	}
}

// --- the generated parity fixture -------------------------------------------

type verdictCase struct {
	Name     string `json:"name"`
	Decision *struct {
		Action   string   `json:"action"`
		Rule     string   `json:"rule"`
		Severity string   `json:"severity"`
		Reason   string   `json:"reason"`
		Target   string   `json:"target"`
		Degraded []string `json:"degraded"`
		Demoted  string   `json:"demoted"`
		GrantID  string   `json:"grant_id"`
		Widened  string   `json:"widened"`
	} `json:"decision"`
	Expect string `json:"expect"`
}

// classify is this package's implementation of the shared classification
// function. It is written in terms of the predicates this package already
// exports — Blocked() and Clean() — so that a change to either is caught here
// rather than discovered by a consumer.
func classify(d *Decision) string {
	switch {
	case d == nil:
		return "unchecked"
	case d.Blocked():
		return "denied"
	case d.GrantID != "":
		return "granted"
	case d.Demoted != "":
		return "demoted"
	case d.Widened != "":
		return "widened"
	case len(d.Degraded) > 0:
		return "degraded"
	case d.Action == Warn:
		return "warned"
	default:
		return "clean"
	}
}

func TestClassificationMatchesTheGeneratedCases(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(contractsDir(t), "verdict.cases.json"))
	if err != nil {
		t.Fatalf("reading verdict.cases.json: %v", err)
	}
	var payload struct {
		Version int           `json:"version"`
		Cases   []verdictCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("parsing verdict.cases.json: %v", err)
	}
	if payload.Version != 1 {
		t.Fatalf("cases file is version %d, this test understands 1", payload.Version)
	}
	if len(payload.Cases) < 8 {
		t.Fatalf("only %d cases; the fixture is not covering the decision space", len(payload.Cases))
	}

	seen := map[string]bool{}
	for _, c := range payload.Cases {
		var d *Decision
		if c.Decision != nil {
			d = &Decision{
				Action:   Action(c.Decision.Action),
				Rule:     RuleID(c.Decision.Rule),
				Severity: Severity(c.Decision.Severity),
				Reason:   c.Decision.Reason,
				Target:   c.Decision.Target,
				Degraded: c.Decision.Degraded,
				Demoted:  c.Decision.Demoted,
				GrantID:  c.Decision.GrantID,
				Widened:  c.Decision.Widened,
			}
		}
		got := classify(d)
		seen[c.Expect] = true
		if got != c.Expect {
			t.Errorf("case %q: classified %q, contract says %q", c.Name, got, c.Expect)
		}
	}

	// A fixture that never exercises a state is not evidence about that state.
	for _, state := range []string{
		"unchecked", "denied", "granted", "demoted", "widened", "degraded", "warned", "clean",
	} {
		if !seen[state] {
			t.Errorf("no case in the fixture reaches %q", state)
		}
	}
}

// TestCleanAgreesWithTheClassification pins the one overlap that has bitten
// before: Clean() is the package's own predicate and "clean" is the contract's
// last branch, and the two must not be able to disagree.
func TestCleanAgreesWithTheClassification(t *testing.T) {
	cases := []Decision{
		{Action: Allow},
		{Action: Allow, GrantID: "G1"},
		{Action: Allow, Demoted: "mode=advisory"},
		{Action: Allow, Widened: "src/**"},
		{Action: Allow, Degraded: []string{"repo_map.unavailable"}},
		{Action: Warn, Rule: RuleProtectedWrite},
		{Action: Deny, Rule: RuleSecretPath, Severity: Hard},
	}
	for _, d := range cases {
		d := d
		if got := classify(&d); (got == "clean") != d.Clean() {
			t.Errorf("Clean()=%v but classification=%q for %+v", d.Clean(), got, d)
		}
	}
}
