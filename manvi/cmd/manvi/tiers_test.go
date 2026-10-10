package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/llm/budget"
	"github.com/bharathvbcr/Manvi/manvi/llm/local"
)

// tierRegistry is the registry a session builds, with no repository config and
// whatever the test exported.
func tierRegistry(t *testing.T) *flags.Registry {
	t.Helper()
	reg, err := newRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// fakeTierCatalogue serves a model set chosen by the test, because the shipped
// Anthropic catalogue prices every model it serves and so cannot exhibit the
// unpriced case at all.
type fakeTierCatalogue struct {
	models map[string]llm.Capability
	prices map[string]budget.Prices
}

func (f fakeTierCatalogue) Capability(model string) (llm.Capability, bool) {
	c, ok := f.models[model]
	return c, ok
}

func (f fakeTierCatalogue) PriceOf(model string) (budget.Prices, bool) {
	p, ok := f.prices[model]
	return p, ok
}

func TestTierDefaultsAreTheShippedProfile(t *testing.T) {
	table, err := resolveTiers(tierRegistry(t), anthropic.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !table.Active {
		t.Fatal("the tier profile is inactive on anthropic, the provider it is catalogued against")
	}
	want := agents.DefaultTierProfile()
	got := table.Profile()
	for _, tier := range agents.Tiers() {
		if got[tier] != want[tier] {
			t.Errorf("%s = %+v, want %+v", tier, got[tier], want[tier])
		}
	}
	for _, row := range table.Rows {
		if row.ModelFrom != "default" || row.EffortFrom != "default" {
			t.Errorf("%s: sources %q/%q, want default/default", row.Tier, row.ModelFrom, row.EffortFrom)
		}
	}
	if err := planTiersErr(table, anthropicTiers{}, false, ""); err != nil {
		t.Fatalf("the shipped profile does not validate against the shipped catalogue: %v", err)
	}
}

// TestTierMainSuppliesTheMainModel. Before tiers, an anthropic session with no
// MANVI_MODEL refused to start; the main tier is now the standing answer, and
// MANVI_MODEL — what an operator typed for this run — still beats it.
func TestTierMainSuppliesTheMainModel(t *testing.T) {
	t.Setenv("MANVI_MODEL", "")
	model, source, err := resolveModelFor(context.Background(), anthropic.Name, nil, tierRegistry(t))
	if err != nil || model != "claude-opus-5-5" || source != SourceTierMain {
		t.Fatalf("resolveModelFor = %q, %q, %v; want claude-opus-5-5 from %s", model, source, err, SourceTierMain)
	}

	t.Setenv(flags.EnvKey(tierModelKey(agents.TierMain)), "claude-sonnet-5-5")
	model, source, err = resolveModelFor(context.Background(), anthropic.Name, nil, tierRegistry(t))
	if err != nil || model != "claude-sonnet-5-5" || source != SourceTierMain {
		t.Fatalf("resolveModelFor = %q, %q, %v; want the operator's main tier", model, source, err)
	}

	t.Setenv("MANVI_MODEL", "claude-haiku-5-5")
	model, source, err = resolveModelFor(context.Background(), anthropic.Name, nil, tierRegistry(t))
	if err != nil || model != "claude-haiku-5-5" || source != SourceEnv {
		t.Fatalf("resolveModelFor = %q, %q, %v; want MANVI_MODEL to win", model, source, err)
	}
	table, err := resolveTiers(tierRegistry(t), anthropic.Name)
	if err != nil {
		t.Fatal(err)
	}
	if main := table.row(agents.TierMain); main.Model != "claude-haiku-5-5" || main.ModelKey != "MANVI_MODEL" {
		t.Fatalf("the table's main row = %+v; it must report the model the session will run", main)
	}
}

// TestTheMainEffortFollowsLLMEffortThenTheTier mirrors the model: llm.effort is
// the existing per-session setting and wins when set; the main tier's effort is
// what applies when it is not.
func TestTheMainEffortFollowsLLMEffortThenTheTier(t *testing.T) {
	t.Setenv(flags.EnvKey(tierEffortKey(agents.TierMain)), "medium")
	reg := tierRegistry(t)
	if effort, key, from, err := mainEffort(reg, anthropic.Name); err != nil || effort != "medium" ||
		key != tierEffortKey(agents.TierMain) || from != "env" {
		t.Fatalf("mainEffort = %q from %s (%s), %v; want medium from the main tier", effort, key, from, err)
	}
	if err := reg.Set(flags.Human, flags.LLMEffort, "high"); err != nil {
		t.Fatal(err)
	}
	if effort, key, _, err := mainEffort(reg, anthropic.Name); err != nil || effort != "high" || key != flags.LLMEffort {
		t.Fatalf("mainEffort = %q from %s, %v; want llm.effort to win", effort, key, err)
	}
	// Off anthropic there is no tier profile, so only llm.effort is read.
	reg = tierRegistry(t)
	if effort, key, _, err := mainEffort(reg, local.Name); err != nil || effort != "" || key != flags.LLMEffort {
		t.Fatalf("mainEffort on local = %q from %s, %v; want the tier ignored", effort, key, err)
	}
}

// The three startup refusals. Each names the tier and its setting, because the
// operator's next action is to change exactly that line.

func TestStartupRefusesATierModelOutsideTheCatalogue(t *testing.T) {
	t.Setenv(flags.EnvKey(tierModelKey(agents.TierLite)), "claude-sonnet-9")
	_, err := checkTiers(tierRegistry(t), anthropic.Name)
	requireTierRefusal(t, err, agents.TierLite, "claude-sonnet-9", "catalogue")
}

func TestStartupRefusesAnUnpricedTierWhileACapIsSet(t *testing.T) {
	reasoning := llm.Capability{Provider: "fake", SupportsReasoning: true, EffortLevels: []string{"low", "high"}}
	cat := fakeTierCatalogue{models: map[string]llm.Capability{}, prices: map[string]budget.Prices{}}
	for _, spec := range agents.DefaultTierProfile() {
		cat.models[spec.Model] = reasoning
		cat.prices[spec.Model] = budget.Prices{}
	}
	delete(cat.prices, "claude-sonnet-5-5")
	table, err := resolveTiers(tierRegistry(t), anthropic.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := planTiersErr(table, cat, false, ""); err != nil {
		t.Fatalf("an unpriced model was refused with no cap set: %v", err)
	}
	requireTierRefusal(t, planTiersErr(table, cat, true, ""), agents.TierLite, "claude-sonnet-5-5", "price")

	// And through the real seam: the cap is the variable meterAnthropic reads.
	t.Setenv(envAnthropicBudget, "50")
	if _, err := checkTiers(tierRegistry(t), anthropic.Name); err != nil {
		t.Fatalf("every shipped tier is priced, yet the capped check refused: %v", err)
	}
}

func TestStartupRefusesATierEffortTheModelCannotHonour(t *testing.T) {
	t.Run("an effort on a model that does not reason", func(t *testing.T) {
		t.Setenv(flags.EnvKey(tierModelKey(agents.TierBasic)), "claude-haiku-4-5")
		t.Setenv(flags.EnvKey(tierEffortKey(agents.TierBasic)), "high")
		_, err := checkTiers(tierRegistry(t), anthropic.Name)
		requireTierRefusal(t, err, agents.TierBasic, "llm.tier.basic.effort", "reasoning")
	})
	t.Run("an effort the model does not serve", func(t *testing.T) {
		t.Setenv(flags.EnvKey(tierEffortKey(agents.TierAdvisor)), "extreme")
		_, err := checkTiers(tierRegistry(t), anthropic.Name)
		requireTierRefusal(t, err, agents.TierAdvisor, "llm.tier.advisor.effort", "extreme")
	})
	t.Run("a non-reasoning tier does not inherit the session ceiling", func(t *testing.T) {
		t.Setenv(flags.EnvKey(tierModelKey(agents.TierBasic)), "claude-haiku-4-5")
		t.Setenv(flags.EnvKey(flags.LLMEffort), "low")
		t.Setenv(flags.EnvKey(flags.LLMEffortCeiling), "high")
		table, err := checkTiers(tierRegistry(t), anthropic.Name)
		if err != nil {
			t.Fatalf("a tier on a model that cannot reason was refused for the parent's ceiling: %v", err)
		}
		if basic := table.row(agents.TierBasic); basic.Ceiling != "" {
			t.Fatalf("basic inherited ceiling %q on a model that cannot climb to it", basic.Ceiling)
		}
		if main := table.row(agents.TierMain); main.Ceiling != "high" {
			t.Fatalf("main ceiling = %q, want high", main.Ceiling)
		}
	})
}

// Every failing tier is reported at once, so one startup refusal does not turn
// into one per tier. (Adapted from hx/tier-profile's TestStartupReportsEveryFailingTier.)
func TestStartupReportsEveryFailingTier(t *testing.T) {
	t.Setenv(flags.EnvKey(tierModelKey(agents.TierLite)), "nope-a")
	t.Setenv(flags.EnvKey(tierModelKey(agents.TierBasic)), "nope-b")
	_, err := checkTiers(tierRegistry(t), anthropic.Name)
	if err == nil || !strings.Contains(err.Error(), "tier lite") || !strings.Contains(err.Error(), "tier basic") {
		t.Fatalf("got %v; want both tiers named", err)
	}
}

// Resolving and checking the profile is read-only and safe to do from every
// session goroutine at once, as the TUI does at each attach. Run under -race.
// (Adapted from hx/tier-profile's TestTierReadsAreSafeUnderConcurrency.)
func TestTierReadsAreSafeUnderConcurrency(t *testing.T) {
	reg := tierRegistry(t)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				if _, err := checkTiers(reg, anthropic.Name); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestTheHeadlessRunRefusesABadTierBeforeContactingAnyone is the end-to-end
// shape of the refusals: a non-zero exit, before any request, naming the tier.
func TestTheHeadlessRunRefusesABadTierBeforeContactingAnyone(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-fixture-tiers")
	// Nothing listens here: reaching it would fail with a transport error,
	// not the refusal this test asks for.
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("MANVI_MODEL", "")
	t.Setenv(flags.EnvKey(tierModelKey(agents.TierHeavy)), "claude-opus-9")

	var out, notes bytes.Buffer
	err := run(&out, &notes, []string{"run", "-p", "hello"})
	requireTierRefusal(t, err, agents.TierHeavy, "claude-opus-9", "catalogue")
	if status := statusFor(err, &notes); status == 0 {
		t.Fatal("a refused tier exited 0")
	}
}

// TestARepositoryCannotSetATier is the security property. Tier keys decide
// spend, and .devcouncil/config.yaml is a file the repository — and an agent
// working in it under a relaxed posture — can write. So the file is refused,
// whichever posture is in force and however the posture was chosen, and the
// refusal is what `manvi flags` prints.
func TestARepositoryCannotSetATier(t *testing.T) {
	for _, tc := range []struct {
		name    string
		posture string
		args    []string
		yaml    string
	}{
		{name: "dev", posture: "dev", yaml: "llm.tier.advisor: claude-haiku-5-5\n"},
		{name: "strict", posture: "strict", yaml: "llm.tier.lite.effort: max\n"},
		{name: "yolo from the file itself", yaml: "harness.posture: yolo\nllm:\n  tier:\n    main: claude-fable-5-1\n"},
		{name: "yolo from the command line", args: []string{"--yolo"}, yaml: "llm.tier.basic: claude-opus-5-5\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			if tc.posture != "" {
				t.Setenv(flags.EnvKey(flags.HarnessPosture), tc.posture)
			}
			if err := os.MkdirAll(filepath.Join(dir, ".devcouncil"), 0o750); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(dir, ".devcouncil"), "config.yaml", tc.yaml)

			for _, cmd := range [][]string{{"flags"}, {"flags", "--all"}} {
				var out, notes bytes.Buffer
				err := run(&out, &notes, append(append([]string{}, tc.args...), cmd...))
				if err == nil {
					t.Fatalf("manvi %v accepted a repository config that sets a tier:\n%s", cmd, out.String())
				}
				for _, want := range []string{"llm.tier.", "config.yaml", "operator"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("manvi %v refusal does not mention %q: %v", cmd, want, err)
					}
				}
			}
		})
	}
}

