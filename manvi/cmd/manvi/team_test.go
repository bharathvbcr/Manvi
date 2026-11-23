package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/credentials"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/llm/gemini"
	"github.com/bharathvbcr/Manvi/manvi/llm/local"
)

// A tier written as provider/model runs on that provider. Before tiers could
// name one, "gemini/gemini-4-hydrogen" was read as a bare Claude id and the
// session refused to start, so a Claude session could not have Gemini scouts.
func TestATierThatNamesItsProviderRunsThere(t *testing.T) {
	t.Setenv("MANVI_MODEL", "")
	t.Setenv(flags.EnvKey(tierModelKey(agents.TierBasic)), "gemini/gemini-4-hydrogen")
	table, err := checkTiers(tierRegistry(t), anthropic.Name)
	if err != nil {
		t.Fatalf("a Gemini basic tier on a Claude session was refused: %v", err)
	}
	basic := table.Profile()[agents.TierBasic]
	if basic.Provider != gemini.Name || basic.Model != "gemini-4-hydrogen" {
		t.Fatalf("basic tier = %+v, want gemini-4-hydrogen on gemini", basic)
	}
	if got := table.extraProviders(); strings.Join(got, ",") != gemini.Name {
		t.Fatalf("extra providers = %v, want [gemini]", got)
	}
	if lite := table.Profile()[agents.TierLite]; lite.Provider != "" {
		t.Errorf("a bare tier must stay on the session's provider, got %+v", lite)
	}
}

// Off anthropic the shipped Claude ids are still not applied — that would send
// every child of a Gemini session to a model Gemini does not serve — but a tier
// the operator qualified is.
func TestOffAnthropicOnlyQualifiedTiersApply(t *testing.T) {
	t.Setenv(flags.EnvKey(tierModelKey(agents.TierAdvisor)), "anthropic/claude-fable-5-1")
	table, err := checkTiers(tierRegistry(t), gemini.Name)
	if err != nil {
		t.Fatal(err)
	}
	profile := table.Profile()
	if len(profile) != 1 {
		t.Fatalf("profile = %+v, want only the qualified advisor", profile)
	}
	if adv := profile[agents.TierAdvisor]; adv.Provider != anthropic.Name || adv.Model != "claude-fable-5-1" {
		t.Fatalf("advisor = %+v", adv)
	}
	for _, key := range table.inactiveOverrides() {
		if key == tierModelKey(agents.TierAdvisor) {
			t.Errorf("the applied advisor tier is reported as not applied")
		}
	}
}

func TestTheMainTierCannotMoveTheSession(t *testing.T) {
	t.Setenv("MANVI_MODEL", "")
	t.Setenv(flags.EnvKey(tierModelKey(agents.TierMain)), "gemini/gemini-4-hydrogen")
	_, err := resolveTiers(tierRegistry(t), anthropic.Name)
	if err == nil || !strings.Contains(err.Error(), flags.LLMDefaultProvider) {
		t.Fatalf("a main tier on another provider = %v; want a refusal naming %s", err, flags.LLMDefaultProvider)
	}

	t.Setenv(flags.EnvKey(tierModelKey(agents.TierMain)), "anthropic/claude-opus-5-5")
	table, err := resolveTiers(tierRegistry(t), anthropic.Name)
	if err != nil {
		t.Fatal(err)
	}
	if main := table.row(agents.TierMain); main.Provider != "" || main.Model != "claude-opus-5-5" {
		t.Fatalf("naming the session's own provider must read as the bare id, got %+v", main)
	}
}

// On a local session a prefix that is not a provider this harness has is part
// of the model id, as local runners' ids often are, and the row applies there.
func TestALocalModelIDWithASlashRunsOnTheLocalSession(t *testing.T) {
	t.Setenv(flags.EnvKey(tierModelKey(agents.TierLite)), "qwen/qwen3-coder")
	table, err := resolveTiers(tierRegistry(t), local.Name)
	if err != nil {
		t.Fatalf("a slash in a local model id refused the session: %v", err)
	}
	for _, row := range table.Rows {
		if row.Tier == agents.TierLite && (row.Provider != local.Name || row.Model != "qwen/qwen3-coder") {
			t.Fatalf("lite = %+v; want the whole id on the local session", row)
		}
	}
}

