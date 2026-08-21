package flags

import (
	"strings"
	"testing"
	"time"
)

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	r := New()
	if err := DefineHarnessFlags(r); err != nil {
		t.Fatalf("define harness flags: %v", err)
	}
	return r
}

// TestUnknownConfigKeyIsAnError is the rule the whole package exists for: a
// mistyped setting must fail, not silently do nothing.
func TestUnknownConfigKeyIsAnError(t *testing.T) {
	r := testRegistry(t)
	err := r.LoadConfig(map[string]string{"policy.file.mod": "off"})
	if err == nil {
		t.Fatal("a typo'd key must be rejected")
	}
	if !strings.Contains(err.Error(), "policy.file.mod") {
		t.Fatalf("error = %v, want it to name the unknown key", err)
	}
}

func TestInvalidValueIsAnError(t *testing.T) {
	r := testRegistry(t)
	err := r.LoadConfig(map[string]string{PolicyFileMode: "advisery"})
	if err == nil || !strings.Contains(err.Error(), "expected one of") {
		t.Fatalf("error = %v, want an enum validation failure", err)
	}

	err = r.LoadConfig(map[string]string{PolicyHardRules: "flase"})
	if err == nil || !strings.Contains(err.Error(), "bool") {
		t.Fatalf("error = %v, want a bool validation failure", err)
	}
}

func TestLayerPrecedenceAndOrigin(t *testing.T) {
	r := testRegistry(t)
	r.envLookup = func(key string) (string, bool) {
		if key == EnvKey(PolicyFileMode) {
			return ModeAdvisory, true
		}
		return "", false
	}

	if v, _ := r.Lookup(PolicyFileMode); v.Origin != OriginDefault || v.Raw != ModeEnforce {
		t.Fatalf("default = %+v, want enforce/default", v)
	}

	if err := r.LoadConfig(map[string]string{PolicyFileMode: ModeOff}); err != nil {
		t.Fatal(err)
	}
	if v, _ := r.Lookup(PolicyFileMode); v.Origin != OriginConfig || v.Raw != ModeOff {
		t.Fatalf("config layer = %+v, want off/config", v)
	}

	if err := r.LoadEnv(); err != nil {
		t.Fatal(err)
	}
	if v, _ := r.Lookup(PolicyFileMode); v.Origin != OriginEnv || v.Raw != ModeAdvisory {
		t.Fatalf("env layer = %+v, want advisory/env", v)
	}

	if err := r.Set(Human, PolicyFileMode, ModeEnforce); err != nil {
		t.Fatal(err)
	}
	if v, _ := r.Lookup(PolicyFileMode); v.Origin != OriginOverride || v.Raw != ModeEnforce {
		t.Fatalf("override layer = %+v, want enforce/override", v)
	}
}

func TestEnvKey(t *testing.T) {
	if got := EnvKey("policy.file.mode"); got != "MANVI_POLICY_FILE_MODE" {
		t.Fatalf("EnvKey = %q", got)
	}
}

func TestBadEnvValueIsReportedNotIgnored(t *testing.T) {
	r := testRegistry(t)
	r.envLookup = func(key string) (string, bool) {
		if key == EnvKey(AgentsMaxFanout) {
			return "lots", true
		}
		return "", false
	}
	err := r.LoadEnv()
	if err == nil || !strings.Contains(err.Error(), "MANVI_AGENTS_MAX_FANOUT") {
		t.Fatalf("error = %v, want the env var named", err)
	}
}

// TestAgentCannotChangeSafetyFlags is the authority rule: an agent that can
// turn off its own gate has no gate.
func TestAgentCannotChangeSafetyFlags(t *testing.T) {
	r := testRegistry(t)
	for _, key := range []string{PolicyFileMode, GrantsAgentEnabled, GrantsAgentMaxTTL, VerifyRigorEnabled} {
		if err := r.Set(Agent, key, defaultOpposite(t, r, key)); err == nil {
			t.Fatalf("an agent must not be able to change %q", key)
		}
	}
	// A human can.
	if err := r.Set(Human, PolicyFileMode, ModeAdvisory); err != nil {
		t.Fatalf("a human should be able to change the gate mode: %v", err)
	}
}

func TestStartupFlagsFreezeAfterSeal(t *testing.T) {
	r := testRegistry(t)
	if err := r.Set(Human, PolicyHardRules, "false"); err != nil {
		t.Fatalf("startup flag should be settable before Seal: %v", err)
	}
	r.Seal()
	if err := r.Set(Human, PolicyHardRules, "true"); err == nil {
		t.Fatal("a startup flag must not be changeable after Seal")
	}
}

