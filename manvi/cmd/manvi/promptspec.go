package main

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/repomap"
	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/harness"
	"github.com/bharathvbcr/Manvi/manvi/llm/local"
	"github.com/bharathvbcr/Manvi/manvi/prompt"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// This file owns how a harness spec reaches a turn: the setting that selects
// it, the one resolution both faces make beside the project instructions, and
// the identity the session log records.
//
// harness.spec is defined here, as the tier keys are, and for a reason that
// shares part of theirs: this harness alone reads it, so it does not belong in
// DevCouncil's catalogue. It is operator scope — the environment or `flags set`
// on human authority — and a repository config that names it is refused with
// the rule rather than failing as an unknown key. That is narrower than a
// repository naming a spec, deliberately: the registry loads the repository's
// config before any Manvi-owned key exists, and what a repository ships is the
// spec file itself, which only the operator's selection brings into a prompt.

// harnessSpecKey is the setting naming the spec a session runs with.
const harnessSpecKey = "harness.spec"

// harnessFlagDefs declares the spec selector: a string, human-only like the
// tier keys, and not a safety flag. Empty selects no spec, which is the
// behaviour of every release before this one.
func harnessFlagDefs() []flags.Def {
	return []flags.Def{{
		Key: harnessSpecKey, Kind: flags.KindString, Default: "", Mutable: flags.HumanOnly,
		Description: "Name of the harness spec this session runs with: extra prompt sections, a skills " +
			"selection, a lessons library and sub-agent roles, from a spec the process registered or " +
			".devcouncil/harness/<name>.json. Empty runs none. A spec cannot set flags, tiers, posture, " +
			"the verify command, fetch hosts or MCP servers, and cannot widen a role. Operator scope: " +
			"never read from the repository's config.yaml.",
	}}
}

// refuseRepositoryHarnessSpec refuses a repository config that sets
// harness.spec, naming the rule. Without it the line would fail as an unknown
// key, which reads as a typo; this reads as what it is. The key is operator
// scope, set with MANVI_HARNESS_SPEC or `manvi flags set harness.spec NAME`.
func refuseRepositoryHarnessSpec(path string) error {
	layer, err := readConfigFile(path)
	if err != nil {
		return err
	}
	if _, set := layer.Values[harnessSpecKey]; !set {
		return nil
	}
	return fmt.Errorf("%s sets %s, which is operator scope: it selects text that joins every turn's system "+
		"prompt, and this file ships with a clone and can be written by an agent under a relaxed posture. "+
		"Remove that line; select a spec with %s or 'manvi flags set %s NAME'. The spec file itself can live in %s",
		path, harnessSpecKey, flags.EnvKey(harnessSpecKey), harnessSpecKey, harness.SpecsDir)
}

// specIdentity is what the log records about the spec a prompt carried. Both
// fields are empty when none was applied, including when one was named and
// refused: the log describes the prompt that was sent.
type specIdentity struct {
	Name, Digest string
}

// turnInputs is what the faces hand the per-turn resolution: where repository
// bytes are read, the role registry specs register into, and the tool registry
// whose surface bounds those roles.
type turnInputs struct {
	skills   skillLoader
	repoRead func(ctx context.Context) harness.ReadFunc
	roles    *agents.Registry
	pipeline *tools.Registry
}

// turnState is what a session remembers between turns of this resolution.
type turnState struct {
	skillNotices noticeOnce
	specNotices  noticeOnce
	roles        harness.Roles
}

// skillsFiltered lists only the skills a spec selected. It filters what the
// repository's own loader returned, so it can never list one the repository
// does not have.
type skillsFiltered struct {
	skillLoader
	only []string
}

func (s skillsFiltered) LoadSkills(ctx context.Context) (harness.SkillSet, error) {
	set, err := s.skillLoader.LoadSkills(ctx)
	if err != nil {
		return set, err
	}
	return harness.FilterSkills(set, s.only), nil
}

// resolveTurnInputs resolves, once per turn and for both faces, everything a
// turn's prompt takes from the repository beside the project instructions: the
// harness spec and the skills listing, which the spec may narrow.
//
// It fills opts, and returns the identity of the spec the prompt carries. A
// spec that is unknown, unreadable or invalid is a notice and not a failure:
// the turn runs without it, and without its roles, and says so. Nothing here
// runs after assembly, so nothing changes the system prompt mid-turn.
func resolveTurnInputs(ctx context.Context, reg *flags.Registry, in turnInputs, st *turnState, opts *PromptOptions) specIdentity {
	provider := opts.Provider
	budget := promptTokenBudget(reg, provider)

	spec, specNotices := resolveSpec(ctx, reg, in, st, *opts, budget)
	var id specIdentity
	var only []string
	if spec != nil {
		id = specIdentity{Name: spec.Name, Digest: spec.Digest()}
		opts.SpecSources = spec.Sources()
		only = spec.Skills
	}

	var skillNotices []string
	loader := in.skills
	if len(only) > 0 {
		loader = skillsFiltered{skillLoader: in.skills, only: only}
	}
	opts.Skills, skillNotices = skillsForPrompt(ctx, loader, budget)
	opts.SkillNotices = st.skillNotices.fresh(skillNotices)

	// Validation was told the names the prompt can own; this is the check
	// against the sources this turn actually has, last, because the skills
	// listing is only known now. A spec whose section the assembler would
	// refuse is not applied at all, so the log never records a spec as applied
	// for a prompt that lacks part of it.
	if spec != nil {
		if clash := collidingSection(reg, *opts, spec); clash != "" {
			_, why := dropSpec(in, st, fmt.Sprintf("harness spec %q was not applied and the turn runs without it: its section %q "+
				"collides with a section this turn's prompt already has", spec.Name, clash))
			specNotices = append(specNotices, why...)
			opts.SpecSources, id = nil, specIdentity{}
		}
	}
	opts.SpecNotices = st.specNotices.fresh(specNotices)
	return id
}

