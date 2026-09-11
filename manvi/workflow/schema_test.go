package workflow

import (
	"encoding/json"
	"testing"
)

func schemaBase(t *testing.T) Capability {
	t.Helper()
	return Capability{
		SchemaVersion: 1,
		ID:            "schema.probe",
		Revision:      "1",
		Application:   "bank",
		Targets: map[string]Selector{
			"search":  {Role: "button", Name: "Search"},
			"balance": {Role: "text_field", Name: "Balance"},
			"status":  {Role: "text_field", Name: "Status"},
			"dismiss": {Role: "button", Name: "Dismiss"},
		},
		Outputs: []OutputDecl{
			{Name: "balance", Type: "money", Currency: "USD", Description: "Verified USD balance"},
		},
		Outcomes: []OutcomeDef{
			{ID: "success", Kind: "success", Description: "Member balance retrieved", Outputs: []string{"balance"}},
			{ID: "not_found", Kind: "business", Description: "Member not found", Outputs: []string{}},
		},
		Limits: DefaultLimits(),
		Steps: []Step{
			{ID: "search", Kind: "press", Target: "search", Effect: "read"},
			{ID: "route", Kind: "branch", Target: "status", Effect: "read", Predicate: Predicate{Op: "equals", Expected: Ref{Source: "literal", Literal: Value{Type: "string", Text: "Member found"}}}, Next: "balance", Otherwise: "missing"},
			{ID: "balance", Kind: "extract", Target: "balance", Effect: "read", Output: "balance", OutputType: "money", Currency: "USD", Next: "ok"},
			{ID: "ok", Kind: "conclude", Outcome: "success"},
			{ID: "missing", Kind: "conclude", Outcome: "not_found"},
		},
	}
}

