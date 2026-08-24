package flags

import (
	"strings"
	"testing"
)

// TestLocalSamplingDefaultsMatchWhatTheCatalogueClaims.
//
// The temperature entry's comment cited Qwen3's generation_config.json —
// "temperature 1.0 with top_k 20 and top_p 0.95" — beside a shipped default of
// 0.7, and said nothing about the gap. A reader had to guess whether 0.7 was a
// deliberate departure or a stale number nobody had reconciled, which is the
// one thing a comment on a default exists to answer.
//
// The two values transcribed straight from that file are pinned here so they
// cannot drift from what the comment says they are. The one that departs is
// checked differently: it has to say so in the Description, because the
// Description is what `manvi flags` shows an operator and a deviation nobody
// can see from outside the source file is not an acknowledged one.
//
// Verified 2026-08-18 against the weights on this machine —
// ~/.cache/huggingface/hub/models--mlx-community--Qwen3.8-27B-4bit/snapshots/
// 3e6447f082e89cc7f0bc6e5441afd38dfce760ff/generation_config.json declares
// {"temperature": 1.0, "top_k": 20, "top_p": 0.95}; the nvfp4 snapshot agrees.
func TestLocalSamplingDefaultsMatchWhatTheCatalogueClaims(t *testing.T) {
	reg := New()
	if err := DefineHarnessFlags(reg); err != nil {
		t.Fatal(err)
	}

	// Transcribed from generation_config.json, unchanged.
	for key, want := range map[string]string{
		LLMLocalTopK: "20",
		LLMLocalTopP: "0.95",
	} {
		d, ok := reg.Def(key)
		if !ok {
			t.Fatalf("%s is not defined", key)
		}
		if d.Default != want {
			t.Errorf("%s default = %q, want %q — the catalogue describes this as Qwen3's "+
				"shipped value, so changing it means changing the claim too", key, d.Default, want)
		}
	}

	// The one that departs from upstream.
	d, ok := reg.Def(LLMLocalTemperature)
	if !ok {
		t.Fatal("llm.local.temperature is not defined")
	}
	if d.Default != "0.7" {
		t.Errorf("temperature default = %q, want 0.7", d.Default)
	}
	if d.Default == "1.0" {
		t.Fatal("temperature now matches upstream; the Description must stop calling it a departure")
	}
	for _, want := range []string{"0.7", "1.0"} {
		if !strings.Contains(d.Description, want) {
			t.Errorf("the temperature Description does not mention %q. It ships %q while Qwen3's "+
				"generation_config.json recommends 1.0, and an operator reading `manvi flags` must "+
				"be told that is deliberate rather than left to find the gap themselves",
				want, d.Default)
		}
	}
}

// TestTheDelegationBoundsHaveCeilings.
//
// agents.max_fanout was validated by strconv.Atoi and nothing else, so
// `agents.max_fanout: 100000` was accepted from a config file, reported by
// `manvi flags` as though it were a considered setting, and never mentioned in
// Weakened(). It is not a queue depth: Pool.Run launches one goroutine per task
// and refuses a batch wider than the limit, so the number is how many model
// turns run at once.
func TestTheDelegationBoundsHaveCeilings(t *testing.T) {
	for _, tc := range []struct {
		name, key, value string
	}{
		{"fanout past the ceiling", AgentsMaxFanout, "100000"},
		{"fanout below one", AgentsMaxFanout, "0"},
		{"fanout negative", AgentsMaxFanout, "-1"},
		{"depth past the ceiling", AgentsMaxSpawnDepth, "99"},
		{"depth negative", AgentsMaxSpawnDepth, "-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := New()
			if err := DefineHarnessFlags(reg); err != nil {
				t.Fatal(err)
			}
			err := reg.LoadConfig(map[string]string{tc.key: tc.value})
			if err == nil {
				v, _ := reg.Lookup(tc.key)
				t.Fatalf("%s = %q was accepted (now %q); an out-of-range bound must be refused, not taken at its word",
					tc.key, tc.value, v.Raw)
			}
			// Refused, not clamped: the message has to name the setting and the
			// range, or the operator cannot tell what value would be taken.
			for _, want := range []string{tc.key, tc.value} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to mention %q", err, want)
				}
			}
			// And nothing was applied — LoadConfig stages before it commits.
			if v, _ := reg.Lookup(tc.key); v.Origin != OriginDefault {
				t.Errorf("%s = %q (%s) after a refused load", tc.key, v.Raw, v.Origin)
			}
		})
	}

	// The legal ends of both ranges still load, or the ceiling is just breakage.
	reg := New()
	if err := DefineHarnessFlags(reg); err != nil {
		t.Fatal(err)
	}
	// The ceiling is spelled out rather than read from the constant: an
	// operator reads a number, and a test that agreed with whatever the source
	// currently says would not notice the ceiling moving.
	if err := reg.LoadConfig(map[string]string{
		AgentsMaxFanout:     "32",
		AgentsMaxSpawnDepth: "0",
	}); err != nil {
		t.Fatalf("the legal ends of the ranges were refused: %v", err)
	}
}

// TestBoundsOnlyBindWhereTheyAreDeclared holds the other edge: the range check
// runs for a flag that declares a ceiling and for no other, so every int flag
// that has never needed one keeps the contract it shipped with. A bound
// declared on anything but an int is refused at Define, because a limit that is
// published in the catalogue and enforced nowhere is worse than no limit.
func TestBoundsOnlyBindWhereTheyAreDeclared(t *testing.T) {
	reg := New()
	if err := DefineHarnessFlags(reg); err != nil {
		t.Fatal(err)
	}
	// llm.local.seed declares no bounds, so its full int range stands.
	if err := reg.LoadConfig(map[string]string{LLMLocalSeed: "-9223372036854775808"}); err != nil {
		t.Errorf("an unbounded int flag rejected a value: %v", err)
	}

	for _, d := range []Def{
		{Key: "probe.string.bounded", Kind: KindString, Mutable: HumanOnly, Max: 4},
		{Key: "probe.empty.range", Kind: KindInt, Default: "1", Mutable: HumanOnly, Min: 9, Max: 4},
		{Key: "probe.default.outside", Kind: KindInt, Default: "50", Mutable: HumanOnly, Min: 1, Max: 4},
	} {
		if err := New().Define(d); err == nil {
			t.Errorf("%s was defined; a bound that cannot bind must fail at boot", d.Key)
		}
	}
}
