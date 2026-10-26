package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agent"
	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/llm/budget"
	"github.com/bharathvbcr/Manvi/manvi/mcp"
)

// This file owns the tier profile: which model serves each agents.Tier, at what
// effort, where each of those values came from, and whether the session's
// provider can honour them.
//
// The keys are defined here, before Seal, rather than in DevCouncil's
// catalogue, for two reasons. Their scope rule is this harness's alone — no
// other consumer of that catalogue picks models by tier — and it is a rule the
// catalogue cannot express: every key there may be set from the repository's
// config.yaml, and a tier may not. Putting them there would mean teaching the
// shared registry a second kind of config layer for one consumer.
//
// Tier keys decide spend, so they come from operator scope only: the user
// config below, MANVI_LLM_TIER_*, and `flags set` on human authority. The
// repository's .devcouncil/config.yaml is a file a clone ships and an agent can
// write under a relaxed posture, so a tier key in it is refused outright rather
// than ignored — an ignored line is a setting that looks applied.

// tierPrefix is the namespace every tier key lives under.
const tierPrefix = "llm.tier."

// tierModelKey is the setting naming the model that serves a tier.
func tierModelKey(t agents.Tier) string { return tierPrefix + t.String() }

// tierEffortKey is the setting naming the effort requests at a tier carry.
func tierEffortKey(t agents.Tier) string { return tierModelKey(t) + ".effort" }

// isTierKey reports whether a config key belongs to the tier namespace. The
// bare "llm.tier" counts: a scalar there is a typo for one of the keys under it,
// and refusing it as a tier key says which rule it broke.
func isTierKey(key string) bool {
	return key == strings.TrimSuffix(tierPrefix, ".") || strings.HasPrefix(key, tierPrefix)
}

// tierFlagDefs declares two settings per tier. Both are human-only: a model
// tool call carries agent authority, and the registry refuses it.
func tierFlagDefs() []flags.Def {
	defaults := agents.DefaultTierProfile()
	var defs []flags.Def
	for _, t := range agents.Tiers() {
		defs = append(defs,
			flags.Def{
				Key: tierModelKey(t), Kind: flags.KindString, Default: defaults[t].Model, Mutable: flags.HumanOnly,
				Description: fmt.Sprintf("Model serving the %s tier, a bare id on the session's provider. "+
					"Operator scope: read from the user config, the environment, or flags set — "+
					"never from the repository's config.yaml.", t),
			},
			flags.Def{
				Key: tierEffortKey(t), Kind: flags.KindString, Default: defaults[t].Effort, Mutable: flags.HumanOnly,
				Description: fmt.Sprintf("Reasoning effort for the %s tier. Empty omits the field. "+
					"Checked against the tier's model at startup. Operator scope, as the model is.", t),
			},
		)
	}
	return defs
}

// newRegistry is the harness's settings registry: DevCouncil's catalogue, the
// tier keys, the repository config, the user config and the environment.
//
// The order is what makes the scope rule hold. The repository file is checked
// for tier keys before anything loads it, so its refusal names the rule rather
// than reading as a typo. The tier keys are defined after it has loaded, so even
// a path that skipped that check would find them unknown there. The environment
// is read a second time once the tier keys exist, because LoadEnv reads only the
// keys defined when it runs.
func newRegistry(repoConfig string) (*flags.Registry, error) {
	if err := refuseRepositoryTiers(repoConfig); err != nil {
		return nil, err
	}
	if err := refuseRepositoryHarnessSpec(repoConfig); err != nil {
		return nil, err
	}
	reg, err := flags.NewHarnessRegistry(repoConfig)
	if err != nil {
		return nil, err
	}
	if err := reg.Define(tierFlagDefs()...); err != nil {
		return nil, err
	}
	if err := reg.Define(harnessFlagDefs()...); err != nil {
		return nil, err
	}
	path, err := userConfigPath()
	if errors.Is(err, errNoUserConfigDir) {
		// No home to hold one, and none named: there is no user config to read,
		// exactly as when the file does not exist. Refusing here would stop
		// 'manvi flags' and 'manvi doctor' in every HOME-less container over a
		// layer that only ever narrows what the repository decides.
		path, err = "", nil
	}
	if err != nil {
		return nil, err
	}
	if err := loadUserConfig(reg, path, projectRoot()); err != nil {
		return nil, err
	}
	if err := reg.LoadEnv(); err != nil {
		return nil, err
	}
	return reg, nil
}