// TestAModelCannotRaiseATier. The agent's authority is the one a tool call
// would carry; tier keys are human-only, so the registry refuses it.
func TestAModelCannotRaiseATier(t *testing.T) {
	reg := tierRegistry(t)
	for _, tier := range agents.Tiers() {
		for _, key := range []string{tierModelKey(tier), tierEffortKey(tier)} {
			def, ok := reg.Def(key)
			if !ok {
				t.Fatalf("%s is not defined", key)
			}
			if def.Mutable != flags.HumanOnly {
				t.Errorf("%s mutability = %q, want human", key, def.Mutable)
			}
			if err := reg.Set(flags.Agent, key, "max"); err == nil {
				t.Errorf("an agent moved %s", key)
			}
		}
	}
}

func TestTheUserConfigSuppliesTiersAndTheEnvironmentBeatsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv(envUserConfig, path)
	writeFile(t, filepath.Dir(path), "config.yaml",
		"llm.tier.lite: claude-haiku-5-5\nllm.tier.lite.effort: low\nllm.tier.heavy: claude-sonnet-5-5\n")

	table, err := resolveTiers(tierRegistry(t), anthropic.Name)
	if err != nil {
		t.Fatal(err)
	}
	lite := table.row(agents.TierLite)
	if lite.Model != "claude-haiku-5-5" || lite.Effort != "low" || !strings.Contains(lite.ModelFrom, path) {
		t.Fatalf("lite = %+v, want the user config's values, sourced to %s", lite, path)
	}

	t.Setenv(flags.EnvKey(tierModelKey(agents.TierHeavy)), "claude-opus-5")
	table, err = resolveTiers(tierRegistry(t), anthropic.Name)
	if err != nil {
		t.Fatal(err)
	}
	if heavy := table.row(agents.TierHeavy); heavy.Model != "claude-opus-5" || heavy.ModelFrom != "env" {
		t.Fatalf("heavy = %+v, want the environment to beat the user config", heavy)
	}
}