// Anywhere but local, a prefix naming no provider is a typo and is refused.
// Read as a bare id it would be "not applied" off anthropic, and the children
// would run on the session's model with nothing said in the TUI.
func TestAMistypedTierProviderIsRefusedOffLocal(t *testing.T) {
	for _, session := range []string{anthropic.Name, gemini.Name} {
		t.Setenv(flags.EnvKey(tierModelKey(agents.TierBasic)), "gemnii/gemini-4-hydrogen")
		if _, err := resolveTiers(tierRegistry(t), session); err == nil || !strings.Contains(err.Error(), "gemnii") {
			t.Fatalf("%s session: a mistyped provider = %v; want it refused by name", session, err)
		}
	}
}

// A tier naming the session's own provider applies on that session. Before,
// the provider was stripped to a bare id, which applies only on anthropic, so
// "gemini/gemini-4-hydrogen" on a gemini session was silently not applied.
func TestATierNamingTheSessionsOwnProviderAppliesOffAnthropic(t *testing.T) {
	t.Setenv("MANVI_MODEL", "")
	t.Setenv(flags.EnvKey(tierModelKey(agents.TierBasic)), "gemini/gemini-4-hydrogen")
	table, err := checkTiers(tierRegistry(t), gemini.Name)
	if err != nil {
		t.Fatal(err)
	}
	spec, held := table.Profile()[agents.TierBasic]
	if !held || spec.Model != "gemini-4-hydrogen" {
		t.Fatalf("basic on a gemini session = %+v (held %v); want gemini-4-hydrogen applied", spec, held)
	}
	if got := table.extraProviders(); len(got) != 0 {
		t.Fatalf("the session's own provider was built again: %v", got)
	}
}

func TestATierModelTheNamedProviderDoesNotShipIsRefused(t *testing.T) {
	t.Setenv(flags.EnvKey(tierModelKey(agents.TierLite)), "gemini/claude-sonnet-5-5")
	if _, err := checkTiers(tierRegistry(t), anthropic.Name); err == nil || !strings.Contains(err.Error(), "gemini catalogue") {
		t.Fatalf("= %v, want a refusal against the gemini catalogue", err)
	}
}

func TestTheTeamIsReadAndChecked(t *testing.T) {
	t.Setenv(flags.EnvKey(rosterKey), "fable=advisor:anthropic/claude-fable-5-1,scout=basic:gemini/gemini-4-hydrogen")
	table, err := checkTiers(tierRegistry(t), anthropic.Name)
	if err != nil {
		t.Fatal(err)
	}
	if table.Roster.Len() != 2 {
		t.Fatalf("roster = %+v", table.Roster.Members())
	}
	if got := table.extraProviders(); strings.Join(got, ",") != gemini.Name {
		t.Fatalf("a member's provider must be built: extra providers = %v", got)
	}
	var out strings.Builder
	if err := writeTierTable(&out, table); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "scout") || !strings.Contains(out.String(), "gemini/gemini-4-hydrogen") {
		t.Errorf("manvi tiers does not show the team:\n%s", out.String())
	}

	t.Setenv(flags.EnvKey(rosterKey), "bad=basic:gemini/claude-haiku-5-5")
	if _, err := checkTiers(tierRegistry(t), anthropic.Name); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("a member on a model its provider does not ship = %v", err)
	}
}

func TestTheTeamIsOperatorScope(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"
	if err := os.WriteFile(path, []byte(rosterKey+": a=lite:gemini/gemini-4-hydrogen\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newRegistry(path); err == nil || !strings.Contains(err.Error(), "operator scope") {
		t.Fatalf("a team in the repository config = %v; want it refused as operator scope", err)
	}
}

// The registry a session places children against holds every provider its
// tiers and team name, built by the production constructor. Before, it held
// the session's adapter alone, so an operator placement on another provider
// failed with "not enabled here" in every real run.
func TestTheSessionRegistryHoldsEveryNamedProvider(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "fixture-gemini-key-team")
	t.Setenv(flags.EnvKey(rosterKey), "scout=basic:gemini/gemini-4-hydrogen")
	reg := tierRegistry(t)
	table, err := checkTiers(reg, anthropic.Name)
	if err != nil {
		t.Fatal(err)
	}
	session := &tierProvider{name: anthropic.Name, reasons: map[string][]string{"parent-model": nil}}
	registry, err := buildTeamRegistry(context.Background(), reg, credentials.NewResolver(), io.Discard, session, table)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(registry.Names(), ","); got != "anthropic,gemini" {
		t.Fatalf("registry = %s, want anthropic and gemini", got)
	}
}