func TestSafetyFlagsCannotBeDeclaredAgentSettable(t *testing.T) {
	r := New()
	err := r.Define(Def{
		Key: "bad.flag", Kind: KindBool, Default: "true",
		Mutable: AgentSettable, Safety: true,
	})
	if err == nil || !strings.Contains(err.Error(), "safety flag") {
		t.Fatalf("error = %v, want a safety/agent-settable refusal", err)
	}
}

// TestWeakenedReportsOnlyMovedSafetyFlags is what keeps a green run honest.
func TestWeakenedReportsOnlyMovedSafetyFlags(t *testing.T) {
	r := testRegistry(t)

	// The shipped defaults are not the safest configuration: the harness
	// deliberately starts in dev posture so it can be run while it is being
	// built. That must appear in Weakened even though nobody changed it —
	// otherwise a report would call the relaxed default "unchanged, therefore
	// fine", which is the one summary that must never be possible.
	weak := r.Weakened()
	if len(weak) != 1 || weak[0].Key != HarnessPosture || weak[0].Raw != PostureDev {
		t.Fatalf("Weakened = %+v, want the dev posture reported on a default registry", weak)
	}

	// Moving a safety flag to its safest value removes it from the list.
	if err := r.Set(Human, HarnessPosture, PostureStrict); err != nil {
		t.Fatal(err)
	}
	if len(r.Weakened()) != 0 {
		t.Fatalf("a fully strict registry has nothing weakened, got %v", r.Weakened())
	}

	// A non-safety flag moving is not a weakening.
	if err := r.Set(Human, AgentsMaxFanout, "4"); err != nil {
		t.Fatal(err)
	}
	if len(r.Weakened()) != 0 {
		t.Fatalf("non-safety flags must not appear in Weakened, got %v", r.Weakened())
	}

	if err := r.Set(Human, PolicyFileMode, ModeOff); err != nil {
		t.Fatal(err)
	}
	weak = r.Weakened()
	if len(weak) != 1 || weak[0].Key != PolicyFileMode || weak[0].Origin != OriginOverride {
		t.Fatalf("Weakened = %+v, want the gate mode override", weak)
	}
}

// TestEverySafetyFlagDeclaresASafestValue: a safety flag whose safest setting
// is not stated cannot be reported on, and Weakened would quietly treat its
// default as safe. Checking the catalogue rather than a list keeps a new flag
// from being added without the decision being made.
func TestEverySafetyFlagDeclaresASafestValue(t *testing.T) {
	r := testRegistry(t)
	for _, key := range r.Keys() {
		d, _ := r.Def(key)
		if !d.Safety {
			continue
		}
		safest := d.Safest
		if safest == "" {
			safest = d.Default
		}
		if err := validate(d, safest); err != nil {
			t.Errorf("%s: safest value %q is not a legal value: %v", key, safest, err)
		}
	}
}

func TestTypedAccessors(t *testing.T) {
	r := testRegistry(t)
	if d, _, err := r.Duration(GrantsAgentMaxTTL); err != nil || d != 15*time.Minute {
		t.Fatalf("agent TTL = %v, %v", d, err)
	}
	if n, _, err := r.Int(AgentsMaxSpawnDepth); err != nil || n != 2 {
		t.Fatalf("spawn depth = %v, %v", n, err)
	}
	if b, _, err := r.Bool(PolicyHardRules); err != nil || !b {
		t.Fatalf("hard rules = %v, %v", b, err)
	}
	if _, _, err := r.Bool("nope.not.here"); err == nil {
		t.Fatal("an unknown key must error rather than return false silently")
	}
}

