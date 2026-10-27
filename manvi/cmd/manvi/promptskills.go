package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/harness"
	"github.com/bharathvbcr/Manvi/manvi/prompt"
)

// skillsSectionOrder places the skills listing after every guidance section
// (the last of which sits at 50) and after the project instructions at 35.
// The listing is non-Essential, so under budget pressure it is the first thing
// the assembler drops, ahead of every Essential section.
const skillsSectionOrder = 60

// skillsSourceName and projectInstructionsName are the names of the two sources
// the per-turn resolution adds to the prompt, named once so the names a spec may
// not reuse are the names the sources have.
const (
	skillsSourceName        = "skills"
	projectInstructionsName = "project-instructions"
)

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
	// NoteListedSkills tells the loader which names this prompt carries, so a
	// refusal of an unknown name can offer the ones it does not.
	NoteListedSkills(names []string)
}

// skillsForPrompt resolves a turn's skills listing once, for both faces.
//
// It returns the source to register, nil when there is nothing to list, and the
// notices to surface through the prompt-fault path: each rejected skill with
// its reason, the counts past the loader's bounds, and a failed load. A failed
// load, a cancelled one included, is a single notice and never an empty
// section: the turn runs without a listing and says so.
//
// budgetTokens is the prompt's token budget, zero for an unbounded prompt, in
// which case only the loader's own hard caps apply.
func skillsForPrompt(ctx context.Context, loader skillLoader, budgetTokens int) (prompt.Source, []string) {
	unread := func(err error) (prompt.Source, []string) {
		loader.NoteListedSkills(nil)
		return nil, []string{fmt.Sprintf("skills could not be read, so none are listed this turn: %v", err)}
	}
	if err := ctx.Err(); err != nil {
		return unread(err)
	}
	set, err := loader.LoadSkills(ctx)
	if err == nil {
		// A read cut short by cancellation can come back as a short list.
		err = ctx.Err()
	}
	if err != nil {
		return unread(err)
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
	if set.ListingCut {
		notices = append(notices, fmt.Sprintf("the skills directory holds more than %d entries; only the first %d were considered, so the listing may be missing skills",
			harness.MaxSkillDirEntries, harness.MaxSkillDirEntries))
	}
	limit := 0
	if budgetTokens > 0 {
		limit = budgetTokens / skillsBudgetShare
	}
	listing, ok := harness.RenderSkills(set.Skills, limit)
	if !ok {
		loader.NoteListedSkills(nil)
		if len(set.Skills) > 0 {
			notices = append(notices, fmt.Sprintf("%d skills exist but the listing does not fit in %d tokens; none are listed",
				len(set.Skills), limit))
		}
		return nil, notices
	}
	loader.NoteListedSkills(listing.Listed)
	return prompt.Static(skillsSourceName, skillsSectionOrder, false, listing.Text), notices
}

// noticeOnce reports a standing condition once rather than on every turn.
//
// Both faces resolve the skills listing every turn, and a rejected skill is the
// same on turn two as on turn one, so printing each notice each turn buries the
// one turn where something changed. It keeps the digest of the last notice set,
// sorted so the order of discovery cannot matter, and passes a set through only
// when it differs from that one. An empty set is remembered too, so a condition
// that clears and comes back is reported again. The zero value is ready.
type noticeOnce struct {
	last [sha256.Size]byte
	seen bool
}

// fresh returns notices when the set differs from the last one passed, and nil
// when it is unchanged.
func (n *noticeOnce) fresh(notices []string) []string {
	sorted := append([]string(nil), notices...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	if n.seen && sum == n.last {
		return nil
	}
	n.last, n.seen = sum, true
	return notices
}
