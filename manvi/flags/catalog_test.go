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