// readConfigFile parses a settings file, reporting a missing one as an empty
// layer. The layer, not the flat map: its line numbers and block lists are what
// let Registry.LoadLayer refuse a list written under a scalar setting, which
// LoadConfig closes as an empty section.
func readConfigFile(path string) (flags.ConfigLayer, error) {
	if strings.TrimSpace(path) == "" {
		return flags.ConfigLayer{}, nil
	}
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		if os.IsNotExist(err) {
			return flags.ConfigLayer{}, nil
		}
		return flags.ConfigLayer{}, fmt.Errorf("reading %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	layer, err := flags.ParseConfigLayer(f)
	if err != nil {
		return flags.ConfigLayer{}, fmt.Errorf("%s: %w", path, err)
	}
	return layer, nil
}

// refuseRepositoryTiers refuses a repository config that sets any tier key.
//
// A file that cannot be read or parsed is reported from here, with the path and
// the parser's own account of the line, which is what NewHarnessRegistry would
// have said about it a moment later.
func refuseRepositoryTiers(path string) error {
	layer, err := readConfigFile(path)
	if err != nil {
		return err
	}
	var set []string
	for key := range layer.Values {
		if isTierKey(key) {
			set = append(set, key)
		}
	}
	if len(set) == 0 {
		return nil
	}
	slices.Sort(set)
	return fmt.Errorf("%s sets %s, and tier settings are operator scope: they decide which model — "+
		"and so which price — every agent runs at, and this file is one a repository ships and an agent "+
		"can write. Remove those lines; set a tier with %s, in %s, or with 'manvi flags set'",
		path, strings.Join(set, ", "), flags.EnvKey(set[0]), userConfigHint())
}

// envUserConfig moves the user config, as MANVI_ANTHROPIC_BUDGET_LEDGER moves
// the ledger beside it.
const envUserConfig = "MANVI_USER_CONFIG"

// errNoUserConfigDir is userConfigPath's answer when the platform has no user
// config directory and MANVI_USER_CONFIG names no file.
var errNoUserConfigDir = errors.New("no user config directory")

// userConfigPath is the operator's own settings file: <user config dir>/manvi/
// config.yaml, beside the budget ledger and the MCP authorization file, or the
// absolute path MANVI_USER_CONFIG names.
func userConfigPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv(envUserConfig)); p != "" {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("%s=%q must be an absolute path", envUserConfig, p)
		}
		return filepath.Clean(p), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("%w: %w (set %s to an absolute path)", errNoUserConfigDir, err, envUserConfig)
	}
	return filepath.Join(dir, "manvi", flags.DefaultConfigFile), nil
}

// userConfigHint names the user config for a refusal, without failing it.
func userConfigHint() string {
	if path, err := userConfigPath(); err == nil {
		return path
	}
	return "the user config (" + envUserConfig + ")"
}

// loadUserConfig applies the user config to the config layer.
//
// It carries tier settings and nothing else. It shares the config layer with
// the repository file, and for tier keys that is unambiguous — the repository
// can never supply one — but for any other key an origin of "config" would no
// longer say which of the two files set it. Every other key is refused, naming
// it, rather than dropped.
//
// A file inside the repository is refused before it is read: it is trusted
// only because a clone cannot write it, and a path or symlink that leads back
// into the tree would let the repository choose its own tier.
func loadUserConfig(reg *flags.Registry, path, root string) error {
	if path == "" {
		return nil
	}
	if inside, err := mcp.PathIsInside(root, path); err != nil {
		return fmt.Errorf("could not tell whether the user config %s lies inside %s: %w", path, root, err)
	} else if inside {
		return fmt.Errorf("the user config %s lies inside the repository %s; a setting a checked-out tree "+
			"can write is not operator scope, so it was not read — move it, or point %s elsewhere",
			path, root, envUserConfig)
	}
	layer, err := readConfigFile(path)
	if err != nil {
		return fmt.Errorf("user config: %w", err)
	}
	var other []string
	for key := range layer.Values {
		if !isTierKey(key) {
			other = append(other, key)
		}
	}
	if len(other) > 0 {
		slices.Sort(other)
		return fmt.Errorf("user config %s sets %s; it carries %s* settings only — put the rest in the "+
			"repository's config.yaml or the environment", path, strings.Join(other, ", "), tierPrefix)
	}
	if err := reg.LoadLayer(layer); err != nil {
		return fmt.Errorf("user config %s: %w", path, err)
	}
	return nil
}

// tierRow is one tier as the session will run it, with where each value came
// from. The keys are what a refusal names, and for the main tier they can be
// MANVI_MODEL or llm.effort rather than a tier key, because those win.
type tierRow struct {
	Tier                          agents.Tier
	Model, ModelKey, ModelFrom    string
	Effort, EffortKey, EffortFrom string
	// Ceiling is the session's llm.effort.max as this tier inherits it, set
	// only once the row has been checked: empty where the tier's model cannot
	// climb to it.
	Ceiling string
}

// tierTable is the resolved profile for one session provider.
type tierTable struct {
	Provider string
	// Active is false off anthropic. The profile is catalogued against that
	// provider only — a session's sub-agents can be placed only on its own
	// provider — so elsewhere the values are reported and not applied.
	Active bool
	Rows   []tierRow
}

