package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agent"
	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/llm/budget"
)

// The tier profile: which model, at which effort, each rung of the ladder runs.
//
// Tier settings decide spend, so they are operator-scope only. The environment
// and the process's own startup supply them; a repository's config.yaml does
// not, because that file is writable by whatever the harness is pointed at and
// a checked-in line raising the lite tier to the advisor's model would be a
// spend decision made by the repository. They are Startup flags for the other
// half of the same rule: nothing that runs inside the process — the model's
// tool calls included — can move one after boot.

// defineTierFlags declares the two settings of every tier, with the shipped
// profile as the default. Manvi defines them here rather than in DevCouncil's
// catalogue: the tiers name Anthropic models and are Manvi's own policy, and a
// Define before Seal needs no change to a module Manvi only pins.
func defineTierFlags(reg *flags.Registry) error {
	var defs []flags.Def
	for _, tier := range agents.Tiers() {
		def, _ := agents.DefaultTierProfile(tier)
		defs = append(defs,
			flags.Def{
				Key: tier.ModelKey(), Kind: flags.KindString, Default: def.Model, Mutable: flags.Startup,
				Description: fmt.Sprintf("Model the %s tier runs. Operator scope: set from the environment, never from .devcouncil/config.yaml. Must be in the provider catalogue, and priced while MANVI_ANTHROPIC_BUDGET_USD is set; checked at startup. The main tier also supplies the Anthropic session model when MANVI_MODEL is unset.", tier),
			},
			flags.Def{
				Key: tier.EffortKey(), Kind: flags.KindString, Default: def.Effort, Mutable: flags.Startup,
				Description: fmt.Sprintf("Reasoning effort the %s tier starts at. Empty omits the field. Operator scope. Must be a level the tier's model supports; checked at startup.", tier),
			})
	}
	return reg.Define(defs...)
}

// newOperatorRegistry is flags.NewHarnessRegistry with the tier settings
// defined and kept out of the config layer.
//
// It repeats that function's steps rather than calling it because the tier
// definitions have to exist before the file is read: an undefined llm.* key in
// the file is a startup error there, which would make the refusal impossible to
// show in 'manvi flags' — the one command an operator needs when the harness
// will not start. The file's tier keys are dropped, the rest of it loads, and
// repoTierKeys is what reports what was dropped.
func newOperatorRegistry(configPath string) (*flags.Registry, error) {
	reg := flags.New()
	if err := flags.DefineHarnessFlags(reg); err != nil {
		return nil, err
	}
	if err := defineTierFlags(reg); err != nil {
		return nil, err
	}
	values, err := readConfigValues(configPath)
	if err != nil {
		return nil, err
	}
	if retired := flags.RetiredConfig(values); len(retired) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "flags: %s sets settings this harness no longer has:\n", configPath)
		for _, r := range retired {
			fmt.Fprintf(&b, "  %s\t— %s\n", r.Key, r.Why)
		}
		b.WriteString("remove those lines")
		return nil, errors.New(b.String())
	}
	for key := range values {
		if isTierKey(key) {
			delete(values, key)
		}
	}
	if err := reg.LoadConfig(values); err != nil {
		return nil, fmt.Errorf("%s: %w", configPath, err)
	}
	if err := reg.LoadEnv(); err != nil {
		return nil, err
	}
	return reg, nil
}

func isTierKey(key string) bool { return strings.HasPrefix(key, agents.TierKeyPrefix) }