func mustCompile(t *testing.T, c Capability) *Program {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Compile(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCompileRejectsConcludeCycles(t *testing.T) {
	c := schemaBase(t)
	c.Steps = []Step{
		{ID: "a", Kind: "conclude", Outcome: "success", Next: "b"},
		{ID: "b", Kind: "conclude", Outcome: "not_found", Next: "a"},
	}
	if err := compileCapability(c); err == nil {
		t.Fatal("conclude cycle compiled")
	}
}

func TestCompileRejectsMixedLadderAndFlatSelector(t *testing.T) {
	c := schemaBase(t)
	c.Targets["search"] = Selector{
		Role:       "button",
		Name:       "Search",
		Strategies: []Selector{{Role: "button", Name: "Search"}},
		Rationale:  "primary search control",
		Stability:  "semantic",
	}
	if err := compileCapability(c); err == nil {
		t.Fatal("mixed ladder/flat selector compiled")
	}
}

func TestCompileRejectsEmptyLadder(t *testing.T) {
	c := schemaBase(t)
	c.Targets["search"] = Selector{
		Strategies: []Selector{},
		Rationale:  "empty",
		Stability:  "semantic",
	}
	if err := compileCapability(c); err == nil {
		t.Fatal("empty ladder compiled")
	}
}

func TestCompileRejectsNestedStrategiesOnRungs(t *testing.T) {
	c := schemaBase(t)
	c.Targets["search"] = Selector{
		Strategies: []Selector{{
			Role:       "button",
			Name:       "Search",
			Strategies: []Selector{{Role: "button", Name: "Find"}},
			Rationale:  "nested",
			Stability:  "semantic",
		}},
		Rationale: "search control",
		Stability: "semantic",
	}
	if err := compileCapability(c); err == nil {
		t.Fatal("nested strategies compiled")
	}
}

func TestCompileRejectsWrongTypeOutputsOnSuccessPath(t *testing.T) {
	c := schemaBase(t)
	c.Outputs = []OutputDecl{{Name: "balance", Type: "string", Description: "wrong declared type"}}
	if err := compileCapability(c); err == nil {
		t.Fatal("wrong-type output contract compiled")
	}
}

func TestCompileAcceptsLadderTargetsAndShorthand(t *testing.T) {
	c := schemaBase(t)
	c.Targets["search"] = Selector{
		Strategies: []Selector{{Role: "button", Name: "Search"}, {Identifier: "search.button"}},
		Rationale:  "semantic first, identifier fallback",
		Stability:  "semantic",
	}
	if err := compileCapability(c); err != nil {
		t.Fatal(err)
	}
	// Existing fixtures stay valid via flat shorthand.
	if err := compileCapability(fixture(t, false).Capability()); err != nil {
		t.Fatal(err)
	}
}

func pressThenPostObserve(t *testing.T, p *Program, s State) State {
	t.Helper()
	e := observed(s)
	e.Observation.Target = p.spec.Steps[s.StepIndex].Target
	var err error
	s, _, err = Reduce(p, s, e, nil)
	if err != nil {
		t.Fatal(err)
	}
	e = inputEvent(s, "receipt")
	e.Delivery = "sent"
	s, _, err = Reduce(p, s, e, nil)
	if err != nil {
		t.Fatal(err)
	}
	e = observed(s)
	e.ActionID = s.ActionID(p)
	s, _, err = Reduce(p, s, e, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBusinessConcludeIsNotFailed(t *testing.T) {
	p := mustCompile(t, schemaBase(t))
	s := pressThenPostObserve(t, p, start(t, p))
	e := observed(s)
	e.ActionID = s.ActionID(p)
	e.Observation = Observation{ID: "obs2", TargetID: "status", Target: "status", Matches: 1, Complete: true, Value: Value{Type: "string", Text: "Member not found"}}
	s, cmds, err := Reduce(p, s, e, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Phase != Concluded || s.Outcome != "not_found" || len(cmds) != 0 {
		t.Fatalf("business conclude wrong state: %+v cmds=%v", s, cmds)
	}
	if s.Phase == Failed {
		t.Fatal("business outcome reported as failed")
	}
	result := ResultFromState(p, s)
	if result.Kind != ResultBusinessOutcome || result.Outcome != "not_found" {
		t.Fatalf("result=%+v", result)
	}
}

func TestSuccessConcludeCompletesWithOutputs(t *testing.T) {
	p := mustCompile(t, schemaBase(t))
	s := pressThenPostObserve(t, p, start(t, p))
	e := observed(s)
	e.ActionID = s.ActionID(p)
	e.Observation = Observation{ID: "obs2", TargetID: "status", Target: "status", Matches: 1, Complete: true, Value: Value{Type: "string", Text: "Member found"}}
	var err error
	s, _, err = Reduce(p, s, e, nil)
	if err != nil || s.Phase != Observing {
		t.Fatalf("branch to extract failed: %+v %v", s, err)
	}
	e = observed(s)
	e.ActionID = s.ActionID(p)
	e.Observation = Observation{ID: "obs3", TargetID: "bal", Target: "balance", Matches: 1, Complete: true, StrategyIndex: 0, Value: Value{Type: "money", Integer: 125000, Currency: "USD"}}
	s, cmds, err := Reduce(p, s, e, nil)
	if err != nil || s.Phase != Completed || s.Outcome != "success" || s.Outputs["balance"].Integer != 125000 || len(cmds) != 0 {
		t.Fatalf("success conclude failed: %+v cmds=%v err=%v", s, cmds, err)
	}
	result := ResultFromState(p, s)
	if result.Kind != ResultSuccess {
		t.Fatalf("result=%+v", result)
	}
}

func TestRecoveryMaxBoundPreventsLoops(t *testing.T) {
	c := schemaBase(t)
	c.Recoveries = []Recovery{{
		ID:     "dismiss_overlay",
		When:   RecoveryWhen{Target: "search", Predicate: Predicate{Op: "equals", Expected: Ref{Source: "literal", Literal: Value{Type: "string", Text: "blocked"}}}},
		Action: RecoveryAction{Kind: "press", Target: "dismiss"},
		Max:    1,
	}}
	c.Steps = []Step{
		{ID: "search", Kind: "press", Target: "search", Effect: "read"},
		{ID: "ok", Kind: "conclude", Outcome: "success"},
	}
	c.Outputs = nil
	c.Outcomes = []OutcomeDef{{ID: "success", Kind: "success", Description: "ok", Outputs: []string{}}}
	p := mustCompile(t, c)
	s := start(t, p)

	applyBlocked := func(state State) (State, []Command) {
		e := observed(state)
		e.Observation = Observation{
			ID: "obs", TargetID: "node", Target: "search", Matches: 0, Complete: true,
			Value: Value{Type: "string", Text: "blocked"},
		}
		next, cmds, err := Reduce(p, state, e, nil)
		if err != nil {
			t.Fatal(err)
		}
		return next, cmds
	}

	s, cmds := applyBlocked(s)
	if s.Phase != Acting || s.ActiveRecovery != "dismiss_overlay" || len(cmds) != 1 || cmds[0].RecoveryID != "dismiss_overlay" {
		t.Fatalf("recovery not applied: %+v %+v", s, cmds)
	}
	e := inputEvent(s, "receipt")
	e.Delivery = "sent"
	s, cmds, err := Reduce(p, s, e, nil)
	if err != nil || s.Phase != Observing || s.ActiveRecovery != "" || s.StepIndex != 0 || len(cmds) != 1 || cmds[0].Kind != "observe" {
		t.Fatalf("recovery receipt did not reobserve: %+v %+v %v", s, cmds, err)
	}
	s, cmds = applyBlocked(s)
	if s.ActiveRecovery != "" || s.RecoveryCounts["dismiss_overlay"] != 1 {
		t.Fatalf("recovery exceeded max: %+v cmds=%v", s, cmds)
	}
	// Without another recovery, unresolved observation retries then pauses.
	for s.Phase == Observing {
		s, cmds = applyBlocked(s)
		if s.ActiveRecovery != "" {
			t.Fatal("recovery looped past max")
		}
		if len(cmds) == 0 {
			break
		}
	}
	if s.Phase != Paused {
		t.Fatalf("expected pause after recovery max + retries, got %+v", s)
	}
}

func TestStrategyIndexOutOfRangeRejected(t *testing.T) {
	c := schemaBase(t)
	c.Targets["search"] = Selector{
		Strategies: []Selector{{Role: "button", Name: "Search"}},
		Rationale:  "one rung",
		Stability:  "semantic",
	}
	c.Steps = []Step{
		{ID: "search", Kind: "press", Target: "search", Effect: "read"},
		{ID: "ok", Kind: "conclude", Outcome: "success"},
	}
	c.Outputs = nil
	c.Outcomes = []OutcomeDef{{ID: "success", Kind: "success", Description: "ok", Outputs: []string{}}}
	p := mustCompile(t, c)
	s := start(t, p)
	e := observed(s)
	e.Observation.Target = "search"
	e.Observation.StrategyIndex = 3
	if _, _, err := Reduce(p, s, e, nil); err == nil {
		t.Fatal("out-of-range strategy_index accepted")
	}
}

func TestResultKinds(t *testing.T) {
	p := mustCompile(t, schemaBase(t))
	cases := []struct {
		phase   Phase
		outcome string
		want    ResultKind
	}{
		{Completed, "success", ResultSuccess},
		{Concluded, "not_found", ResultBusinessOutcome},
		{Failed, "", ResultHardFailure},
		{Cancelled, "", ResultCancelled},
		{Unknown, "", ResultOutcomeUnknown},
		{Paused, "", ResultRecoverableExhausted},
	}
	for _, tc := range cases {
		s := State{Phase: tc.phase, Outcome: tc.outcome, CapabilitySHA256: p.Digest(), Outputs: map[string]Value{}}
		got := ResultFromState(p, s)
		if got.Kind != tc.want || got.Outcome != tc.outcome {
			t.Fatalf("phase=%s got=%+v want kind=%s", tc.phase, got, tc.want)
		}
	}
}

func TestTargetAliasKeepsSelectorMapsCompatible(t *testing.T) {
	var _ map[string]Target = map[string]Selector{"x": {Name: "X"}}
	var _ map[string]Selector = map[string]Target{"x": {Name: "X"}}
	s := Selector{Strategies: []Selector{{Name: "A"}, {Name: "B"}}, Rationale: "r", Stability: "semantic"}
	if s.Primary().Name != "A" || len(s.Ladder()) != 2 {
		t.Fatal("primary/ladder mismatch")
	}
	flat := Selector{Name: "Only"}
	if flat.Primary().Name != "Only" || len(flat.Ladder()) != 1 {
		t.Fatal("shorthand ladder mismatch")
	}
	cloned := s.Clone()
	cloned.Strategies[0].Name = "mutated"
	if s.Strategies[0].Name != "A" {
		t.Fatal("Clone did not deep-copy strategies")
	}
}

func TestMigratedExampleShapeCompilesAtSchemaVersion1(t *testing.T) {
	raw := []byte(`{
  "schema_version":1,"id":"bank.balance","revision":"1","application":"jarvis-bank",
  "parameters":[{"name":"member_id","type":"string","sensitive":true}],
  "targets":{
    "member":{"strategies":[{"role":"text_field","name":"Member ID"}],"rationale":"member field","stability":"semantic"},
    "search":{"strategies":[{"role":"button","name":"Search"}],"rationale":"search","stability":"semantic"},
    "status":{"strategies":[{"role":"text_field","name":"Status"}],"rationale":"status","stability":"semantic"},
    "balance":{"strategies":[{"role":"text_field","name":"Balance"}],"rationale":"balance","stability":"semantic"}
  },
  "outputs":[{"name":"balance","type":"money","currency":"USD","description":"USD balance"}],
  "outcomes":[
    {"id":"success","kind":"success","description":"found","outputs":["balance"]},
    {"id":"not_found","kind":"business","description":"missing","outputs":[]}
  ],
  "steps":[
    {"id":"member","kind":"set_value","target":"member","effect":"read","input":{"source":"input","key":"member_id"}},
    {"id":"search","kind":"press","target":"search","effect":"read"},
    {"id":"settle","kind":"wait","target":"status","effect":"read","predicate":{"op":"contains","expected":{"source":"literal","literal":{"type":"string","text":"Member"}}}},
    {"id":"route","kind":"branch","target":"status","effect":"read","predicate":{"op":"equals","expected":{"source":"literal","literal":{"type":"string","text":"Member found"}}},"next":"balance","otherwise":"not_found"},
    {"id":"balance","kind":"extract","target":"balance","effect":"read","output":"balance","output_type":"money","currency":"USD","next":"ok"},
    {"id":"ok","kind":"conclude","outcome":"success"},
    {"id":"not_found","kind":"conclude","outcome":"not_found"}
  ],
  "limits":{"max_actions":40,"active_seconds":300,"observation_attempts":3,"intervention_seconds":600}
}`)
	p, err := Compile(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Capability().SchemaVersion != 1 {
		t.Fatal("schema_version must stay 1")
	}
}