// row returns the row for a tier; the table always has one per tier.
func (t tierTable) row(tier agents.Tier) tierRow {
	for _, r := range t.Rows {
		if r.Tier == tier {
			return r
		}
	}
	return tierRow{Tier: tier}
}

// Profile is the table as the router reads it, or nil when it does not apply.
func (t tierTable) Profile() agents.TierProfile {
	if !t.Active {
		return nil
	}
	profile := agents.TierProfile{}
	for _, r := range t.Rows {
		profile[r.Tier] = agents.TierSpec{Model: r.Model, Effort: r.Effort}
	}
	return profile
}

// describeOrigin says where a tier value came from, naming the file when the
// config layer supplied it: for a tier key that can only be the user config.
func describeOrigin(origin flags.Origin) string {
	if origin == flags.OriginConfig {
		return "user config " + userConfigHint()
	}
	return string(origin)
}

// tierSetting reads one tier key with its described origin.
func tierSetting(reg *flags.Registry, key string) (string, string, error) {
	v, origin, err := reg.String(key)
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(v), describeOrigin(origin), nil
}

// mainModel is the model the main agent runs on an anthropic session:
// MANVI_MODEL, which is what an operator typed for this run, and otherwise the
// main tier. key is the setting that supplied it, for a refusal to name.
func mainModel(reg *flags.Registry) (model, key, from string, err error) {
	if m := strings.TrimSpace(os.Getenv("MANVI_MODEL")); m != "" {
		return m, "MANVI_MODEL", string(flags.OriginEnv), nil
	}
	key = tierModelKey(agents.TierMain)
	model, from, err = tierSetting(reg, key)
	return model, key, from, err
}

// mainEffort is the effort the main agent's requests carry: llm.effort when it
// is set, as MANVI_MODEL beats the main tier's model, and otherwise the main
// tier's effort. Off anthropic there is no tier profile, so it is llm.effort.
// key is the setting that supplied it.
//
// llm.effort's origin is reported as the layer and nothing more: unlike a tier
// key, it may come from the repository's config.yaml, so "config" there does
// not mean the user config.
func mainEffort(reg *flags.Registry, provider string) (effort, key, from string, err error) {
	v, origin, err := reg.String(flags.LLMEffort)
	if err != nil {
		return "", "", "", err
	}
	if v = strings.TrimSpace(v); v != "" || provider != anthropic.Name {
		return v, flags.LLMEffort, string(origin), nil
	}
	key = tierEffortKey(agents.TierMain)
	effort, from, err = tierSetting(reg, key)
	return effort, key, from, err
}

// resolveTiers reads the profile for a session on provider, without checking
// it against anything.
func resolveTiers(reg *flags.Registry, provider string) (tierTable, error) {
	table := tierTable{Provider: provider, Active: provider == anthropic.Name}
	for _, t := range agents.Tiers() {
		row := tierRow{Tier: t, ModelKey: tierModelKey(t), EffortKey: tierEffortKey(t)}
		var err error
		if t == agents.TierMain && table.Active {
			row.Model, row.ModelKey, row.ModelFrom, err = mainModel(reg)
			if err == nil {
				row.Effort, row.EffortKey, row.EffortFrom, err = mainEffort(reg, provider)
			}
		} else {
			row.Model, row.ModelFrom, err = tierSetting(reg, row.ModelKey)
			if err == nil {
				row.Effort, row.EffortFrom, err = tierSetting(reg, row.EffortKey)
			}
		}
		if err != nil {
			return tierTable{}, err
		}
		table.Rows = append(table.Rows, row)
	}
	return table, nil
}

// tierCatalogue is what a profile is checked against: whether the provider
// serves a model and what it can do, and what it costs.
type tierCatalogue interface {
	Capability(model string) (llm.Capability, bool)
	PriceOf(model string) (budget.Prices, bool)
}

// anthropicTiers is the shipped Anthropic catalogue and price table.
type anthropicTiers struct{}

func (anthropicTiers) Capability(model string) (llm.Capability, bool) {
	return anthropic.Capability(model)
}
func (anthropicTiers) PriceOf(model string) (budget.Prices, bool) { return anthropic.PriceOf(model) }

