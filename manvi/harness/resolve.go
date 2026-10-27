package harness

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"sync"
)

// SpecsDir is where a repository keeps named specs, relative to its root, one
// <name>.json each.
//
// The directory is under .devcouncil, which an agent's write policy denies: a
// repository spec is text that reaches every later turn's system prompt, so an
// agent that could write it could rewrite its own instructions between turns.
// That denial sits in the hard-rule block the yolo posture switches off, so
// under yolo an agent can write here, and the spec's digest in the session log
// is the record that it changed.
const SpecsDir = ".devcouncil/harness"

// Input is one place a spec may come from. Resolve asks each in turn; they are
// inputs to one resolver and not branches of it, so an inline spec supplied by
// a host is one more Input and nothing else changes.
type Input interface {
	// Lookup returns the spec called name, false when this input has none, and
	// an error when it has something it cannot use.
	Lookup(name string) (Spec, bool, error)
	// Origin says where a spec found by this input came from, for the log.
	Origin(name string) string
}

// Resolve finds the spec called name in the first input that has it and
// decodes it. Order is precedence: put the inputs the operator controls first.
// A found spec is returned undecided about validity; the caller validates it
// against the session it joins.
func Resolve(name string, inputs ...Input) (Spec, string, error) {
	if !ValidName(name) {
		return Spec{}, "", fmt.Errorf("%s is not a spec name: %s", QuoteName(name), nameRule)
	}
	for _, in := range inputs {
		spec, ok, err := in.Lookup(name)
		if err != nil {
			return Spec{}, "", fmt.Errorf("harness spec %q: %w", name, err)
		}
		if !ok {
			continue
		}
		if spec.Name != name {
			return Spec{}, "", fmt.Errorf("harness spec %q: %s holds a spec named %q", name, in.Origin(name), spec.Name)
		}
		return spec, in.Origin(name), nil
	}
	return Spec{}, "", &NoSuchSpecError{Name: name}
}

// NoSuchSpecError is Resolve's answer when no input has the spec: absence, which
// a host op reports differently from a spec that exists and is refused.
type NoSuchSpecError struct{ Name string }

func (e *NoSuchSpecError) Error() string { return fmt.Sprintf("no harness spec named %q", e.Name) }

// Catalogue is a process-level set of named specs: what a host registers before
// a session starts. It is empty by default.
type Catalogue struct {
	mu    sync.RWMutex
	specs map[string]Spec
}

// DefaultCatalogue is the process's catalogue, the one the harness consults.
var DefaultCatalogue = &Catalogue{}

// Register adds a spec. It is refused for an invalid name or a duplicate: a
// catalogue entry is never silently replaced. Validity against a session is
// judged when the spec is resolved, since that depends on the provider.
func (c *Catalogue) Register(s Spec) error {
	if !ValidName(s.Name) {
		return fmt.Errorf("%s is not a spec name: %s", QuoteName(s.Name), nameRule)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.specs[s.Name]; dup {
		return fmt.Errorf("harness spec %q is already registered", s.Name)
	}
	if c.specs == nil {
		c.specs = map[string]Spec{}
	}
	c.specs[s.Name] = s.Clone()
	return nil
}

// Names lists the registered specs, sorted.
func (c *Catalogue) Names() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.specs))
	for n := range c.specs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Lookup implements Input.
func (c *Catalogue) Lookup(name string) (Spec, bool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.specs[name]
	return s.Clone(), ok, nil
}

// Origin implements Input.
func (c *Catalogue) Origin(string) string { return "catalogue" }

// RepoSpecs reads specs from SpecsDir through the repository's contained
// reader, the same one skills are read with.
type RepoSpecs struct {
	Read ReadFunc
}

func specFile(name string) string { return SpecsDir + "/" + name + ".json" }

// Lookup implements Input. A missing file is "no such spec here"; any other
// failure, a withheld or oversized file included, is an error and is not
// mistaken for absence.
func (r RepoSpecs) Lookup(name string) (Spec, bool, error) {
	if r.Read == nil {
		return Spec{}, false, nil
	}
	raw, err := r.Read(specFile(name), MaxSpecBytes)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Spec{}, false, nil
		}
		return Spec{}, false, err
	}
	s, err := DecodeSpec(raw)
	if err != nil {
		return Spec{}, false, fmt.Errorf("%s: %w", specFile(name), err)
	}
	return s, true, nil
}

// Origin implements Input.
func (r RepoSpecs) Origin(name string) string { return specFile(name) }

// FilterSkills narrows a loaded skill set to the names a spec selected. An
// empty selection is every skill. It only removes, so it can never list a skill
// the repository does not have; a selected name with no skill is reported in
// Rejected, which the prompt surfaces as a notice, and is never ignored.
func FilterSkills(set SkillSet, only []string) SkillSet {
	if len(only) == 0 {
		return set
	}
	want := make(map[string]bool, len(only))
	for _, n := range only {
		want[n] = true
	}
	out := SkillSet{Rejected: set.Rejected, RejectedOmitted: set.RejectedOmitted, NotExamined: set.NotExamined, ListingCut: set.ListingCut}
	for _, s := range set.Skills {
		if want[s.Name] {
			out.Skills = append(out.Skills, s)
			delete(want, s.Name)
		}
	}
	missing := make([]string, 0, len(want))
	for n := range want {
		missing = append(missing, n)
	}
	sort.Strings(missing)
	for _, n := range missing {
		if _, rejected := set.Rejection(n); rejected {
			continue // already reported, with its own reason
		}
		out.Rejected = append(out.Rejected, Rejection{Name: n, Reason: "the harness spec selects it but the repository has no such skill"})
	}
	return out
}
