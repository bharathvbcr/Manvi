package main

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/bharathvbcr/Manvi/manvi/harness"
	"github.com/bharathvbcr/Manvi/manvi/prompt"
)

// skillsSectionOrder places the skills listing after every guidance section
// (the last of which sits at 50) and after the project instructions at 35.
// The listing is non-Essential, so under budget pressure it is the first thing
// the assembler drops, ahead of every Essential section.
const skillsSectionOrder = 60

// skillsBudgetShare is the divisor on the prompt's token budget: the listing
// may take one part in this many of it. A repository with many skills must not
// be able to crowd out the rules, so the listing is bounded rather than
// trusted to fit.
const skillsBudgetShare = 4

// skillLoader is what the prompt needs from the skills owner. One production
// implementation: *devcouncil.Registry, the same reader devcouncil_load_skill
// answers from, so the names listed here are the names it accepts.
type skillLoader interface {
	LoadSkills(ctx context.Context) (harness.SkillSet, error)
}

// skillsForPrompt resolves a turn's skills listing once, for both faces.
//
// It returns the source to register, nil when there is nothing to list, and the
// notices to surface through the prompt-fault path: each rejected skill with
// its reason, the counts past the loader's bounds, and a failed load. A failed
// load is never an empty section: the turn runs without a listing and says so.
//
// budgetTokens is the prompt's token budget, zero for an unbounded prompt, in
// which case only the loader's own hard caps apply.
func skillsForPrompt(ctx context.Context, loader skillLoader, budgetTokens int) (prompt.Source, []string) {
	set, err := loader.LoadSkills(ctx)
	if err != nil {
		return nil, []string{fmt.Sprintf("skills could not be read, so none are listed this turn: %v", err)}
	}
	var notices []string
	for _, r := range set.Rejected {
		notices = append(notices, fmt.Sprintf("skill %q was rejected and is not listed: %s", r.Name, r.Reason))
	}
	if set.RejectedOmitted > 0 {
		notices = append(notices, fmt.Sprintf("%d more skill directories were rejected and are not named here", set.RejectedOmitted))
	}
	if set.NotExamined > 0 {
		notices = append(notices, fmt.Sprintf("%d skill directories past the limit of %d were not examined; the listing is a prefix",
			set.NotExamined, harness.MaxSkills))
	}
	limit := 0
	if budgetTokens > 0 {
		limit = budgetTokens / skillsBudgetShare
	}
	text, ok := skillsSectionWithin(set.Skills, limit)
	if !ok {
		if len(set.Skills) > 0 {
			notices = append(notices, fmt.Sprintf("%d skills exist but the listing does not fit in %d tokens; none are listed",
				len(set.Skills), limit))
		}
		return nil, notices
	}
	return prompt.Static("skills", skillsSectionOrder, false, text), notices
}

// skillsSectionWithin renders the listing inside a token limit, zero meaning
// none. SkillsSection is bounded in runes; the prompt's budget is in
// prompt.EstimateTokens, the unit boundProjectInstructions already uses, so the
// rune budget is found as the largest one whose rendering still fits. The
// result depends only on its arguments, which keeps the prompt-cache prefix
// byte-identical across assemblies.
func skillsSectionWithin(skills []harness.Skill, tokens int) (string, bool) {
	full, ok := harness.SkillsSection(skills, 0)
	if !ok || tokens <= 0 || prompt.EstimateTokens(full) <= tokens {
		return full, ok
	}
	best, found := "", false
	lo, hi := 1, utf8.RuneCountInString(full)
	for lo <= hi {
		mid := (lo + hi) / 2
		text, fits := harness.SkillsSection(skills, mid)
		if fits && prompt.EstimateTokens(text) <= tokens {
			best, found = text, true
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return best, found
}