// collidingSection names a section of the spec that shares a name with a source
// the turn's prompt, without the spec, already registers; "" when none does.
func collidingSection(reg *flags.Registry, opts PromptOptions, spec *harness.Spec) string {
	opts.SpecSources = nil
	taken := map[string]bool{}
	for _, src := range promptSources(reg, opts) {
		taken[src.Name()] = true
	}
	for _, sec := range spec.Sections {
		if taken[sec.Name] {
			return sec.Name
		}
	}
	return ""
}

// resolveSpec selects, resolves and validates the session's spec, applying its
// roles. It returns nil with the reason when the turn should run without one.
func resolveSpec(ctx context.Context, reg *flags.Registry, in turnInputs, st *turnState, opts PromptOptions, budget int) (*harness.Spec, []string) {
	name, _, err := reg.String(harnessSpecKey)
	if err != nil {
		return dropSpec(in, st, fmt.Sprintf("the %s setting could not be read: %v", harnessSpecKey, err))
	}
	if strings.TrimSpace(name) == "" {
		return dropSpec(in, st)
	}
	inputs := []harness.Input{harness.DefaultCatalogue, harness.RepoSpecs{Read: in.repoRead(ctx)}}
	spec, origin, err := harness.Resolve(name, inputs...)
	if err != nil {
		return dropSpec(in, st, fmt.Sprintf("harness spec %q was not applied and the turn runs without it: %v", name, err))
	}
	env := harness.Env{Provider: opts.Provider, Reserved: reservedPromptNames(reg, opts), BudgetTokens: budget}
	if problems := spec.Validate(env); len(problems) > 0 {
		lines := make([]string, len(problems))
		for i, p := range problems {
			lines[i] = p.String()
		}
		return dropSpec(in, st, fmt.Sprintf("harness spec %q (%s) is invalid, was not applied, and the turn runs without it: %s",
			name, origin, strings.Join(lines, "; ")))
	}
	var notices []string
	if in.roles != nil {
		notices = st.roles.Apply(in.roles, spec.Roles, surfaceOf(in.pipeline))
	}
	return &spec, notices
}

// dropSpec is the no-spec outcome: the roles of any earlier spec are removed,
// and the reasons, if any, become the turn's notices.
func dropSpec(in turnInputs, st *turnState, reasons ...string) (*harness.Spec, []string) {
	if in.roles != nil {
		reasons = append(reasons, st.roles.Apply(in.roles, nil, harness.Surface{})...)
	}
	return nil, reasons
}

// reservedPromptNames are the source and section names the prompt owns without
// a spec: asked of the assembler's own sources, so a section added to the
// prompt is protected from collision the day it is added.
//
// They are asked of a prompt with every option on, not of this turn's: a source
// that is conditional today (code-map guidance, documentation lookup, the skills
// listing, the project instructions, a provider's own sections) would otherwise
// be a name a spec could take, to be refused at assembly after it validated.
func reservedPromptNames(reg *flags.Registry, opts PromptOptions) []string {
	full := opts
	full.SpecSources = nil
	full.TaskToolsOffered, full.DynamicTools = true, true
	full.CodeMapAvailable, full.DocLookupAvailable = true, true
	full.ActiveGroups = []string{tools.GroupCore, tools.GroupTask, tools.GroupNav, tools.GroupSubagent,
		tools.GroupMCP, tools.GroupArtifact, tools.GroupQuestion, tools.GroupWeb}
	full.Areas = []repomap.AreaSummary{{Name: "area", Files: 1}}
	full.Skills = prompt.Static(skillsSourceName, skillsSectionOrder, false, "x")
	names := []string{skillsSourceName, projectInstructionsName}
	for _, provider := range []string{opts.Provider, local.Name, "anthropic"} {
		full.Provider = provider
		for _, src := range promptSources(reg, full) {
			names = append(names, src.Name())
			if secs, err := src.Sections(); err == nil {
				for _, s := range secs {
					names = append(names, s.Name)
				}
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// surfaceOf is what the session itself holds, which a spec's roles can only
// narrow.
func surfaceOf(pipeline *tools.Registry) harness.Surface {
	var sf harness.Surface
	if pipeline == nil {
		return sf
	}
	for _, t := range pipeline.ListSummaries() {
		sf.Tools = append(sf.Tools, t.Name)
		if !t.ReadOnly {
			sf.Writes = true
		}
	}
	return sf
}