// TestProviderBaseURLsMatchVerifiedDocs pins the endpoints to what each
// vendor's current documentation says. They were deliberately blank until
// verified; a default that looks like a fact but was written from memory is
// worse than no default, because it fails as a 404 rather than a missing
// setting.
func TestProviderBaseURLsMatchVerifiedDocs(t *testing.T) {
	r := testRegistry(t)
	for key, want := range map[string]string{
		LLMXAIBaseURL:    "https://api.x.ai/v1",
		LLMGeminiBaseURL: "https://generativelanguage.googleapis.com/v1beta",
	} {
		got, _, err := r.String(key)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// defaultOpposite returns a legal value different from the flag's default, so
// the mutability test actually attempts a change.
func defaultOpposite(t *testing.T, r *Registry, key string) string {
	t.Helper()
	d, ok := r.Def(key)
	if !ok {
		t.Fatalf("no such flag %q", key)
	}
	switch d.Kind {
	case KindBool:
		if d.Default == "true" {
			return "false"
		}
		return "true"
	case KindEnum:
		for _, v := range d.Values {
			if v != d.Default {
				return v
			}
		}
	case KindDuration:
		return "1m"
	case KindInt:
		return "99"
	}
	return d.Default + "x"
}

// TestEffectiveGateModeIsWhatTheGateActuallyRuns pins the resolution both the
// gate and `manvi doctor` read. They had to be one function: a doctor that
// prints "enforce" while the gate runs advisory is a report that actively
// misleads, and two implementations of one rule drift by default.
func TestEffectiveGateModeIsWhatTheGateActuallyRuns(t *testing.T) {
	r := testRegistry(t)

	// Default posture is dev, and the mode is untouched, so the posture decides.
	mode, origin, err := EffectiveGateMode(r, PolicyFileMode)
	if err != nil {
		t.Fatal(err)
	}
	if mode != ModeAdvisory {
		t.Fatalf("mode = %q, want advisory under the dev posture", mode)
	}
	if !strings.Contains(string(origin), HarnessPosture) {
		t.Fatalf("origin = %q, want it to name the setting that decided", origin)
	}

	// An explicit mode wins over the posture.
	if err := r.Set(Human, PolicyFileMode, ModeEnforce); err != nil {
		t.Fatal(err)
	}
	if mode, origin, _ = EffectiveGateMode(r, PolicyFileMode); mode != ModeEnforce || origin != OriginOverride {
		t.Fatalf("mode/origin = %q/%q, want an explicit setting to win", mode, origin)
	}

	// Strict posture enforces an untouched mode.
	r2 := testRegistry(t)
	if err := r2.Set(Human, HarnessPosture, PostureStrict); err != nil {
		t.Fatal(err)
	}
	if mode, _, _ = EffectiveGateMode(r2, PolicyCommandMode); mode != ModeEnforce {
		t.Fatalf("mode = %q, want enforce under the strict posture", mode)
	}
}

// The rename from devharness to manvi renamed every variable with it. A stale
// export has to be reported, because the alternative is a run that quietly took
// the default — including for the safety flags, whose whole purpose is to be
// reported when they are off their default.
func TestStaleEnvNamesTheReplacement(t *testing.T) {
	stale := StaleEnv([]string{
		"PATH=/usr/bin",
		"DEVHARNESS_POLICY_FILE_MODE=report",
		"MANVI_POLICY_FILE_MODE=enforce",
		"DEVHARNESS_COVERAGE=/tmp/cover.out",
		"MALFORMED",
	})
	if len(stale) != 2 {
		t.Fatalf("found %d stale variables, want 2: %+v", len(stale), stale)
	}
	// Sorted, so the message an operator reads is stable between runs.
	if stale[0].Old != "DEVHARNESS_COVERAGE" || stale[0].New != "MANVI_COVERAGE" {
		t.Fatalf("first rename is %+v", stale[0])
	}
	if stale[1].Old != "DEVHARNESS_POLICY_FILE_MODE" || stale[1].New != EnvKey("policy.file.mode") {
		t.Fatalf("second rename is %+v; New should be exactly what EnvKey now returns", stale[1])
	}
	if len(StaleEnv([]string{"MANVI_COVERAGE=x", "HOME=/home/x"})) != 0 {
		t.Fatal("a clean environment reported stale variables")
	}
}

// TestEffortIsUnsetByDefaultAndReadsFromEnv covers the flag that decides whether
// a reasoning field is sent at all. The default has to be empty rather than a
// tier: any non-empty value here would be the harness choosing a thinking level
// for every model an operator ever configures, and a level chosen for them is
// spent tokens they did not ask for.
func TestEffortIsUnsetByDefaultAndReadsFromEnv(t *testing.T) {
	r := testRegistry(t)
	value, err := r.Lookup(LLMEffort)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if value.Raw != "" {
		t.Fatalf("llm.effort defaults to %q; an unset effort must omit the field entirely", value.Raw)
	}
	if value.Origin != OriginDefault {
		t.Fatalf("origin = %q, want default", value.Origin)
	}

	r.envLookup = func(key string) (string, bool) {
		if key == "MANVI_LLM_EFFORT" {
			return "high", true
		}
		return "", false
	}
	if err := r.LoadEnv(); err != nil {
		t.Fatalf("load env: %v", err)
	}
	got, origin, err := r.String(LLMEffort)
	if err != nil || got != "high" || origin != OriginEnv {
		t.Fatalf("String(llm.effort) = %q, %q, %v", got, origin, err)
	}
}

// TestEffortIsNotAnEnum: the accepted levels differ per model, so a fixed list
// in the catalogue would refuse a value some model genuinely serves. Anthropic's
// xhigh is the case that proves it — it is legal on two models in this repo's
// catalogue and on no Gemini model.
func TestEffortIsNotAnEnum(t *testing.T) {
	r := testRegistry(t)
	if err := r.Set(Human, LLMEffort, "xhigh"); err != nil {
		t.Fatalf("Set(llm.effort, xhigh) = %v; the flag layer must not gate on a per-model vocabulary", err)
	}
}

// TestLocalSamplingDefaultsMatchWhatModelsShip guards the defaults against
// drifting back to values that contradict the model's own configuration.
func TestLocalSamplingDefaultsMatchWhatModelsShip(t *testing.T) {
	r := testRegistry(t)
	for _, tc := range []struct{ key, want, why string }{
		{LLMLocalTemperature, "0.7",
			"0.1 approximates greedy decoding, which thinking models document as a cause of repetition loops"},
		{LLMLocalTopK, "20",
			"Qwen3's shipped generation_config declares top_k 20 and the harness could not send it at all"},
		{LLMLocalTopP, "0.95", "matches the shipped generation_config"},
		{LLMLocalRepetitionPenalty, "", "not part of any shipped configuration; unset omits the field"},
		{LLMLocalSeed, "", "unset means the server picks; setting it makes a run reproducible"},
	} {
		val, err := r.Lookup(tc.key)
		if err != nil {
			t.Fatalf("lookup %s: %v", tc.key, err)
		}
		if val.Raw != tc.want {
			t.Errorf("%s default = %q, want %q — %s", tc.key, val.Raw, tc.want, tc.why)
		}
	}
}

func TestLocalLLMFlagsMinPAndStop(t *testing.T) {
	r := testRegistry(t)
	minPVal, err := r.Lookup(LLMLocalMinP)
	if err != nil {
		t.Fatalf("lookup LLMLocalMinP: %v", err)
	}
	// Empty by default, deliberately. min_p is not part of the sampling
	// configuration open-weight models ship beside their weights — Qwen3
	// declares temperature, top_k and top_p and says nothing about min_p — so
	// sending a value nobody tuned for is a change to the model's behaviour
	// that no one asked for. Empty omits the field; the operator can set it.
	if minPVal.Raw != "" {
		t.Errorf("expected min_p to default to unset, got %q", minPVal.Raw)
	}

	stopVal, err := r.Lookup(LLMLocalStop)
	if err != nil {
		t.Fatalf("lookup LLMLocalStop: %v", err)
	}
	if stopVal.Raw != "" {
		t.Errorf("expected default stop '', got %q", stopVal.Raw)
	}

	if err := r.Set(Human, LLMLocalMinP, "0.08"); err != nil {
		t.Fatalf("Set min_p: %v", err)
	}
	if err := r.Set(Human, LLMLocalStop, "<|im_end|>,<|endoftext|>"); err != nil {
		t.Fatalf("Set stop: %v", err)
	}

	val, _, _ := r.String(LLMLocalMinP)
	if val != "0.08" {
		t.Errorf("expected min_p 0.08, got %q", val)
	}
	val, _, _ = r.String(LLMLocalStop)
	if val != "<|im_end|>,<|endoftext|>" {
		t.Errorf("expected stop '<|im_end|>,<|endoftext|>', got %q", val)
	}
}

// Surrounding whitespace must mean the same thing for every Kind.
//
// It did not: a string flag's consumer trimmed before parsing, so " 0.7" was
// accepted, while a bool flag parsed the raw string, so " true" was a hard
// startup error. Which one an operator hit depended only on the Kind of the
// flag their heredoc or YAML happened to add a space to.
func TestSurroundingWhitespaceIsAcceptedForEveryKind(t *testing.T) {
	reg, err := NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ key, raw, want string }{
		{LLMLocalCoreToolsOnly, " true ", "true"},
		{LLMLocalTemperature, " 0.7 ", "0.7"},
		{LLMLocalTopK, " 20 ", "20"},
		{LLMLocalStallTimeout, " 5m ", "5m"},
	}
	for _, tc := range cases {
		if err := reg.Set(Human, tc.key, tc.raw); err != nil {
			t.Errorf("%s = %q was refused: %v", tc.key, tc.raw, err)
			continue
		}
		got, _, err := reg.String(tc.key)
		if err != nil {
			t.Errorf("%s: %v", tc.key, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s stored %q, want %q — Raw is not canonical", tc.key, got, tc.want)
		}
	}
}

// TestUndeclaredMutabilityIsRefused closes a fail-open default. Mutability is
// a string, so a Def that simply omits it carries the zero value — and the
// authority switch in Set matched none of the three cases and fell through to
// the assignment, which made "I forgot to say who may change this" mean "anyone
// may, including the agent". A permission that defaults to granted is the one
// class of default this package exists to refuse.
func TestUndeclaredMutabilityIsRefused(t *testing.T) {
	r := New()
	err := r.Define(Def{Key: "no.mutability", Kind: KindBool, Default: "true"})
	if err == nil || !strings.Contains(err.Error(), "mutability") {
		t.Fatalf("error = %v, want a refusal naming the missing mutability", err)
	}

	err = r.Define(Def{Key: "odd.mutability", Kind: KindBool, Default: "true", Mutable: Mutability("sometimes")})
	if err == nil || !strings.Contains(err.Error(), "sometimes") {
		t.Fatalf("error = %v, want a refusal naming the unknown mutability", err)
	}
}

// TestSetRefusesUnknownMutability is the same rule enforced at the other end.
// Define is the boot-time guard; this is the runtime one, for a registry built
// by a path that did not go through Define — a test helper, a future loader —
// so the fall-through can never reappear as a silent grant.
func TestSetRefusesUnknownMutability(t *testing.T) {
	r := New()
	// Injected past Define deliberately: the point is that Set does not trust
	// its definitions to have been validated.
	r.defs["ghost"] = Def{Key: "ghost", Kind: KindBool, Default: "false"}
	r.order = append(r.order, "ghost")

	for _, auth := range []Authority{Agent, Human} {
		if err := r.Set(auth, "ghost", "true"); err == nil {
			t.Fatalf("%s was allowed to set a flag with no declared mutability", auth)
		}
	}
	if v, _ := r.Lookup("ghost"); v.Raw != "false" || v.Origin != OriginDefault {
		t.Fatalf("value = %+v, want the refusal to have left the default in place", v)
	}
}

// TestEveryHarnessFlagDeclaresMutability asserts the shipped catalogue, not
// just the validator. A flag that reaches the catalogue without an authority is
// a setting the /flags report describes with an empty column.
func TestEveryHarnessFlagDeclaresMutability(t *testing.T) {
	r := testRegistry(t)
	for _, key := range r.Keys() {
		d, ok := r.Def(key)
		if !ok {
			t.Fatalf("%s: defined key has no definition", key)
		}
		switch d.Mutable {
		case Startup, HumanOnly, AgentSettable:
		default:
			t.Errorf("%s declares mutability %q, which is not one of startup/human/agent", key, d.Mutable)
		}
	}
}

// TestFlagReachCoversTheCatalogue keeps the classification total. A flag added
// to the catalogue is classified by ReachOf whether or not anyone thought about
// it, and the answer it falls into by default — "live, the change lands
// immediately" — is the reassuring one. This asserts the two things that make
// that default safe to keep: every key resolves to a Reach this package
// defines, and every prefix in the table still matches a flag, so a rename
// cannot leave a snapshotted namespace silently reclassified as live.
func TestFlagReachCoversTheCatalogue(t *testing.T) {
	r := testRegistry(t)
	for _, key := range r.Keys() {
		d, _ := r.Def(key)
		switch ReachOf(d) {
		case ReachLive, ReachReload, ReachNewSession, ReachBoot:
		default:
			t.Errorf("%s: ReachOf returned %q, which is not a defined Reach", key, ReachOf(d))
		}
	}

	for _, entry := range reachByPrefix {
		matched := false
		for _, key := range r.Keys() {
			if strings.HasPrefix(key, entry.prefix) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("reachByPrefix names %q, which matches no flag in the catalogue — "+
				"a renamed namespace has silently become 'live'", entry.prefix)
		}
	}
}

// TestStartupFlagsReachOnlyBoot pins the one classification that is not a
// courtesy: a startup flag cannot be moved after Seal, so reporting anything
// other than boot would describe a change the registry is about to refuse.
func TestStartupFlagsReachOnlyBoot(t *testing.T) {
	r := testRegistry(t)
	sawStartup := false
	for _, key := range r.Keys() {
		d, _ := r.Def(key)
		if d.Mutable != Startup {
			continue
		}
		sawStartup = true
		if got := ReachOf(d); got != ReachBoot {
			t.Errorf("%s is startup-only but reaches %q", key, got)
		}
	}
	if !sawStartup {
		t.Fatal("no startup flags in the catalogue; this test asserts nothing")
	}
}