// readConfigValues parses the config file, treating a missing file or an empty
// path as no settings.
func readConfigValues(path string) (map[string]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("flags: reading %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	values, err := flags.ParseConfig(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return values, nil
}

// repoTierKeys lists the tier settings the config file at path tried to set,
// sorted. A file that cannot be read has none to report: the registry already
// failed on it, and this runs for display.
func repoTierKeys(path string) []string {
	values, err := readConfigValues(path)
	if err != nil {
		return nil
	}
	var keys []string
	for key := range values {
		if isTierKey(key) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// writeTierRefusals says, once per refused key, that the file was not obeyed.
func writeTierRefusals(w io.Writer, path string) {
	for _, key := range repoTierKeys(path) {
		fmt.Fprintf(w, "refused: %s is set in %s and ignored — tier settings decide spend and come from the operator only (%s)\n",
			key, path, flags.EnvKey(key))
	}
}

// tierRow is one resolved line of the tier table.
type tierRow struct {
	Tier         agents.Tier
	Model        string
	ModelOrigin  flags.Origin
	Effort       string
	EffortOrigin flags.Origin
}

// resolveTierTable reads every tier from the registry, weakest first.
func resolveTierTable(reg *flags.Registry) ([]tierRow, error) {
	var rows []tierRow
	for _, tier := range agents.Tiers() {
		model, err := reg.Lookup(tier.ModelKey())
		if err != nil {
			return nil, err
		}
		effort, err := reg.Lookup(tier.EffortKey())
		if err != nil {
			return nil, err
		}
		rows = append(rows, tierRow{
			Tier: tier, Model: strings.TrimSpace(model.Raw), ModelOrigin: model.Origin,
			Effort: strings.TrimSpace(effort.Raw), EffortOrigin: effort.Origin,
		})
	}
	return rows, nil
}

// tierCatalogue is what a tier is checked against. It is a pair of functions so
// a test can describe a catalogue the real one does not have, such as a model
// that is served but unpriced.
type tierCatalogue struct {
	provider   string
	capability func(model string) (llm.Capability, bool)
	price      func(model string) (budget.Prices, bool)
}

func anthropicTierCatalogue() tierCatalogue {
	return tierCatalogue{provider: anthropic.Name, capability: anthropic.Capability, price: anthropic.PriceOf}
}

// budgetCapSet reports whether the Anthropic spend cap is configured.
func budgetCapSet() bool { return strings.TrimSpace(os.Getenv(envAnthropicBudget)) != "" }

// validateTiers refuses a profile that would fail later, mid-turn, for a reason
// that was knowable at startup. Every failing tier is reported, each named.
//
// The price check runs only while a cap is set: the cap meter cannot account
// for a model it cannot price, so an unpriced tier would otherwise spend
// outside the limit. With no cap there is nothing to account against.
func validateTiers(reg *flags.Registry, cat tierCatalogue, capSet bool) error {
	rows, err := resolveTierTable(reg)
	if err != nil {
		return err
	}
	var failures []error
	for _, row := range rows {
		if err := validateTier(row, cat, capSet); err != nil {
			failures = append(failures, fmt.Errorf("%s tier (%s): %w", row.Tier, row.Tier.ModelKey(), err))
		}
	}
	return errors.Join(failures...)
}

func validateTier(row tierRow, cat tierCatalogue, capSet bool) error {
	if row.Model == "" {
		return errors.New("no model named; the tier cannot run")
	}
	capability, ok := cat.capability(row.Model)
	if !ok {
		return fmt.Errorf("model %q is not in the %s catalogue", row.Model, cat.provider)
	}
	if capSet {
		if _, ok := cat.price(row.Model); !ok {
			return fmt.Errorf("model %q has no price, so %s cannot account for it", row.Model, envAnthropicBudget)
		}
	}
	if err := agent.ValidateEffort(capability, row.Effort); err != nil {
		return fmt.Errorf("%s: %w", row.Tier.EffortKey(), err)
	}
	return nil
}

// showTiers prints the resolved table, with where each value came from, then
// anything the repository tried to set that was refused. The table is printed
// even when a tier fails, because the operator reading it is working out which
// one; the failure is the returned error.
func showTiers(out io.Writer, reg *flags.Registry, cat tierCatalogue, capSet bool) error {
	rows, err := resolveTierTable(reg)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "tiers\n\n  %-8s %-20s %-9s %-8s %s\n", "tier", "model", "source", "effort", "source")
	for _, row := range rows {
		effort := row.Effort
		if effort == "" {
			effort = "-"
		}
		fmt.Fprintf(out, "  %-8s %-20s %-9s %-8s %s\n", row.Tier, row.Model, row.ModelOrigin, effort, row.EffortOrigin)
	}
	fmt.Fprintln(out)
	writeTierRefusals(out, configPath())
	if capSet {
		fmt.Fprintf(out, "  %s is set: every tier's model must be priced\n", envAnthropicBudget)
	}
	return validateTiers(reg, cat, capSet)
}

// startsSession reports whether args begin a session that will spend: the face,
// a run, a watch, or a server. Diagnostics — flags, tiers, doctor, providers —
// are exempt so a broken profile can still be read, and so is help.
func startsSession(args []string) bool {
	if len(args) == 0 {
		return true
	}
	if len(args) > 1 && (args[1] == "-h" || args[1] == "--help") {
		return false
	}
	switch args[0] {
	case "tui", "run", "serve", "watch":
		return true
	}
	return false
}

// tierMainModel is the model the main tier names, for a session whose provider
// serves it.
func tierMainModel(reg *flags.Registry) (string, bool) {
	model, _, err := reg.String(agents.TierMain.ModelKey())
	model = strings.TrimSpace(model)
	return model, err == nil && model != ""
}