// TestAUserConfigInsideTheRepositoryIsRefused. The user config is trusted
// because a clone cannot write it; pointed back inside the tree, by the
// variable or by a symlink, that stops being true.
func TestAUserConfigInsideTheRepositoryIsRefused(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("MANVI_PROJECT_ROOT", root)
	writeFile(t, root, "tiers.yaml", "llm.tier.advisor: claude-haiku-5-5\n")

	t.Setenv(envUserConfig, filepath.Join(root, "tiers.yaml"))
	if _, err := newRegistry(""); err == nil || !strings.Contains(err.Error(), "inside the repository") {
		t.Fatalf("a user config inside the repository was read: %v", err)
	}

	outside := t.TempDir()
	link := filepath.Join(outside, "config.yaml")
	if err := os.Symlink(filepath.Join(root, "tiers.yaml"), link); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envUserConfig, link)
	if _, err := newRegistry(""); err == nil || !strings.Contains(err.Error(), "inside the repository") {
		t.Fatalf("a user config symlinked into the repository was read: %v", err)
	}
}

// TestTheUserConfigCarriesOnlyTierSettings. It is read ahead of nothing else,
// so a gate setting written there would land in the same config layer as the
// repository's with nothing to say which file it came from.
func TestTheUserConfigCarriesOnlyTierSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv(envUserConfig, path)
	writeFile(t, filepath.Dir(path), "config.yaml", "policy.file.mode: off\n")
	if _, err := newRegistry(""); err == nil || !strings.Contains(err.Error(), "policy.file.mode") {
		t.Fatalf("the user config set a gate: %v", err)
	}

	t.Setenv(envUserConfig, "relative/config.yaml")
	if _, err := newRegistry(""); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("a relative user config path was accepted: %v", err)
	}
}

