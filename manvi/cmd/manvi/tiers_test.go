package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/llm/budget"
)

// tierRegistry builds the registry the way startup does, from a state directory
// holding config.yaml with the given content ("" for no file).
func tierRegistry(t *testing.T, config string) *flags.Registry {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MANVI_STATE_DIR", dir)
	if config != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reg, err := newOperatorRegistry(configPath())
	if err != nil {
		t.Fatalf("newOperatorRegistry: %v", err)
	}
	reg.Seal()
	return reg
}

func TestTierDefaultsAreTheDecidedProfile(t *testing.T) {
	reg := tierRegistry(t, "")
	want := map[agents.Tier]string{
		agents.TierAdvisor: "claude-fable-5-1",
		agents.TierMain:    "claude-opus-5-5",
		agents.TierHeavy:   "claude-opus-5-5",
		agents.TierLite:    "claude-sonnet-5-5",
		agents.TierBasic:   "claude-haiku-5-5",
	}
	rows, err := resolveTierTable(reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(want) {
		t.Fatalf("%d rows, want %d", len(rows), len(want))
	}
	for _, row := range rows {
		if row.Model != want[row.Tier] || row.ModelOrigin != flags.OriginDefault || row.Effort != "" {
			t.Errorf("%s = %+v", row.Tier, row)
		}
	}
	if err := validateTiers(reg, anthropicTierCatalogue(), false); err != nil {
		t.Fatalf("the defaults must validate against the real catalogue: %v", err)
	}
	if err := validateTiers(reg, anthropicTierCatalogue(), true); err != nil {
		t.Fatalf("the defaults must be priced, since a cap may be set: %v", err)
	}
}

func TestTierMainSuppliesTheMainModelAndMANVIMODELStillWins(t *testing.T) {
	reg := tierRegistry(t, "")
	t.Setenv("MANVI_MODEL", "")
	model, source, err := resolveModelFor(context.Background(), anthropic.Name, nil, reg)
	if err != nil || model != "claude-opus-5-5" || source != SourceTier {
		t.Fatalf("got %q, %q, %v; want the main tier's model from %q", model, source, err, SourceTier)
	}

	t.Setenv("MANVI_LLM_TIER_MAIN", "claude-sonnet-5-5")
	reg = tierRegistry(t, "")
	if model, _, _ = resolveModelFor(context.Background(), anthropic.Name, nil, reg); model != "claude-sonnet-5-5" {
		t.Errorf("env-set tier.main = %q", model)
	}

	t.Setenv("MANVI_MODEL", "claude-haiku-5-5")
	model, source, err = resolveModelFor(context.Background(), anthropic.Name, nil, reg)
	if err != nil || model != "claude-haiku-5-5" || source != SourceEnv {
		t.Fatalf("MANVI_MODEL must win: got %q, %q, %v", model, source, err)
	}
}

func TestTierMainDoesNotChooseAnotherProvidersModel(t *testing.T) {
	reg := tierRegistry(t, "")
	t.Setenv("MANVI_MODEL", "")
	if model, _, err := resolveModelFor(context.Background(), "xai", nil, reg); err == nil {
		t.Fatalf("an Anthropic tier model %q was sent to xai", model)
	}
}

func TestStartupRefusesAnUnknownTierModel(t *testing.T) {
	for _, bad := range []string{"claude-nope-9", "", "  ", "CLAUDE-OPUS-5-5"} {
		t.Run("["+bad+"]", func(t *testing.T) {
			t.Setenv("MANVI_LLM_TIER_LITE", bad)
			reg := tierRegistry(t, "")
			err := validateTiers(reg, anthropicTierCatalogue(), false)
			if err == nil || !strings.Contains(err.Error(), "lite") {
				t.Fatalf("got %v; want a refusal naming the lite tier", err)
			}
		})
	}
}

func TestStartupRefusesAnUnpricedTierModelOnlyWhileABudgetIsSet(t *testing.T) {
	reg := tierRegistry(t, "")
	cat := anthropicTierCatalogue()
	cat.price = func(model string) (budget.Prices, bool) {
		if model == "claude-haiku-5-5" {
			return budget.Prices{}, false
		}
		return anthropic.PriceOf(model)
	}
	if err := validateTiers(reg, cat, false); err != nil {
		t.Fatalf("with no cap a missing price is not a refusal: %v", err)
	}
	err := validateTiers(reg, cat, true)
	if err == nil || !strings.Contains(err.Error(), "basic") || !strings.Contains(err.Error(), "price") {
		t.Fatalf("got %v; want a refusal naming the basic tier and the missing price", err)
	}
}

func TestStartupRefusesAnEffortTheTierModelCannotHonour(t *testing.T) {
	cases := []struct {
		name, tier, model, effort string
	}{
		{"model does not reason", "BASIC", "claude-haiku-4-5", "low"},
		{"level not on the ladder", "LITE", "claude-sonnet-5-5", "ludicrous"},
		{"case matters", "MAIN", "claude-opus-5-5", "HIGH"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MANVI_LLM_TIER_"+tc.tier, tc.model)
			t.Setenv("MANVI_LLM_TIER_"+tc.tier+"_EFFORT", tc.effort)
			reg := tierRegistry(t, "")
			err := validateTiers(reg, anthropicTierCatalogue(), false)
			if err == nil || !strings.Contains(err.Error(), strings.ToLower(tc.tier)) {
				t.Fatalf("got %v; want a refusal naming the %s tier", err, tc.tier)
			}
		})
	}
	t.Run("unset effort on a non-reasoning model is fine", func(t *testing.T) {
		t.Setenv("MANVI_LLM_TIER_BASIC", "claude-haiku-4-5")
		reg := tierRegistry(t, "")
		if err := validateTiers(reg, anthropicTierCatalogue(), false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a supported effort is accepted", func(t *testing.T) {
		t.Setenv("MANVI_LLM_TIER_ADVISOR_EFFORT", "max")
		reg := tierRegistry(t, "")
		if err := validateTiers(reg, anthropicTierCatalogue(), false); err != nil {
			t.Fatal(err)
		}
	})
}

func TestStartupReportsEveryFailingTier(t *testing.T) {
	t.Setenv("MANVI_LLM_TIER_LITE", "nope-a")
	t.Setenv("MANVI_LLM_TIER_BASIC", "nope-b")
	err := validateTiers(tierRegistry(t, ""), anthropicTierCatalogue(), false)
	if err == nil || !strings.Contains(err.Error(), "lite") || !strings.Contains(err.Error(), "basic") {
		t.Fatalf("got %v; want both tiers named", err)
	}
}

func TestASessionCommandExitsNonZeroOnABadTierAndDiagnosticsStillRun(t *testing.T) {
	t.Setenv("MANVI_STATE_DIR", t.TempDir())
	t.Setenv("MANVI_LLM_TIER_LITE", "claude-nope-9")
	for _, args := range [][]string{{"run", "hello"}, {"tui"}, {"serve"}, {"watch"}} {
		var out, notes bytes.Buffer
		err := run(&out, &notes, args)
		if err == nil || !strings.Contains(err.Error(), "lite") {
			t.Errorf("%v: got %v; want a refusal naming the lite tier", args, err)
		}
	}
	for _, args := range [][]string{{"flags"}, {"doctor"}, {"providers"}} {
		var out, notes bytes.Buffer
		if err := run(&out, &notes, args); err != nil && strings.Contains(err.Error(), "tier") {
			t.Errorf("%v must still run so the operator can see what is wrong: %v", args, err)
		}
	}
	// The tier table is the diagnostic itself: it prints, and still exits non-zero.
	var out, notes bytes.Buffer
	if err := run(&out, &notes, []string{"tiers"}); err == nil || !strings.Contains(out.String(), "claude-nope-9") {
		t.Errorf("tiers: err %v, output %q", err, out.String())
	}
}

// postureConfigs is every way a repository can state a posture.
var postureConfigs = []string{
	"",
	"harness.posture: strict\n",
	"harness.posture: dev\n",
	"harness.posture: yolo\n",
}

func TestARepoLocalConfigCannotSetATierInAnyPosture(t *testing.T) {
	for _, posture := range postureConfigs {
		for _, tier := range agents.Tiers() {
			for _, key := range []string{tier.ModelKey(), tier.EffortKey()} {
				t.Run(strings.TrimSpace(posture)+"/"+key, func(t *testing.T) {
					reg := tierRegistry(t, posture+key+": claude-fable-5-1\n")
					got, err := reg.Lookup(key)
					if err != nil {
						t.Fatal(err)
					}
					if got.Origin == flags.OriginConfig {
						t.Fatalf("%s came from the repo-writable config: %+v", key, got)
					}
					def, _ := agents.DefaultTierProfile(tier)
					want := def.Model
					if key == tier.EffortKey() {
						want = def.Effort
					}
					if got.Raw != want {
						t.Fatalf("%s = %q, want the default %q", key, got.Raw, want)
					}
					var out bytes.Buffer
					if err := showFlags(&out, reg, nil); err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(out.String(), "refused") || !strings.Contains(out.String(), key) {
						t.Errorf("manvi flags does not show the refusal of %s:\n%s", key, out.String())
					}
				})
			}
		}
	}
}

func TestARepoLocalConfigStillLoadsItsOtherSettings(t *testing.T) {
	reg := tierRegistry(t, "llm.tier.lite: claude-fable-5-1\nllm.effort: high\n")
	got, err := reg.Lookup(flags.LLMEffort)
	if err != nil || got.Raw != "high" || got.Origin != flags.OriginConfig {
		t.Fatalf("a tier refusal must not discard the rest of the file: %+v, %v", got, err)
	}
}

func TestTierKeysComeFromTheOperatorOnly(t *testing.T) {
	t.Setenv("MANVI_LLM_TIER_LITE", "claude-opus-5-5")
	reg := tierRegistry(t, "llm.tier.lite: claude-fable-5-1\n")
	got, _ := reg.Lookup("llm.tier.lite")
	if got.Raw != "claude-opus-5-5" || got.Origin != flags.OriginEnv {
		t.Fatalf("env must apply: %+v", got)
	}
	if err := reg.Set(flags.Agent, "llm.tier.lite", "claude-fable-5-1"); err == nil {
		t.Fatal("a model tool call raised a tier")
	}
	if err := reg.Set(flags.Agent, "llm.tier.lite.effort", "max"); err == nil {
		t.Fatal("a model tool call raised a tier's effort")
	}
	if err := reg.Set(flags.Human, "llm.tier.lite", "claude-fable-5-1"); err == nil {
		t.Fatal("a tier moved after startup; spend settings are fixed for the life of the process")
	}
}

func TestTierReadsAreSafeUnderConcurrency(t *testing.T) {
	reg := tierRegistry(t, "llm.tier.lite: claude-fable-5-1\n")
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				if _, err := resolveTierTable(reg); err != nil {
					t.Error(err)
					return
				}
				_ = validateTiers(reg, anthropicTierCatalogue(), true)
			}
		}()
	}
	wg.Wait()
}

