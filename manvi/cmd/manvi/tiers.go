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
	"github.com/bharathvbcr/Manvi/manvi/harness"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/llm/budget"
	"github.com/bharathvbcr/Manvi/manvi/llm/gemini"
	"github.com/bharathvbcr/Manvi/manvi/llm/xai"
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

// tierFallbackKey is the setting naming where a child on a tier re-runs, once,
// when its provider fails with a service condition the transport's own
// retries could not outlast — rate limited, overloaded, unavailable. It must
// name its provider: a fallback exists to leave the provider that failed.
func tierFallbackKey(t agents.Tier) string { return tierModelKey(t) + ".fallback" }

// rosterKey is the setting naming the operator's team: the members a
// dispatching model may choose by name. It lives under the tier prefix because
// it is the same kind of decision — which model, and so which price, a child
// runs at — and so it inherits every scope rule a tier key has: refused from
// the repository's config.yaml, accepted from the user config, the
// environment and 'manvi flags set'. See agents.ParseRoster for the grammar.
const rosterKey = tierPrefix + "roster"

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
				Description: fmt.Sprintf("Model serving the %s tier: a bare id, which runs on the session's provider, "+
					"or provider/model, which runs on that provider. "+
					"Operator scope: read from the user config, the environment, or flags set — "+
					"never from the repository's config.yaml.", t),
			},
			flags.Def{
				Key: tierFallbackKey(t), Kind: flags.KindString, Default: "", Mutable: flags.HumanOnly,
				Description: fmt.Sprintf("Where a child on the %s tier re-runs, once, when its provider is rate limited, "+
					"overloaded or unavailable after retries: provider/model. Empty means no fallback. Operator scope.", t),
			},
			flags.Def{
				Key: tierEffortKey(t), Kind: flags.KindString, Default: defaults[t].Effort, Mutable: flags.HumanOnly,
				Description: fmt.Sprintf("Reasoning effort for the %s tier. Empty omits the field. "+
					"Checked against the tier's model at startup. Operator scope, as the model is.", t),
			},
		)
	}
	defs = append(defs, flags.Def{
		Key: rosterKey, Kind: flags.KindString, Default: "", Mutable: flags.HumanOnly,
		Description: "The team a dispatching model may choose from by name: comma-separated " +
			"name=tier:provider/model[@effort] entries, for example " +
			"fable=advisor:anthropic/claude-fable-5-2,scout=basic:gemini/gemini-4-hydrogen. " +
			"A member is admitted only at or below a role's ceiling, by its own tier. Operator scope, as a tier is.",
	})
	return defs
}