// TestNoUserConfigDirectoryMeansNoUserConfig. A process with no home has no
// user config to read; that is the file being absent, not a reason to refuse
// every command.
func TestNoUserConfigDirectoryMeansNoUserConfig(t *testing.T) {
	t.Setenv(envUserConfig, "")
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("AppData", "")
	if _, err := userConfigPath(); !errors.Is(err, errNoUserConfigDir) {
		t.Fatalf("userConfigPath with no HOME = %v; want errNoUserConfigDir", err)
	}
	reg, err := newRegistry("")
	if err != nil {
		t.Fatalf("a process with no user config directory was refused: %v", err)
	}
	if v, origin, err := reg.String(tierModelKey(agents.TierMain)); err != nil || origin != flags.OriginDefault ||
		v != agents.DefaultTierProfile()[agents.TierMain].Model {
		t.Fatalf("main tier = %q (%s), %v; want the default", v, origin, err)
	}
}

// TestTiersAreInactiveOffAnthropic. The profile is catalogued against one
// provider; on another the defaults are not refused — that would break every
// local session — but they are not applied, and the table says so.
func TestTiersAreInactiveOffAnthropic(t *testing.T) {
	table, err := checkTiers(tierRegistry(t), local.Name)
	if err != nil {
		t.Fatalf("the default profile was refused on local: %v", err)
	}
	if table.Active || table.Profile() != nil {
		t.Fatalf("table = %+v, want an inactive profile off anthropic", table)
	}
	var out bytes.Buffer
	if err := writeTierTable(&out, table); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "inactive") {
		t.Fatalf("the table does not say the profile is inactive:\n%s", out.String())
	}
}