// planTiers checks every row of an active table and fills in the ceiling each
// tier inherits. Every refusal is reported, not just the first, and each names
// the tier and the setting to change.
//
// capped is whether a spend cap is in force. A tier the cap cannot price is
// refused then, not when its first request is: budget.Provider refuses an
// unpriced request, and a profile that routes work to a model it can never
// meter is a profile that fails mid-fan-out.
func planTiers(table tierTable, cat tierCatalogue, capped bool, ceiling string) (tierTable, error) {
	if !table.Active {
		return table, nil
	}
	var errs []error
	rows := append([]tierRow(nil), table.Rows...)
	for i, r := range rows {
		capability, ok := cat.Capability(r.Model)
		if !ok {
			errs = append(errs, fmt.Errorf("tier %s: %s=%q (%s) is not in the %s catalogue",
				r.Tier, r.ModelKey, r.Model, r.ModelFrom, table.Provider))
			continue
		}
		if _, priced := cat.PriceOf(r.Model); capped && !priced {
			errs = append(errs, fmt.Errorf("tier %s: %s=%q (%s) has no price, and %s caps spend: "+
				"a request a capped run cannot price is refused", r.Tier, r.ModelKey, r.Model, r.ModelFrom,
				envAnthropicBudget))
		}
		plan, err := agent.PlanTierEffort(capability, r.Effort, ceiling)
		if err != nil {
			errs = append(errs, fmt.Errorf("tier %s: %s=%q (%s) on %s: %w",
				r.Tier, r.EffortKey, r.Effort, r.EffortFrom, r.Model, err))
			continue
		}
		if plan.Escalates() {
			rows[i].Ceiling = ceiling
		}
	}
	if len(errs) > 0 {
		return table, errors.Join(errs...)
	}
	table.Rows = rows
	return table, nil
}

// checkTiers resolves and checks the profile a session on provider will run,
// against the shipped catalogue, the spend cap meterAnthropic reads, and the
// session's effort ceiling. A session refuses to start on its error.
func checkTiers(reg *flags.Registry, provider string) (tierTable, error) {
	table, err := resolveTiers(reg, provider)
	if err != nil {
		return tierTable{}, err
	}
	ceiling, _, err := reg.String(flags.LLMEffortCeiling)
	if err != nil {
		return tierTable{}, err
	}
	capped := strings.TrimSpace(os.Getenv(envAnthropicBudget)) != ""
	return planTiers(table, anthropicTiers{}, capped, strings.TrimSpace(ceiling))
}

// checkDefaultTiers checks the profile for the provider the registry defaults
// to, for a host that must refuse a profile its sessions could not run before
// it builds any session: checkTiers, with the provider read from the registry.
func checkDefaultTiers(reg *flags.Registry) error {
	provider, _, err := reg.String(flags.LLMDefaultProvider)
	if err != nil {
		return err
	}
	_, err = checkTiers(reg, provider)
	return err
}

// inactiveOverrides lists tier values an operator set that this session will
// not apply, so a run off anthropic can say so instead of looking configured.
func (t tierTable) inactiveOverrides() []string {
	if t.Active {
		return nil
	}
	var set []string
	for _, r := range t.Rows {
		if r.ModelFrom != string(flags.OriginDefault) {
			set = append(set, r.ModelKey)
		}
		if r.EffortFrom != string(flags.OriginDefault) {
			set = append(set, r.EffortKey)
		}
	}
	return set
}

// --- tiers ---

// showTiers prints the resolved profile and exits non-zero on the error a
// session would refuse to start with, printing the table either way: the table
// is what an operator reads to find the line to change, and the refusal says
// which line that is.
func showTiers(out io.Writer, reg *flags.Registry) error {
	provider, _, err := reg.String(flags.LLMDefaultProvider)
	if err != nil {
		return err
	}
	table, checkErr := checkTiers(reg, provider)
	if checkErr != nil && table.Rows == nil {
		return checkErr
	}
	if err := writeTierTable(out, table); err != nil {
		return err
	}
	return checkErr
}

// writeTierTable prints one row per tier. A refusal is not repeated here: the
// command returns it, and the dispatcher prints it once, on stderr.
func writeTierTable(out io.Writer, table tierTable) error {
	var b strings.Builder
	if table.Active {
		fmt.Fprintf(&b, "tier profile on %s\n\n", table.Provider)
	} else {
		fmt.Fprintf(&b, "tier profile — inactive on %s: it is catalogued against %s only, so these values "+
			"are reported and not applied\n\n", table.Provider, anthropic.Name)
	}
	for _, r := range table.Rows {
		effort, ceiling := r.Effort, r.Ceiling
		if effort == "" {
			effort = "-"
		}
		if ceiling == "" {
			ceiling = "-"
		}
		fmt.Fprintf(&b, "  %-8s %-18s from %s (%s) · effort %s from %s (%s) · ceiling %s\n",
			r.Tier, r.Model, r.ModelKey, r.ModelFrom, effort, r.EffortKey, r.EffortFrom, ceiling)
	}
	if table.Active {
		fmt.Fprintf(&b, "\nMANVI_MODEL beats the main tier's model and %s its effort. A tier is set with %s*, "+
			"in %s, or with 'manvi flags set'; never from the repository's config.yaml.\n",
			flags.LLMEffort, flags.EnvKey(tierPrefix), userConfigHint())
	}
	_, err := io.WriteString(out, b.String())
	return err
}
