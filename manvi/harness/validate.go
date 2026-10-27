package harness

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/prompt"
)

const (
	// MinSectionOrder is where a spec's sections start. The fixed sections end
	// at 50 and the skills listing sits at 60, so a spec's text comes after all
	// of the rules and never between them.
	MinSectionOrder = 60
	// MaxSectionOrder keeps an order a number a person chose.
	MaxSectionOrder = 10_000
	// MaxSections, MaxRoles and MaxAllowedTools bound the lists of one spec.
	MaxSections     = 32
	MaxRoles        = 16
	MaxAllowedTools = 64
	// MaxSectionBytes bounds one section's text.
	MaxSectionBytes = 16 * 1024
	// MaxAlwaysOnTokens is the hard ceiling on a spec's section text, in
	// estimated tokens, for a prompt with no budget at all. A budgeted prompt
	// allows the smaller of this and 1/BudgetShare of its budget.
	MaxAlwaysOnTokens = 8192
	// BudgetShare is the divisor on a provider's prompt budget: a spec may take
	// one part in this many of it, so a spec cannot crowd out the rules.
	BudgetShare = 4
	// MaxRoleDescriptionRunes and MaxRolePromptRunes bound one role's text.
	MaxRoleDescriptionRunes = 512
	MaxRolePromptRunes      = 8192
)

var toolName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Env is what a spec is judged against: the provider whose prompt it joins, the
// section names that prompt already owns, and its token budget.
type Env struct {
	// Provider names the session's provider, for messages. The limits that
	// differ by provider arrive in BudgetTokens, from the prompt's own budget.
	Provider string
	// Reserved are the section and source names the fixed prompt uses. They come
	// from the assembler's own sources, never from a list kept here, so a
	// section added to the prompt tomorrow is protected tomorrow.
	Reserved []string
	// BudgetTokens is the prompt's token budget, zero when unbounded.
	BudgetTokens int
}

// Problem is one thing wrong with a spec, located by Field.
type Problem struct {
	Field   string
	Message string
}

func (p Problem) String() string { return p.Field + ": " + p.Message }

// Problems is every problem found, in document order. Validate never stops at
// the first: an author fixing a spec should see the whole list once.
type Problems []Problem

// Err is nil for no problems, and otherwise one error listing all of them.
func (p Problems) Err() error {
	if len(p) == 0 {
		return nil
	}
	errs := make([]error, len(p))
	for i, x := range p {
		errs[i] = errors.New(x.String())
	}
	return fmt.Errorf("harness spec has %d problem(s): %w", len(p), errors.Join(errs...))
}

// ValidName reports whether name may name a spec, a section, a role-adjacent
// identifier or a lessons library: the rule skills already use, which also
// keeps a spec name a safe file name.
func ValidName(name string) bool { return ValidSkillName(name) }

// textProblem is why text may not reach a prompt, or "".
func textProblem(text string) string {
	if !utf8.ValidString(text) {
		return "is not valid UTF-8"
	}
	for _, token := range []string{"{{", "}}", "${"} {
		if strings.Contains(text, token) {
			return fmt.Sprintf("contains %q: a spec is data and is never templated", token)
		}
	}
	return hiddenRuneProblem(text)
}