func TestTheTierTableNamesTheSourceOfEachValue(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("MANVI_MODEL", "")
	t.Setenv(flags.EnvKey(tierEffortKey(agents.TierAdvisor)), "max")
	var out, notes bytes.Buffer
	if err := run(&out, &notes, []string{"tiers"}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, tier := range agents.Tiers() {
		line := findFlagLine(t, text, tier.String())
		if !strings.Contains(line, tierModelKey(tier)) {
			t.Errorf("%s row does not name its setting: %q", tier, line)
		}
	}
	if line := findFlagLine(t, text, "advisor"); !strings.Contains(line, "claude-fable-5-1") ||
		!strings.Contains(line, "max") || !strings.Contains(line, "env") || !strings.Contains(line, "default") {
		t.Errorf("advisor row = %q, want its model from the default and its effort from env", line)
	}

	t.Setenv(flags.EnvKey(tierModelKey(agents.TierLite)), "claude-sonnet-9")
	out.Reset()
	err := run(&out, &notes, []string{"tiers"})
	requireTierRefusal(t, err, agents.TierLite, "claude-sonnet-9")
	if !strings.Contains(findFlagLine(t, out.String(), "lite"), "claude-sonnet-9") {
		t.Errorf("the table was not printed beside the refusal:\n%s", out.String())
	}
}

func planTiersErr(table tierTable, cat tierCatalogue, capped bool, ceiling string) error {
	_, err := planTiers(table, cat, capped, ceiling)
	return err
}

func requireTierRefusal(t *testing.T, err error, tier agents.Tier, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("tier %s was accepted", tier)
	}
	for _, want := range append([]string{"tier " + tier.String()}, wants...) {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
}