func TestANamedProviderWithoutACredentialRefusesTheSession(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv(flags.EnvKey(tierModelKey(agents.TierBasic)), "gemini/gemini-4-hydrogen")
	reg := tierRegistry(t)
	table, err := checkTiers(reg, anthropic.Name)
	if err != nil {
		t.Fatal(err)
	}
	session := &tierProvider{name: anthropic.Name, reasons: map[string][]string{"parent-model": nil}}
	_, err = buildTeamRegistry(context.Background(), reg, credentials.NewResolver(), io.Discard, session, table)
	if err == nil || !strings.Contains(err.Error(), "provider gemini") {
		t.Fatalf("= %v; want the session refused, naming gemini", err)
	}
}

// A provider nobody named is never built: a session with the shipped profile
// must not require a Gemini key or probe a local server.
func TestAProviderNobodyNamedIsNeverBuilt(t *testing.T) {
	t.Setenv("MANVI_MODEL", "")
	table, err := checkTiers(tierRegistry(t), anthropic.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got := table.extraProviders(); len(got) != 0 {
		t.Fatalf("the shipped profile names extra providers %v", got)
	}
}

// twoProviderRunner is a runner attached with a Claude session, a Gemini
// provider and the given tiers and team.
func twoProviderRunner(t *testing.T, tiers agents.TierProfile, roster string) (*subAgentRunner, *tierProvider, *tierProvider) {
	t.Helper()
	claude := &tierProvider{name: anthropic.Name, reasons: map[string][]string{"parent-model": nil}}
	gem := &tierProvider{name: gemini.Name, reasons: map[string][]string{"g-scout": nil, "g-judge": reasoningLevels}}
	models := llm.NewRegistry()
	for _, p := range []llm.Provider{claude, gem} {
		if err := models.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	team, err := agents.ParseRoster(roster)
	if err != nil {
		t.Fatal(err)
	}
	r := &subAgentRunner{}
	r.attach(subAgentConfig{provider: claude, models: models, model: "parent-model", tiers: tiers, roster: team,
		registry: surfaceRegistry(t)})
	return r, claude, gem
}

func TestAChildOnAQualifiedTierRunsOnThatProvider(t *testing.T) {
	r, claude, gem := twoProviderRunner(t,
		agents.TierProfile{agents.TierBasic: {Provider: gemini.Name, Model: "g-scout"}}, "")
	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: "scout", Prompt: "look", ReadOnly: true, Tier: agents.TierBasic,
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(claude.requests()); n != 0 {
		t.Errorf("the session's provider served %d request(s) for a gemini-tier child", n)
	}
	seen := gem.requests()
	if len(seen) == 0 || seen[0].Model != "g-scout" {
		t.Fatalf("gemini requests = %+v, want g-scout", seen)
	}
}

func TestAChildOnATeamMemberRunsWhereTheMemberRuns(t *testing.T) {
	r, claude, gem := twoProviderRunner(t, nil, "judge=advisor:gemini/g-judge@high")
	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: "judge", Prompt: "decide", ReadOnly: true, Tier: agents.TierAdvisor, Member: "judge",
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(claude.requests()); n != 0 {
		t.Errorf("the session's provider served %d request(s) for a member on gemini", n)
	}
	seen := gem.requests()
	if len(seen) == 0 || seen[0].Model != "g-judge" || seen[0].Effort != "high" {
		t.Fatalf("gemini requests = %+v, want g-judge at high", seen)
	}

	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: "ghost", Prompt: "x", ReadOnly: true, Tier: agents.TierBasic, Member: "ghost",
	}); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("a member not on the roster = %v; want a refusal", err)
	}
}

func TestTheRunnerReportsItsTeamToTheDispatcher(t *testing.T) {
	r, _, _ := twoProviderRunner(t,
		agents.TierProfile{agents.TierBasic: {Provider: "local", Model: "qwen"}}, "scout=basic:gemini/g-scout")
	var view devcouncil.TeamView = r
	if _, ok := view.TeamRoster().Member("scout"); !ok {
		t.Fatal("the dispatcher cannot see the team")
	}
	if got := view.TierPlacement(agents.TierBasic); got != "local/qwen" {
		t.Fatalf("basic placement = %q; a tier on local must reach the fan-out width", got)
	}
}

// One escalation moves a child to the nearest tier that runs a different
// model. The same id on another provider is a different model: before
// providers could be named, the ladder compared ids alone and skipped it.
func TestEscalationClimbsToTheSameIdOnAnotherProvider(t *testing.T) {
	profile := agents.TierProfile{
		agents.TierLite:  {Model: "m"},
		agents.TierHeavy: {Provider: gemini.Name, Model: "m"},
	}
	if to, ok := nextTier(profile, agents.TierLite); !ok || to != agents.TierHeavy {
		t.Fatalf("nextTier = %v, %v; want heavy", to, ok)
	}
}

