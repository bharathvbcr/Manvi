package flags

import (
	"strings"
	"testing"
)

// TestYoloPostureTurnsBothGatesOff pins what the option actually does. yolo is
// a claim about the gates, not a label: if it resolved to anything other than
// off, the approval cards it exists to stop would still be raised.
func TestYoloPostureTurnsBothGatesOff(t *testing.T) {
	r := testRegistry(t)
	if err := r.Set(Human, HarnessPosture, PostureYolo); err != nil {
		t.Fatal(err)
	}

	for _, gate := range []string{PolicyFileMode, PolicyCommandMode} {
		mode, origin, err := EffectiveGateMode(r, gate)
		if err != nil {
			t.Fatal(err)
		}
		if mode != ModeOff {
			t.Fatalf("%s = %q, want off under the yolo posture", gate, mode)
		}
		// The origin has to name the posture rather than the gate flag: an
		// operator reading "off" needs to know which setting to change, and
		// policy.file.mode is not the one that decided.
		if !strings.Contains(string(origin), HarnessPosture) || !strings.Contains(string(origin), PostureYolo) {
			t.Fatalf("%s origin = %q, want it to name harness.posture=yolo", gate, origin)
		}
	}
}

// TestExplicitModeWinsOverYolo: --yolo is a default-setter, not an override. An
// operator who pinned a gate to enforce typed that on purpose, and a posture
// that silently unpinned it would make the stricter setting unreachable.
func TestExplicitModeWinsOverYolo(t *testing.T) {
	r := testRegistry(t)
	if err := r.Set(Human, HarnessPosture, PostureYolo); err != nil {
		t.Fatal(err)
	}
	if err := r.Set(Human, PolicyFileMode, ModeEnforce); err != nil {
		t.Fatal(err)
	}

	mode, origin, err := EffectiveGateMode(r, PolicyFileMode)
	if err != nil {
		t.Fatal(err)
	}
	if mode != ModeEnforce || origin != OriginOverride {
		t.Fatalf("mode/origin = %q/%q, want the explicit enforce to survive yolo", mode, origin)
	}
	// The other gate, untouched, still follows the posture.
	if mode, _, _ := EffectiveGateMode(r, PolicyCommandMode); mode != ModeOff {
		t.Fatalf("command gate = %q, want off — only the pinned gate is exempt", mode)
	}
}

// TestYoloIsReportedAsWeakened is the honesty half of the feature. A run with
// the gates off must not be able to describe itself as strict.
func TestYoloIsReportedAsWeakened(t *testing.T) {
	r := testRegistry(t)
	if err := r.Set(Human, HarnessPosture, PostureYolo); err != nil {
		t.Fatal(err)
	}
	weak := r.Weakened()
	if len(weak) != 1 || weak[0].Key != HarnessPosture || weak[0].Raw != PostureYolo {
		t.Fatalf("Weakened = %+v, want the yolo posture reported", weak)
	}
	if weak[0].Origin != OriginOverride {
		t.Fatalf("origin = %q, want the layer that set it", weak[0].Origin)
	}
}

// TestAgentCannotEnterYolo: the posture is the switch that decides whether the
// gate asks before a write lands. An agent able to move it has no gate.
func TestAgentCannotEnterYolo(t *testing.T) {
	r := testRegistry(t)
	if err := r.Set(Agent, HarnessPosture, PostureYolo); err == nil {
		t.Fatal("an agent must not be able to put its own harness in yolo posture")
	}
	if v, _ := r.Lookup(HarnessPosture); v.Raw != PostureDev {
		t.Fatalf("posture = %q after a refused set, want the default untouched", v.Raw)
	}
}

// TestYoloIsALegalValueAndTyposAreNot keeps the enum doing its job: the new
// posture is accepted through every layer, and a near-miss still fails loudly
// rather than falling back to a default nobody asked for.
func TestYoloIsALegalValueAndTyposAreNot(t *testing.T) {
	r := testRegistry(t)
	if err := r.LoadConfig(map[string]string{HarnessPosture: PostureYolo}); err != nil {
		t.Fatalf("config layer must accept the yolo posture: %v", err)
	}
	r2 := testRegistry(t)
	if err := r2.LoadConfig(map[string]string{HarnessPosture: "yolo!"}); err == nil {
		t.Fatal("a mistyped posture must be an error, not a silent default")
	}
}