func TestTiersCommandPrintsTheTableWithSources(t *testing.T) {
	t.Setenv("MANVI_LLM_TIER_LITE", "claude-opus-5-5")
	t.Setenv("MANVI_LLM_TIER_LITE_EFFORT", "low")
	reg := tierRegistry(t, "llm.tier.basic: claude-fable-5-1\n")
	var out bytes.Buffer
	if err := showTiers(&out, reg, anthropicTierCatalogue(), false); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"advisor", "main", "heavy", "lite", "basic", "claude-fable-5-1", "claude-haiku-5-5", "default", "env", "refused"} {
		if !strings.Contains(text, want) {
			t.Errorf("table lacks %q:\n%s", want, text)
		}
	}
	var lite string
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "lite") {
			lite = line
		}
	}
	if !strings.Contains(lite, "claude-opus-5-5") || !strings.Contains(lite, "env") || !strings.Contains(lite, "low") {
		t.Errorf("lite row = %q", lite)
	}
}

func TestTiersCommandReportsAFailingTierWithoutHidingTheTable(t *testing.T) {
	t.Setenv("MANVI_LLM_TIER_LITE", "claude-nope-9")
	reg := tierRegistry(t, "")
	var out bytes.Buffer
	if err := showTiers(&out, reg, anthropicTierCatalogue(), false); err == nil {
		t.Fatal("a failing tier must make the command exit non-zero")
	}
	if !strings.Contains(out.String(), "claude-nope-9") {
		t.Errorf("the table was withheld:\n%s", out.String())
	}
}
