package main

import (
	"strings"
	"testing"

	"manvi/flags"
	"manvi/llm/gemini"
)

// TestTheEffortCeilingIsCheckedAtAttach covers the other half of the reasoning
// wiring: llm.effort says where a turn starts, llm.effort.max says how far a
// turn that has stopped making progress may climb, and a ceiling the model
// cannot reach has to be refused before a prompt is typed rather than surfacing
// as a refused request mid-turn.
func TestTheEffortCeilingIsCheckedAtAttach(t *testing.T) {
	// The credential resolver is never called: Capability answers from the
	// catalogue and no request is made here.
	provider := gemini.New("", nil)

	cases := []struct {
		name    string
		effort  string
		ceiling string
		model   string
		want    string
		wantErr []string
	}{
		{
			name: "unset stays unset", model: "gemini-3.7-flash", effort: "high", want: "",
		},
		{
			name:  "a ceiling above the starting tier is carried",
			model: "gemini-3.7-flash", effort: "low", ceiling: "high", want: "high",
		},
		{
			name:  "a ceiling the model does not serve is refused",
			model: "gemini-3.7-flash", effort: "low", ceiling: "max",
			wantErr: []string{"MANVI_LLM_EFFORT_MAX", "max", "low medium high"},
		},
		{
			// Not an error the model would ever report: this one is the
			// setting doing nothing, which is worse than the setting failing.
			name:  "a ceiling that is not above the starting tier is refused",
			model: "gemini-3.7-flash", effort: "high", ceiling: "high",
			wantErr: []string{"MANVI_LLM_EFFORT_MAX", "not above"},
		},
		{
			name:  "a ceiling with no starting tier is refused",
			model: "gemini-3.7-flash", ceiling: "high",
			wantErr: []string{"MANVI_LLM_EFFORT_MAX", "starts at"},
		},
		{
			name:  "a model that cannot reason refuses any ceiling",
			model: "gemini-3.5-flash-lite", effort: "low", ceiling: "high",
			wantErr: []string{"MANVI_LLM_EFFORT_MAX", "does not support reasoning"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reg, err := flags.NewHarnessRegistry("")
			if err != nil {
				t.Fatalf("registry: %v", err)
			}
			if c.ceiling != "" {
				if err := reg.Set(flags.Human, flags.LLMEffortCeiling, c.ceiling); err != nil {
					t.Fatalf("set ceiling: %v", err)
				}
			}
			host := &harnessHost{reg: reg}

			got, err := host.resolveEffortCeiling(provider, c.model, c.effort)
			if len(c.wantErr) > 0 {
				if err == nil {
					t.Fatalf("ceiling %q over %q on %s was accepted, want a refusal at attach",
						c.ceiling, c.effort, c.model)
				}
				for _, want := range c.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %q", err.Error(), want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveEffortCeiling: %v", err)
			}
			if got != c.want {
				t.Fatalf("ceiling = %q, want %q", got, c.want)
			}
		})
	}
}

// TestTheCeilingSettingIsResolvedLowestFirst pins the layer order the whole
// settings mechanism promises — default, then the config file, then the
// environment — for the setting that decides how much a stuck turn may spend.
func TestTheCeilingSettingIsResolvedLowestFirst(t *testing.T) {
	reg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	value, origin, err := reg.String(flags.LLMEffortCeiling)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if value != "" || origin != flags.OriginDefault {
		t.Fatalf("%s defaults to %q from %q; an unconfigured ceiling must never raise the tier",
			flags.LLMEffortCeiling, value, origin)
	}

	if err := reg.LoadConfig(map[string]string{flags.LLMEffortCeiling: "medium"}); err != nil {
		t.Fatalf("load config: %v", err)
	}
	if value, origin, _ := reg.String(flags.LLMEffortCeiling); value != "medium" || origin != flags.OriginConfig {
		t.Fatalf("after the config file: %q from %q, want medium from config", value, origin)
	}

	t.Setenv(flags.EnvKey(flags.LLMEffortCeiling), "high")
	if err := reg.LoadEnv(); err != nil {
		t.Fatalf("load env: %v", err)
	}
	if value, origin, _ := reg.String(flags.LLMEffortCeiling); value != "high" || origin != flags.OriginEnv {
		t.Fatalf("after the environment: %q from %q, want high from env", value, origin)
	}
}