// TestDescribePostureSaysWhatStillBlocks. The notice is the only thing standing
// between "I turned off the prompts" and "I turned off the harness", so it has
// to state the limit as well as the relaxation — and a posture that relaxes
// nothing must produce no notice at all, or every strict run prints a warning
// about nothing.
func TestDescribePostureSaysWhatStillBlocks(t *testing.T) {
	yolo := DescribePosture(PostureYolo)
	if !yolo.Relaxed || yolo.Short == "" || yolo.Notice == "" {
		t.Fatalf("yolo effect = %+v, want it described as relaxed with both lengths filled", yolo)
	}
	// The notice has to name what stopped enforcing. An operator who reads
	// "gates off" and assumes credential paths — or the repository boundary —
	// survived is exactly the reader this sentence exists for.
	for _, want := range []string{"hard rules included", "credential paths", "git safety", "repository boundary", "land anywhere"} {
		if !strings.Contains(strings.ToLower(yolo.Notice), want) {
			t.Fatalf("yolo notice %q does not mention %q", yolo.Notice, want)
		}
	}
	if dev := DescribePosture(PostureDev); !dev.Relaxed || dev.Short == yolo.Short {
		t.Fatalf("dev effect = %+v, want it relaxed and worded differently from yolo", dev)
	}
	if strict := DescribePosture(PostureStrict); strict.Relaxed || strict.Notice != "" {
		t.Fatalf("strict effect = %+v, want nothing reported as relaxed", strict)
	}
	// An unrecognised value is not evidence of a relaxation, and must not be
	// described as one.
	if unknown := DescribePosture("something-else"); unknown.Relaxed {
		t.Fatal("an unknown posture must not be described as relaxed")
	}
}

// TestUnknownPostureEnforces: DescribePosture stays quiet about a value it does
// not know, but the gate mode it resolves to must be the strictest one. Silence
// and permission are different answers.
func TestUnknownPostureEnforces(t *testing.T) {
	r := New()
	if err := r.Define(Def{
		Key: HarnessPosture, Kind: KindString, Default: "posture-from-the-future",
		Mutable: HumanOnly,
	}, Def{
		Key: PolicyFileMode, Kind: KindEnum, Values: []string{ModeEnforce, ModeAdvisory, ModeOff},
		Default: ModeEnforce, Mutable: HumanOnly,
	}); err != nil {
		t.Fatal(err)
	}
	mode, _, err := EffectiveGateMode(r, PolicyFileMode)
	if err != nil {
		t.Fatal(err)
	}
	if mode != ModeEnforce {
		t.Fatalf("mode = %q, want enforce for a posture this code cannot map", mode)
	}
}

// TestYoloTurnsOffTheHardRules pins the resolution the gate and doctor share.
// The flag itself is startup-only and stays where the operator left it; the
// posture is what changes the answer.
func TestYoloTurnsOffTheHardRules(t *testing.T) {
	r := testRegistry(t)
	on, _, err := EffectiveHardRules(r)
	if err != nil {
		t.Fatal(err)
	}
	if !on {
		t.Fatal("hard rules must run under the shipped dev posture")
	}

	if err := r.Set(Human, HarnessPosture, PostureYolo); err != nil {
		t.Fatal(err)
	}
	on, origin, err := EffectiveHardRules(r)
	if err != nil {
		t.Fatal(err)
	}
	if on {
		t.Fatal("hard rules must not run under the yolo posture")
	}
	if !strings.Contains(string(origin), HarnessPosture) || !strings.Contains(string(origin), PostureYolo) {
		t.Fatalf("origin = %q, want it to name harness.posture=yolo", origin)
	}

	// The flag is untouched: yolo resolves the value, it does not write to a
	// startup-only setting behind the operator's back.
	if v, _ := r.Lookup(PolicyHardRules); v.Raw != "true" || v.Origin != OriginDefault {
		t.Fatalf("policy.hard_rules.enabled = %+v, want it left where it was", v)
	}
}

// TestExplicitHardRulesBeatThePosture in both directions: yolo does not
// overrule a value someone typed, and neither does any other posture.
func TestExplicitHardRulesBeatThePosture(t *testing.T) {
	r := testRegistry(t)
	if err := r.LoadConfig(map[string]string{
		HarnessPosture:  PostureYolo,
		PolicyHardRules: "true",
	}); err != nil {
		t.Fatal(err)
	}
	on, origin, err := EffectiveHardRules(r)
	if err != nil {
		t.Fatal(err)
	}
	if !on {
		t.Fatal("an explicit policy.hard_rules.enabled=true must survive the yolo posture")
	}
	if origin != OriginConfig {
		t.Fatalf("origin = %q, want the layer that set the flag", origin)
	}

	// And the flag can still be turned off without yolo, as it always could.
	r2 := testRegistry(t)
	if err := r2.LoadConfig(map[string]string{PolicyHardRules: "false"}); err != nil {
		t.Fatal(err)
	}
	if on, _, _ := EffectiveHardRules(r2); on {
		t.Fatal("policy.hard_rules.enabled=false must still turn the hard rules off on its own")
	}
}

// TestUnknownPostureLeavesHardRulesOn: an unmappable posture is a reason to
// keep enforcing, and the hard rules are the last thing that should follow a
// value this code cannot interpret.
func TestUnknownPostureLeavesHardRulesOn(t *testing.T) {
	r := New()
	if err := r.Define(Def{
		Key: HarnessPosture, Kind: KindString, Default: "posture-from-the-future", Mutable: HumanOnly,
	}, Def{
		Key: PolicyHardRules, Kind: KindBool, Default: "true", Mutable: Startup, Safety: true,
	}); err != nil {
		t.Fatal(err)
	}
	if on, _, err := EffectiveHardRules(r); err != nil || !on {
		t.Fatalf("hard rules = %v (err %v), want them left on", on, err)
	}
}
