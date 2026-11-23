package agents

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// A team is the set of placements an operator has enabled beyond the tier
// ladder: a named member is one model on one provider, admitted at one tier.
//
// The tier ladder answers "what does this class of work cost" with one model
// per class. A team needs more than one answer per class — an advisor that is
// Fable and a second advisor that is a Gemini model, so the two can be made to
// disagree; Haiku scouts beside Gemini scouts — and it needs the dispatching
// model to be able to choose between them. Letting a model write a provider or
// a model id would undo the rule ModelPlacement enforces: a model names costs,
// never ids. A member name is the same kind of thing as a tier name — a word
// from a vocabulary the operator wrote — and it carries its tier with it, so a
// role's ceiling caps a member exactly as it caps a tier.

// Member is one named placement the operator enabled.
type Member struct {
	// Name is what a dispatching model writes to choose this member.
	Name string
	// Tier is the cost class the member is admitted at. A role whose ceiling
	// is below it cannot use the member.
	Tier Tier
	// Spec is where the member runs. Provider is always set: a member exists
	// to name a placement, and a bare model would leave which provider serves
	// it to whatever session happened to load the roster.
	Spec TierSpec
}

// Placement is the member's spec in the form Definition.Model uses.
func (m Member) Placement() string { return m.Spec.Placement() }

// MaxRosterMembers bounds a roster. A team wider than this is a fan-out
// setting, not a list of distinct members anyone reviewed.
const MaxRosterMembers = 32

var memberNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// Roster is the operator's team: the members a dispatching model may name.
// The zero value is an empty roster.
type Roster struct {
	members []Member
}

// ParseRoster reads the roster setting. Entries are separated by commas or
// semicolons and each is
//
//	name=tier:provider/model[@effort]
//
// for example "fable=advisor:anthropic/claude-fable-5-2,
// scout=basic:gemini/gemini-4-hydrogen@low". Every malformed entry is
// reported, not only the first, and nothing is half-applied: an operator who
// mistyped one member does not get a team missing it.
//
// A name that is also a tier is refused, so a dispatch that writes "basic"
// always means the tier. "inherit" is refused for the same reason.
func ParseRoster(raw string) (Roster, error) {
	var (
		members []Member
		errs    []string
		seen    = map[string]bool{}
	)
	for _, entry := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' }) {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		m, err := parseMember(entry)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%q: %v", entry, err))
			continue
		}
		if seen[m.Name] {
			errs = append(errs, fmt.Sprintf("%q: member %q is named twice", entry, m.Name))
			continue
		}
		seen[m.Name] = true
		members = append(members, m)
	}
	if len(members)+len(errs) > MaxRosterMembers {
		errs = append(errs, fmt.Sprintf("%d members, above the bound of %d", len(members)+len(errs), MaxRosterMembers))
	}
	if len(errs) > 0 {
		return Roster{}, fmt.Errorf("team roster: %s", strings.Join(errs, "; "))
	}
	return Roster{members: members}, nil
}

func parseMember(entry string) (Member, error) {
	name, rest, ok := strings.Cut(entry, "=")
	if !ok {
		return Member{}, fmt.Errorf("want name=tier:provider/model[@effort]")
	}
	name = strings.TrimSpace(name)
	if !memberNamePattern.MatchString(name) {
		return Member{}, fmt.Errorf("name %q must be lower-case letters, digits, '-' or '_', starting with a letter, at most 32 long", name)
	}
	if _, isTier := ParseTier(name); isTier || name == InheritSpec {
		return Member{}, fmt.Errorf("name %q is reserved: it already means a tier", name)
	}
	tierText, placement, ok := strings.Cut(rest, ":")
	if !ok {
		return Member{}, fmt.Errorf("want tier:provider/model after %q=", name)
	}
	tier, ok := ParseTier(tierText)
	if !ok {
		return Member{}, fmt.Errorf("%q is not a tier (%s)", strings.TrimSpace(tierText), joinTiers(Tiers()))
	}
	if tier == TierMain {
		return Member{}, fmt.Errorf("a member cannot be on the main tier: main is the dispatching agent itself")
	}
	placement, effort, _ := strings.Cut(placement, "@")
	provider, model, ok := strings.Cut(strings.TrimSpace(placement), "/")
	provider, model = strings.TrimSpace(provider), strings.TrimSpace(model)
	if !ok || provider == "" || model == "" {
		return Member{}, fmt.Errorf("placement %q must be provider/model", strings.TrimSpace(placement))
	}
	return Member{
		Name: name,
		Tier: tier,
		Spec: TierSpec{Provider: strings.ToLower(provider), Model: model, Effort: strings.TrimSpace(effort)},
	}, nil
}

// Members lists the roster in the order the operator wrote it.
func (r Roster) Members() []Member { return slices.Clone(r.members) }

// Len is how many members the roster holds.
func (r Roster) Len() int { return len(r.members) }

// Member finds a member by name. Names are matched exactly after trimming:
// they were validated lower-case, and a near miss is a different name.
func (r Roster) Member(name string) (Member, bool) {
	name = strings.TrimSpace(name)
	for _, m := range r.members {
		if m.Name == name {
			return m, true
		}
	}
	return Member{}, false
}

// Providers lists every provider the roster names, sorted and without
// duplicates.
func (r Roster) Providers() []string {
	var out []string
	for _, m := range r.members {
		if !slices.Contains(out, m.Spec.Provider) {
			out = append(out, m.Spec.Provider)
		}
	}
	slices.Sort(out)
	return out
}

// MemberPlacement resolves what a dispatching model wrote for a child: a tier,
// a member's name, or nothing. It is ModelPlacement widened by exactly the
// roster: a member is admitted only at or below the role's ceiling, by the
// member's own tier, so naming a member can never buy more than naming its
// tier could.
//
// It returns the tier the child runs at and, when a member was named, the
// member. Anything that is neither is refused with both vocabularies listed.
func (r Roster) MemberPlacement(spec string, ceiling Tier) (Tier, *Member, error) {
	spec = strings.TrimSpace(spec)
	if m, ok := r.Member(spec); ok {
		if m.Tier.Rank() > ceiling.Rank() {
			return "", nil, fmt.Errorf("team member %q is on tier %s, above this role's ceiling (%s); allowed here: %s",
				m.Name, m.Tier, ceiling, r.namesAtOrBelow(ceiling))
		}
		return m.Tier, &m, nil
	}
	tier, err := ModelPlacement(spec, ceiling)
	if err != nil && r.Len() > 0 {
		return "", nil, fmt.Errorf("%w; team members allowed here: %s", err, r.namesAtOrBelow(ceiling))
	}
	return tier, nil, err
}

func (r Roster) namesAtOrBelow(ceiling Tier) string {
	var names []string
	for _, m := range r.members {
		if m.Tier.Rank() <= ceiling.Rank() {
			names = append(names, m.Name)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