// newRegistry is the harness's settings registry: DevCouncil's catalogue, the
// tier keys, the repository config, the user config and the environment.
//
// The pre-read refusal above is for its clear message; it reads the file with
// its own open, and the registry opens it again, so a writer swapping the file
// between the two could show them different bytes. The check that holds is the
// one on what the registry loaded, refuseLoadedRepositoryEndpoints. Tier and
// spec keys need no such second check: they are not defined when the repository
// file loads, so the registry rejects them there as unknown keys.
//
// The order is what makes the scope rule hold. The repository file is checked
// for operator-scope keys before anything loads it, and before any posture it
// sets is read, so its refusal names the rule rather
// than reading as a typo. The tier keys are defined after it has loaded, so even
// a path that skipped that check would find them unknown there. The environment
// is read a second time once the tier keys exist, because LoadEnv reads only the
// keys defined when it runs.
func newRegistry(repoConfig string) (*flags.Registry, error) {
	if err := refuseRepositoryOperatorKeys(repoConfig); err != nil {
		return nil, err
	}
	reg, err := flags.NewHarnessRegistry(repoConfig)
	if err != nil {
		return nil, err
	}
	if err := refuseLoadedRepositoryEndpoints(reg, repoConfig); err != nil {
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
func readConfigFile(path string, open func(string) (*os.File, error)) (flags.ConfigLayer, error) {
	if strings.TrimSpace(path) == "" {
		return flags.ConfigLayer{}, nil
	}
	f, err := open(filepath.Clean(path))
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

// operatorScopeRule is one class of key that only the operator may set. The
// repository's config.yaml is a file a clone ships and an agent can write under
// a relaxed posture, so each class is refused from it, naming the key, rather
// than ignored: an ignored line is a setting that looks applied.
type operatorScopeRule struct {
	// matches reports whether a flattened config key belongs to the class. The
	// parser flattens nested and dotted forms alike and stores a key exactly as
	// written, so a match here is a key the registry would have stored.
	matches func(key string) bool
	// refusal builds the error for the keys of this class that the file sets,
	// sorted.
	refusal func(path string, keys []string) error
}

// endpointKeys are the settings that decide where a provider credential is
// sent, or which credential is used. The adapters attach the resolved secret to
// requests at the base URL, so a value from a clone would redirect it.
var endpointKeys = []string{
	flags.LLMXAIBaseURL,
	flags.LLMGeminiBaseURL,
	flags.LLMLocalBaseURL,
	flags.LLMDefaultProvider,
}

// isEndpointKey reports whether a config key is one of endpointKeys.
func isEndpointKey(key string) bool { return slices.Contains(endpointKeys, key) }

// isUserConfigKey reports whether the user config may set a key: tier keys, the
// endpoint keys, and llm.local.model. That last one names a model id for the
// local server and chooses no credential and no endpoint, so it is safe in a
// file only the operator writes; 'manvi use local --model' saves it there.
func isUserConfigKey(key string) bool {
	return isTierKey(key) || isEndpointKey(key) || key == flags.LLMLocalModel
}

// operatorScopeRules is the one table of what the repository file may not set.
// Rules are tried in order and the first that matches names its keys.
var operatorScopeRules = []operatorScopeRule{
	{
		matches: isTierKey,
		refusal: func(path string, keys []string) error {
			return fmt.Errorf("%s sets %s, and tier settings are operator scope: they decide which model — "+
				"and so which price — every agent runs at, and this file is one a repository ships and an agent "+
				"can write. Remove those lines; set a tier with %s, in %s, or with 'manvi flags set'",
				path, strings.Join(keys, ", "), flags.EnvKey(keys[0]), userConfigHint())
		},
	},
	{
		matches: func(key string) bool { return key == harnessSpecKey },
		refusal: func(path string, keys []string) error {
			return fmt.Errorf("%s sets %s, which is operator scope: it selects text that joins every turn's system "+
				"prompt, and this file ships with a clone and can be written by an agent under a relaxed posture. "+
				"Remove that line; select a spec with %s or 'manvi flags set %s NAME'. The spec file itself can live in %s",
				path, harnessSpecKey, flags.EnvKey(harnessSpecKey), harnessSpecKey, harness.SpecsDir)
		},
	},
	{
		matches: func(key string) bool { return key == serialLookupsKey },
		refusal: func(path string, keys []string) error {
			return fmt.Errorf("%s sets %s, which is operator scope: it decides whether the agent loop enforces "+
				"batching, and this file ships with a clone and can be written by an agent under a relaxed posture. "+
				"Remove that line; set it with %s or 'manvi flags set %s MODE'",
				path, serialLookupsKey, flags.EnvKey(serialLookupsKey), serialLookupsKey)
		},
	},
	{
		matches: func(key string) bool { return key == execContainmentKey },
		refusal: func(path string, keys []string) error {
			return fmt.Errorf("%s sets %s, which is operator scope: it decides whether agent-authored shell commands "+
				"are denied the repository's secret files, and this file ships with a clone and can be written by an "+
				"agent under a relaxed posture. Remove that line; set it with %s or 'manvi flags set %s MODE'",
				path, execContainmentKey, flags.EnvKey(execContainmentKey), execContainmentKey)
		},
	},
	{
		matches: isToolOfferingKey,
		refusal: func(path string, keys []string) error {
			return fmt.Errorf("%s sets %s, which is operator scope: it decides which tools the agent is offered, "+
				"and this file ships with a clone and can be written by an agent under a relaxed posture. "+
				"Remove those lines; set %s with %s or 'manvi flags set %s VALUE'",
				path, strings.Join(keys, ", "), keys[0], flags.EnvKey(keys[0]), keys[0])
		},
	},
	{
		matches: isWebSearchKey,
		refusal: func(path string, keys []string) error {
			return fmt.Errorf("%s sets %s, which is operator scope: web search sends the query text the model writes "+
				"to a third party outside this machine, and this file ships with a clone and can be written by an "+
				"agent under a relaxed posture. Remove those lines; set %s in the environment, in %s, or with "+
				"'manvi flags set %s on'",
				path, strings.Join(keys, ", "), flags.EnvKey(keys[0]), userConfigHint(), webSearchKey)
		},
	},
	{matches: isEndpointKey, refusal: refuseEndpointKeys},
}

// isToolOfferingKey matches the settings that choose which tools a turn offers.
func isToolOfferingKey(key string) bool {
	return key == coreToolsOnlyKey || key == dynamicToolsKey || key == batchToolKey
}

// refuseEndpointKeys is the refusal for endpoint keys set in the repository file.
func refuseEndpointKeys(path string, keys []string) error {
	return fmt.Errorf("%s sets %s, which is operator scope: it decides where a provider's credential is "+
		"sent or which credential is used, and this file ships with a clone and can be written by an agent "+
		"under a relaxed posture, so a value in it could redirect the key. Remove those lines; set %s in "+
		"the environment, or in %s, which lies outside the repository",
		path, strings.Join(keys, ", "), flags.EnvKey(keys[0]), userConfigHint())
}

// refuseLoadedRepositoryEndpoints refuses an endpoint key whose value the
// registry took from the repository config. The registry has already read the
// environment by then (flags.NewHarnessRegistry calls LoadEnv), but the user
// config is not loaded until after this check, and an environment value has
// the origin env, not config, so the config origin can only mean the repository
// file. It asks the registry rather than the file, so it judges the bytes that
// were actually loaded.
func refuseLoadedRepositoryEndpoints(reg *flags.Registry, repoConfig string) error {
	var set []string
	for _, key := range endpointKeys {
		if _, origin, err := reg.String(key); err != nil {
			return err
		} else if origin == flags.OriginConfig {
			set = append(set, key)
		}
	}
	if len(set) == 0 {
		return nil
	}
	slices.Sort(set)
	return refuseEndpointKeys(repoConfig, set)
}

// refuseRepositoryOperatorKeys refuses a repository config that sets any
// operator-scope key. It reads the file as the registry will, and runs before
// the registry loads it, so nothing the same file says — a relaxed posture
// included — is consulted first.
//
// A file that cannot be read or parsed is reported from here, with the path and
// the parser's own account of the line, which is what NewHarnessRegistry would
// have said about it a moment later.
func refuseRepositoryOperatorKeys(path string) error {
	layer, err := readConfigFile(path, flags.OpenRegular)
	if err != nil {
		return err
	}
	for _, rule := range operatorScopeRules {
		var set []string
		for key := range layer.Values {
			if rule.matches(key) {
				set = append(set, key)
			}
		}
		if len(set) > 0 {
			slices.Sort(set)
			return rule.refusal(path, set)
		}
	}
	return nil
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

// userConfigHint names the user config for a refusal, without failing it. It
// names a location only when loadUserConfig would read it; one inside the
// repository is not named, and the advice points at the environment variable
// that moves it instead.
func userConfigHint() string {
	path, err := userConfigPath()
	if err != nil {
		return "the user config (" + envUserConfig + ")"
	}
	if refuseUserConfigInside(path, projectRoot()) != nil {
		return "a user config outside the repository (the current location is inside it, or cannot be checked, " +
			"and manvi refuses it; set " + envUserConfig + " to an absolute path outside the repository)"
	}
	return path
}

// refuseUserConfigInside is the one test of whether a user config location may
// be read: not when it lies inside the repository, and not when that cannot be
// told. loadUserConfig applies it before reading, and userConfigHint applies it
// before naming a location, so advice never names a file manvi would refuse.
func refuseUserConfigInside(path, root string) error {
	inside, err := mcp.PathIsInside(root, path)
	if err != nil {
		return fmt.Errorf("could not tell whether the user config %s lies inside %s: %w", path, root, err)
	}
	if inside {
		return fmt.Errorf("the user config %s lies inside the repository %s; a setting a checked-out tree "+
			"can write is not operator scope, so it was not read — move it, or point %s elsewhere",
			path, root, envUserConfig)
	}
	return nil
}

// loadUserConfig applies the user config to the config layer.
//
// It carries tier settings, the endpoint keys (isEndpointKey) and llm.local.model, and nothing
// else. It shares the config layer with the repository file, and for these keys
// that is unambiguous — the repository can never supply one, see
// operatorScopeRules — but for any other key an origin of "config" would no
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
	if err := refuseUserConfigInside(path, root); err != nil {
		return err
	}
	layer, err := readConfigFile(path, os.Open)
	if err != nil {
		return fmt.Errorf("user config: %w", err)
	}
	var other []string
	for key := range layer.Values {
		if !isUserConfigKey(key) {
			other = append(other, key)
		}
	}
	if len(other) > 0 {
		slices.Sort(other)
		return fmt.Errorf("user config %s sets %s; it carries %s*, provider endpoint settings (%s) and %s only — "+
			"put the rest in the repository's config.yaml or the environment",
			path, strings.Join(other, ", "), tierPrefix, strings.Join(endpointKeys, ", "), flags.LLMLocalModel)
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
	Tier agents.Tier
	// Provider is the provider the operator named for this tier with
	// "provider/model", or empty for a bare id, which runs on the session's
	// provider.
	Provider                      string
	Model, ModelKey, ModelFrom    string
	Effort, EffortKey, EffortFrom string
	// Fallback is where a child on this tier re-runs when its provider is
	// unavailable, always naming its provider, and FallbackFrom where it was
	// set. The zero spec means no fallback.
	Fallback     agents.TierSpec
	FallbackFrom string
	// Ceiling is the session's llm.effort.max as this tier inherits it, set
	// only once the row has been checked: empty where the tier's model cannot
	// climb to it.
	Ceiling string
}

// active reports whether the session applies this row. A row that names its
// provider always applies: the operator said where it runs. A bare row applies
// only where bare ids have always meant something, an anthropic session — the
// shipped profile is Claude ids, and reading them on a Gemini session would
// send every child to a model that provider does not serve.
func (r tierRow) active(table tierTable) bool { return r.Provider != "" || table.Active }

// provider is where the row runs: the provider it names, or the session's.
func (r tierRow) provider(table tierTable) string {
	if r.Provider != "" {
		return r.Provider
	}
	return table.Provider
}

// placement is the row's model in the form Definition.Model uses.
func (r tierRow) placement() string {
	return agents.TierSpec{Provider: r.Provider, Model: r.Model}.Placement()
}

// tierTable is the resolved profile for one session provider.
type tierTable struct {
	Provider string
	// Active is whether bare tier ids apply: true on anthropic only, the one
	// provider the shipped profile is catalogued against. A row that names its
	// provider applies on any session; see tierRow.active.
	Active bool
	Rows   []tierRow
	// Roster is the operator's team, and RosterFrom where it was set.
	Roster     agents.Roster
	RosterFrom string
}

// applies reports whether any row is applied on this session.
func (t tierTable) applies() bool {
	for _, r := range t.Rows {
		if r.active(t) {
			return true
		}
	}
	return false
}

// extraProviders lists the providers this session must build beside its own:
// every provider an applied row or a team member names, sorted, without the
// session's provider. They are built because the operator named them — a
// provider nobody named is never constructed, so a credential nobody asked
// for is never required and a local server nobody pointed at is never probed.
func (t tierTable) extraProviders() []string {
	var out []string
	add := func(p string) {
		if p != "" && p != t.Provider && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	for _, r := range t.Rows {
		if r.active(t) {
			add(r.Provider)
		}
		add(r.Fallback.Provider)
	}
	for _, p := range t.Roster.Providers() {
		add(p)
	}
	slices.Sort(out)
	return out
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

// Profile is the table as the router reads it, or nil when no row applies.
func (t tierTable) Profile() agents.TierProfile {
	if !t.applies() {
		return nil
	}
	profile := agents.TierProfile{}
	for _, r := range t.Rows {
		if r.active(t) {
			profile[r.Tier] = agents.TierSpec{Provider: r.Provider, Model: r.Model, Effort: r.Effort}
		}
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
		if row, err = qualifyRow(row, provider); err != nil {
			return tierTable{}, err
		}
		if row, err = readFallback(reg, row); err != nil {
			return tierTable{}, err
		}
		table.Rows = append(table.Rows, row)
	}
	raw, from, err := tierSetting(reg, rosterKey)
	if err != nil {
		return tierTable{}, err
	}
	if table.Roster, err = agents.ParseRoster(raw); err != nil {
		return tierTable{}, fmt.Errorf("%s (%s): %w", rosterKey, from, err)
	}
	table.RosterFrom = from
	return table, nil
}

// readFallback reads a tier's fallback. A value that does not name its
// provider is refused: the fallback is for leaving a provider that failed, and
// a bare id would run on the session's provider, which is the one that failed
// whenever the tier was on it.
func readFallback(reg *flags.Registry, row tierRow) (tierRow, error) {
	key := tierFallbackKey(row.Tier)
	raw, from, err := tierSetting(reg, key)
	if err != nil || raw == "" {
		return row, err
	}
	provider, model, ok := strings.Cut(raw, "/")
	provider, model = strings.ToLower(strings.TrimSpace(provider)), strings.TrimSpace(model)
	if !ok || provider == "" || model == "" {
		return row, fmt.Errorf("tier %s: %s=%q (%s) must be provider/model", row.Tier, key, raw, from)
	}
	if !knownProvider(provider) {
		return row, fmt.Errorf("tier %s: %s=%q (%s) names provider %q, which this harness has no adapter for (%s)",
			row.Tier, key, raw, from, provider, strings.Join(providerNames(), ", "))
	}
	row.Fallback, row.FallbackFrom = agents.TierSpec{Provider: provider, Model: model}, from
	return row, nil
}

// Fallbacks maps each tier that has a fallback to it.
func (t tierTable) Fallbacks() map[agents.Tier]agents.TierSpec {
	out := map[agents.Tier]agents.TierSpec{}
	for _, r := range t.Rows {
		if r.Fallback.Model != "" {
			out[r.Tier] = r.Fallback
		}
	}
	return out
}

// qualifyRow splits a "provider/model" tier value into its provider and model.
// A bare value is left as it is and keeps running on the session's provider.
//
// The main tier may not move the session: llm.default_provider is the one
// owner of which provider the dispatching agent runs on, and a main tier that
// named another one would be a second answer to that question, applied to the
// children's ladder and not to the parent. Naming the session's own provider
// is the same as writing the bare id, and is read as that.
func qualifyRow(row tierRow, session string) (tierRow, error) {
	provider, model, ok := strings.Cut(row.Model, "/")
	if !ok {
		return row, nil
	}
	provider, model = strings.ToLower(strings.TrimSpace(provider)), strings.TrimSpace(model)
	if provider == "" || model == "" {
		return row, fmt.Errorf("tier %s: %s=%q (%s) must be a bare model id or provider/model",
			row.Tier, row.ModelKey, row.Model, row.ModelFrom)
	}
	if !knownProvider(provider) {
		return row, fmt.Errorf("tier %s: %s=%q (%s) names provider %q, which this harness has no adapter for (%s)",
			row.Tier, row.ModelKey, row.Model, row.ModelFrom, provider, strings.Join(providerNames(), ", "))
	}
	row.Model = model
	if provider == session {
		return row, nil
	}
	if row.Tier == agents.TierMain {
		return row, fmt.Errorf("tier main: %s=%q (%s) names provider %q, but this session runs on %q: "+
			"the main tier is the dispatching agent itself, and %s decides its provider",
			row.ModelKey, provider+"/"+model, row.ModelFrom, provider, session, flags.LLMDefaultProvider)
	}
	row.Provider = provider
	return row, nil
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
	if !table.applies() && table.Roster.Len() == 0 && len(table.Fallbacks()) == 0 {
		return table, nil
	}
	var errs []error
	rows := append([]tierRow(nil), table.Rows...)
	for i, r := range rows {
		if !r.active(table) {
			continue
		}
		// A row on the session's provider is checked against cat, which the
		// caller chose for that provider. A row that names another provider
		// is checked against that provider's own shipped catalogue, and only
		// an anthropic row can be priced against the cap, which meters
		// anthropic alone.
		rowCat, rowCapped := cat, capped
		if r.Provider != "" {
			shipped, ok := shippedCatalogue(r.Provider)
			if !ok {
				// local serves what its server serves; there is no shipped
				// list to check against. buildRunEnv checks the row against
				// the built provider before any child runs.
				continue
			}
			rowCat, rowCapped = shipped, capped && r.Provider == anthropic.Name
		}
		capability, ok := rowCat.Capability(r.Model)
		if !ok {
			errs = append(errs, fmt.Errorf("tier %s: %s=%q (%s) is not in the %s catalogue",
				r.Tier, r.ModelKey, r.Model, r.ModelFrom, r.provider(table)))
			continue
		}
		if _, priced := rowCat.PriceOf(r.Model); rowCapped && !priced {
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
	errs = append(errs, planRoster(table.Roster, capped, ceiling)...)
	for _, r := range table.Rows {
		if r.Fallback.Model == "" {
			continue
		}
		if shipped, ok := shippedCatalogue(r.Fallback.Provider); ok {
			if _, served := shipped.Capability(r.Fallback.Model); !served {
				errs = append(errs, fmt.Errorf("tier %s: %s=%q (%s) is not in the %s catalogue",
					r.Tier, tierFallbackKey(r.Tier), r.Fallback.Placement(), r.FallbackFrom, r.Fallback.Provider))
			}
		}
	}
	if len(errs) > 0 {
		return table, errors.Join(errs...)
	}
	table.Rows = rows
	return table, nil
}

// planRoster checks every team member the way planTiers checks a row that
// names its provider: against that provider's shipped catalogue, its effort
// against the model, and its price when the cap meters it.
func planRoster(roster agents.Roster, capped bool, ceiling string) []error {
	var errs []error
	for _, m := range roster.Members() {
		cat, ok := shippedCatalogue(m.Spec.Provider)
		if !ok {
			if !knownProvider(m.Spec.Provider) {
				errs = append(errs, fmt.Errorf("%s: member %q names provider %q, which this harness has no adapter for (%s)",
					rosterKey, m.Name, m.Spec.Provider, strings.Join(providerNames(), ", ")))
			}
			continue
		}
		capability, ok := cat.Capability(m.Spec.Model)
		if !ok {
			errs = append(errs, fmt.Errorf("%s: member %q runs %q, which is not in the %s catalogue",
				rosterKey, m.Name, m.Spec.Model, m.Spec.Provider))
			continue
		}
		if _, priced := cat.PriceOf(m.Spec.Model); capped && m.Spec.Provider == anthropic.Name && !priced {
			errs = append(errs, fmt.Errorf("%s: member %q runs %q, which has no price, and %s caps spend",
				rosterKey, m.Name, m.Spec.Model, envAnthropicBudget))
		}
		if _, err := agent.PlanTierEffort(capability, m.Spec.Effort, ceiling); err != nil {
			errs = append(errs, fmt.Errorf("%s: member %q effort %q on %s: %w",
				rosterKey, m.Name, m.Spec.Effort, m.Spec.Model, err))
		}
	}
	return errs
}

// shippedCatalogue is the catalogue a provider ships with, for the providers
// that have one. local has none: what it serves is whatever its server lists.
func shippedCatalogue(provider string) (tierCatalogue, bool) {
	switch provider {
	case anthropic.Name:
		return anthropicTiers{}, true
	case gemini.Name:
		return staticCatalogue{capability: gemini.Capability}, true
	case xai.Name:
		return staticCatalogue{capability: xai.Capability}, true
	default:
		return nil, false
	}
}

// staticCatalogue is a shipped catalogue with no price table. A model in it is
// served; none of them is priced, so a cap could not meter one — and no cap
// meters these providers.
type staticCatalogue struct {
	capability func(string) (llm.Capability, bool)
}

func (s staticCatalogue) Capability(model string) (llm.Capability, bool) { return s.capability(model) }
func (staticCatalogue) PriceOf(string) (budget.Prices, bool)             { return budget.Prices{}, false }

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
	return planAnthropicTiers(table, strings.TrimSpace(ceiling))
}

// planAnthropicTiers checks a table against the shipped catalogue and the spend
// cap meterAnthropic reads.
func planAnthropicTiers(table tierTable, ceiling string) (tierTable, error) {
	capped := strings.TrimSpace(os.Getenv(envAnthropicBudget)) != ""
	return planTiers(table, anthropicTiers{}, capped, ceiling)
}

// checkMainTierFor refuses a main tier the catalogue does not hold, or the spend
// cap cannot price, when model is the one the main tier supplies — the model an
// op took from the selection serve offers. It checks that one row with the same
// planner checkTiers runs over all of them: an op that sends no effort and uses
// no other tier has no business with a bad heavy tier, and a model the host
// chose for itself is not the main tier's to refuse.
func checkMainTierFor(reg *flags.Registry, provider, model string) error {
	table, err := resolveTiers(reg, provider)
	if err != nil {
		return err
	}
	main := table.row(agents.TierMain)
	if !table.Active || main.Model != model {
		return nil
	}
	main.Effort = ""
	_, err = planAnthropicTiers(tierTable{Provider: table.Provider, Active: true, Rows: []tierRow{main}}, "")
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
		if r.active(t) {
			continue
		}
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
	switch {
	case table.Active:
		fmt.Fprintf(&b, "tier profile on %s\n\n", table.Provider)
	case table.applies():
		fmt.Fprintf(&b, "tier profile on %s — bare ids are catalogued against %s only, so only the tiers that "+
			"name their provider are applied\n\n", table.Provider, anthropic.Name)
	default:
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
		applied := ""
		if !r.active(table) && table.applies() {
			applied = " · not applied"
		}
		if r.Fallback.Model != "" {
			applied += " · fallback " + r.Fallback.Placement()
		}
		fmt.Fprintf(&b, "  %-8s %-18s from %s (%s) · effort %s from %s (%s) · ceiling %s%s\n",
			r.Tier, r.placement(), r.ModelKey, r.ModelFrom, effort, r.EffortKey, r.EffortFrom, ceiling, applied)
	}
	if members := table.Roster.Members(); len(members) > 0 {
		fmt.Fprintf(&b, "\nteam (%s, %s)\n", rosterKey, table.RosterFrom)
		for _, m := range members {
			effort := m.Spec.Effort
			if effort == "" {
				effort = "-"
			}
			fmt.Fprintf(&b, "  %-12s %-8s %s · effort %s\n", m.Name, m.Tier, m.Placement(), effort)
		}
	}
	if table.applies() {
		fmt.Fprintf(&b, "\nMANVI_MODEL beats the main tier's model and %s its effort. A tier is set with %s*, "+
			"in %s, or with 'manvi flags set'; never from the repository's config.yaml.\n",
			flags.LLMEffort, flags.EnvKey(tierPrefix), userConfigHint())
	}
	_, err := io.WriteString(out, b.String())
	return err
}