// Validate lists every problem with the spec against env. An empty result
// means the spec may be resolved.
func (s Spec) Validate(env Env) Problems {
	var out Problems
	add := func(field, format string, args ...any) {
		out = append(out, Problem{Field: field, Message: fmt.Sprintf(format, args...)})
	}

	if !ValidName(s.Name) {
		add("name", "%s is not a spec name: %s", QuoteName(s.Name), nameRule)
	}

	reserved := map[string]bool{}
	for _, r := range env.Reserved {
		reserved[strings.ToLower(r)] = true
	}
	if len(s.Sections) > MaxSections {
		add("sections", "%d sections, over the limit of %d", len(s.Sections), MaxSections)
	}
	seen := map[string]bool{}
	tokens := 0
	for i, sec := range s.Sections {
		f := fmt.Sprintf("sections[%d]", i)
		switch {
		case !ValidName(sec.Name):
			add(f, "%s is not a section name: %s", QuoteName(sec.Name), nameRule)
		case reserved[strings.ToLower(sec.Name)]:
			add(f, "section %q collides with a section the prompt already owns", sec.Name)
		case seen[strings.ToLower(sec.Name)]:
			add(f, "section %q is listed twice", sec.Name)
		}
		seen[strings.ToLower(sec.Name)] = true
		if sec.Order < MinSectionOrder || sec.Order > MaxSectionOrder {
			add(f, "order %d is outside %d..%d: spec sections go after the fixed ones", sec.Order, MinSectionOrder, MaxSectionOrder)
		}
		switch {
		case strings.TrimSpace(sec.Text) == "":
			add(f, "section %q has no text", sec.Name)
		case len(sec.Text) > MaxSectionBytes:
			add(f, "section %q is %d bytes, over the limit of %d", sec.Name, len(sec.Text), MaxSectionBytes)
		default:
			if why := textProblem(sec.Text); why != "" {
				add(f, "section %q text %s", sec.Name, why)
			}
		}
		tokens += prompt.EstimateTokens(sec.Text)
	}
	if limit := AlwaysOnLimit(env.BudgetTokens); tokens > limit {
		add("sections", "%d estimated tokens of always-on text, over the %d this %s prompt allows a spec",
			tokens, limit, providerLabel(env.Provider))
	}

	if len(s.Roles) > MaxRoles {
		add("roles", "%d roles, over the limit of %d", len(s.Roles), MaxRoles)
	}
	builtIn := agents.NewRegistry()
	roleSeen := map[string]bool{}
	for i, r := range s.Roles {
		f := fmt.Sprintf("roles[%d]", i)
		switch {
		case !ValidName(r.Name):
			add(f, "%s is not a role name: %s", QuoteName(r.Name), nameRule)
		case builtIn.IsBuiltIn(r.Name):
			add(f, "role %q would shadow a built-in role", r.Name)
		case roleSeen[r.Name]:
			add(f, "role %q is listed twice", r.Name)
		}
		roleSeen[r.Name] = true
		switch {
		case strings.TrimSpace(r.Description) == "":
			add(f, "role %q needs a description: it is how a model knows when to dispatch it", r.Name)
		case utf8.RuneCountInString(r.Description) > MaxRoleDescriptionRunes:
			add(f, "role %q description is over %d runes", r.Name, MaxRoleDescriptionRunes)
		default:
			if why := textProblem(r.Description); why != "" {
				add(f, "role %q description %s", r.Name, why)
			}
		}
		if utf8.RuneCountInString(r.SystemPrompt) > MaxRolePromptRunes {
			add(f, "role %q system_prompt is over %d runes", r.Name, MaxRolePromptRunes)
		} else if why := textProblem(r.SystemPrompt); why != "" {
			add(f, "role %q system_prompt %s", r.Name, why)
		}
		if len(r.AllowedTools) > MaxAllowedTools {
			add(f, "role %q lists %d tools, over the limit of %d", r.Name, len(r.AllowedTools), MaxAllowedTools)
		}
		toolSeen := map[string]bool{}
		for _, t := range r.AllowedTools {
			if !toolName.MatchString(t) || len(t) > 128 {
				add(f, "role %q allowed_tools has %s, which is not a tool name", r.Name, QuoteName(t))
			} else if toolSeen[t] {
				add(f, "role %q allowed_tools lists %q twice", r.Name, t)
			}
			toolSeen[t] = true
		}
	}

	if len(s.Skills) > MaxSkills {
		add("skills", "%d skills, over the limit of %d", len(s.Skills), MaxSkills)
	}
	skillSeen := map[string]bool{}
	for i, name := range s.Skills {
		f := fmt.Sprintf("skills[%d]", i)
		if !ValidSkillName(name) {
			add(f, "%s is not a skill name: %s", QuoteName(name), nameRule)
		} else if skillSeen[name] {
			add(f, "skill %q is listed twice", name)
		}
		skillSeen[name] = true
	}

	if s.Lessons != "" && !ValidName(s.Lessons) {
		add("lessons", "%s is not a lessons library name: %s", QuoteName(s.Lessons), nameRule)
	}
	return out
}

const nameRule = "it must match [a-z0-9][a-z0-9_-]* and be at most 64 characters"

// AlwaysOnLimit is how many estimated tokens of section text a spec may carry
// under a prompt budget: a share of the budget, and never more than the
// absolute ceiling, which is all an unbounded prompt has.
func AlwaysOnLimit(budgetTokens int) int {
	if budgetTokens <= 0 {
		return MaxAlwaysOnTokens
	}
	return min(MaxAlwaysOnTokens, budgetTokens/BudgetShare)
}

func providerLabel(p string) string {
	if p == "" {
		return "provider's"
	}
	return p
}