// The team is read from the operator's user config as well as the
// environment, and the roster's punctuation survives the config parser.
func TestTheTeamIsReadFromTheUserConfig(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	body := "llm.tier.basic: gemini/gemini-4-hydrogen\n" +
		"llm.tier.lite.fallback: gemini/gemini-4-hydrogen\n" +
		"llm.tier.roster: fable=advisor:anthropic/claude-fable-5-1@high,scout=basic:gemini/gemini-4-hydrogen\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envUserConfig, path)
	t.Setenv("MANVI_MODEL", "")
	table, err := checkTiers(tierRegistry(t), anthropic.Name)
	if err != nil {
		t.Fatal(err)
	}
	fable, ok := table.Roster.Member("fable")
	if !ok || fable.Spec.Effort != "high" || fable.Spec.Model != "claude-fable-5-1" || fable.Tier != agents.TierAdvisor {
		t.Fatalf("fable = %+v (%v); the roster did not survive the config parser", fable, ok)
	}
	if !strings.HasPrefix(table.RosterFrom, "user config") {
		t.Errorf("roster origin = %q, want the user config", table.RosterFrom)
	}
	if basic := table.Profile()[agents.TierBasic]; basic.Provider != gemini.Name {
		t.Errorf("basic tier from the user config = %+v", basic)
	}
	if fb := table.Fallbacks()[agents.TierLite]; fb.Provider != gemini.Name {
		t.Errorf("lite fallback from the user config = %+v", fb)
	}
}

// A Claude session with an effort and a ceiling set, and a Gemini tier and
// member beside it, is the ordinary mixed team; neither the startup check nor
// the placement may refuse it for an effort the Gemini model was never asked
// to carry.
func TestAMixedTeamUnderAnEffortCeilingStarts(t *testing.T) {
	t.Setenv("MANVI_MODEL", "")
	t.Setenv(flags.EnvKey(flags.LLMEffort), "low")
	t.Setenv(flags.EnvKey(flags.LLMEffortCeiling), "high")
	t.Setenv(flags.EnvKey(tierModelKey(agents.TierBasic)), "gemini/gemini-4-hydrogen")
	t.Setenv(flags.EnvKey(rosterKey), "scout=basic:gemini/gemini-4-hydrogen")
	if _, err := checkTiers(tierRegistry(t), anthropic.Name); err != nil {
		t.Fatalf("a mixed team under an effort ceiling was refused at startup: %v", err)
	}

	r, _, gem := twoProviderRunner(t,
		agents.TierProfile{agents.TierBasic: {Provider: gemini.Name, Model: "g-scout"}}, "scout=basic:gemini/g-scout")
	r.mu.Lock()
	r.cfg.effort, r.cfg.effortCeiling = "low", "high"
	r.mu.Unlock()
	for _, req := range []devcouncil.SubAgentRequest{
		{Label: "tier", Prompt: "look", ReadOnly: true, Tier: agents.TierBasic},
		{Label: "member", Prompt: "look", ReadOnly: true, Tier: agents.TierBasic, Member: "scout"},
	} {
		if _, err := r.RunSubAgent(context.Background(), req); err != nil {
			t.Fatalf("%s child on a non-reasoning Gemini model was refused: %v", req.Label, err)
		}
	}
	for _, req := range gem.requests() {
		if req.Effort != "" {
			t.Errorf("a non-reasoning model was sent effort %q", req.Effort)
		}
	}
}

// The Anthropic spend cap meters anthropic only; a session that builds another
// provider under it says so rather than looking capped.
func TestTheCapSaysWhatItDoesNotMeter(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "fixture-gemini-key-cap")
	t.Setenv(envAnthropicBudget, "5")
	t.Setenv(flags.EnvKey(rosterKey), "scout=basic:gemini/gemini-4-hydrogen")
	reg := tierRegistry(t)
	table, err := checkTiers(reg, anthropic.Name)
	if err != nil {
		t.Fatal(err)
	}
	session := &tierProvider{name: anthropic.Name, reasons: map[string][]string{"parent-model": nil}}
	var notes strings.Builder
	if _, err := buildTeamRegistry(context.Background(), reg, credentials.NewResolver(), &notes, session, table); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(notes.String(), "not metered") || !strings.Contains(notes.String(), gemini.Name) {
		t.Fatalf("notes = %q; want the unmetered provider named", notes.String())
	}
}
