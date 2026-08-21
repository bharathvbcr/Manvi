// Package prompt assembles the system prompt.
//
// It exists as a seam rather than a string constant because of the invariant
// the rest of the harness is built around: everything the model sees must be
// reconstructable from the session log. A prompt assembled by string
// concatenation somewhere in the loop satisfies that only by accident — the log
// records the final text, but nothing records which parts were included, which
// were dropped for budget, or which failed to load. When a run goes wrong the
// question is always "what did the model actually know", and a flat string
// cannot answer it.
//
// So a prompt here is a list of named, ordered, individually-attributable
// sections, and assembly returns both the text and an account of what happened
// to each one.
package prompt

import (
	"fmt"
	"sort"
	"strings"
)

// Section is one contributed part of the prompt.
type Section struct {
	// Name identifies the section in the assembly report. Unique within a set.
	Name string
	// Order sorts sections. Ties break by name, so assembly is deterministic
	// and two runs with the same inputs produce byte-identical prompts —
	// which is what makes a prompt diffable across runs and cacheable.
	Order int
	// Text is the content. Empty means the section contributes nothing and is
	// recorded as omitted rather than silently skipped.
	Text string
	// Essential marks a section that must not be dropped for budget. The
	// policy rules and the tool contract are essential: a model told it may
	// write anywhere, because the scope section was trimmed, is a model that
	// will try.
	Essential bool
}

// Source contributes sections. It is an interface so the prompt is assembled
// from the same registry pattern as everything else here, and so a contributor
// that fails is reported rather than throwing away the run.
type Source interface {
	// Name identifies the contributor in the report.
	Name() string
	// Sections returns what it contributes, or an error.
	Sections() ([]Section, error)
}

// SourceFunc adapts a function to a Source.
type SourceFunc struct {
	Label string
	Fn    func() ([]Section, error)
}

// Name identifies the contributor.
func (s SourceFunc) Name() string { return s.Label }

// Sections calls Fn.
func (s SourceFunc) Sections() ([]Section, error) { return s.Fn() }

// Static returns a Source contributing one fixed section.
func Static(name string, order int, essential bool, text string) Source {
	return SourceFunc{Label: name, Fn: func() ([]Section, error) {
		return []Section{{Name: name, Order: order, Text: text, Essential: essential}}, nil
	}}
}

// EstimateTokens provides a fast, calibrated BPE token estimate for mixed code and prose.
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	// Calibrated for BPE tokenizers (tiktoken, Qwen 152k, LLaMA):
	// Count alphanumeric words, symbol transitions, punctuation tokens, and whitespace indents.
	tokens := 0
	inWord := false
	spaceRun := 0
	// runLen counts the current word's characters. A BPE tokenizer does not
	// emit one token for an arbitrarily long run: base64 blobs, hashes and
	// minified single-line files split every few characters. Measured against
	// a 2 MB unbroken run, counting the whole run as one word under-estimated
	// by five orders of magnitude — which read as "plenty of room" to the
	// compaction planner and sent the request out to be truncated.
	runLen := 0
	for i := 0; i < len(text); i++ {
		b := text[i]
		if b == ' ' {
			spaceRun++
			if spaceRun == 4 {
				tokens++
				spaceRun = 0
			}
			inWord = false
			continue
		}
		spaceRun = 0

		if b == '\t' || b == '\n' {
			tokens++
			inWord = false
			continue
		}

		isAlpha := (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
		if isAlpha {
			if !inWord {
				tokens++
				inWord = true
				runLen = 1
			} else {
				runLen++
				// Every 8 characters of the same word costs another token.
				// Natural-language words are shorter than that, so prose is
				// unaffected; a 2 MB run now estimates as ~250k tokens rather
				// than 1.
				if runLen%8 == 0 {
					tokens++
				}
				if b >= 'A' && b <= 'Z' && i > 0 && (text[i-1] >= 'a' && text[i-1] <= 'z') {
					// CamelCase boundary: e.g. "myVar" -> "my" + "Var"
					tokens++
				}
			}
		} else {
			// Punctuation and symbols are individual tokens or short pairs
			tokens++
			inWord = false
		}
	}
	if tokens < 1 && len(text) > 0 {
		return 1
	}
	return tokens
}

// Assembler builds prompts from registered sources.
type Assembler struct {
	sources []Source
	// MaxRunes bounds the assembled prompt in runes. Zero means unbounded.
	MaxRunes int
	// MaxTokens bounds the assembled prompt in estimated tokens. Zero means unbounded.
	MaxTokens int
}

// New returns an assembler.
func New() *Assembler { return &Assembler{} }

// Add registers a source. A duplicate name is refused: two contributors under
// one name make the assembly report ambiguous about which produced what, and
// the report is the reason this package exists.
func (a *Assembler) Add(sources ...Source) error {
	for _, s := range sources {
		if s == nil || s.Name() == "" {
			return fmt.Errorf("prompt: a source must have a name")
		}
		for _, existing := range a.sources {
			if existing.Name() == s.Name() {
				return fmt.Errorf("prompt: source %q is already registered", s.Name())
			}
		}
		a.sources = append(a.sources, s)
	}
	return nil
}

// Included records one section that made it into the prompt.
type Included struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Runes  int    `json:"runes"`
	Tokens int    `json:"tokens,omitempty"`
}

// Omitted records one that did not, and why. This is the field that keeps an
// assembled prompt honest: a section dropped for budget and a section that was
// never contributed are different failures, and both are invisible in the text.
type Omitted struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Reason string `json:"reason"`
}

// Report accounts for every section.
type Report struct {
	Included []Included `json:"included"`
	Omitted  []Omitted  `json:"omitted,omitempty"`
	// Failed names sources that errored. A prompt assembled while a
	// contributor was failing is not the prompt that was intended, and the run
	// must be able to say so.
	Failed []Omitted `json:"failed,omitempty"`
	Runes  int       `json:"runes"`
	Tokens int       `json:"tokens,omitempty"`
}

// Complete reports whether every section was contributed and included.
func (r Report) Complete() bool { return len(r.Omitted) == 0 && len(r.Failed) == 0 }

// Assemble builds the prompt and the account of how it was built.
//
// A failing source does not abort assembly. The alternative — refusing to build
// a prompt because one optional contributor could not read a file — turns a
// degraded run into no run at all. What it must never do is proceed quietly,
// which is what Report.Failed prevents.
func (a *Assembler) Assemble() (string, Report) {
	var report Report
	type entry struct {
		section Section
		source  string
	}
	var entries []entry

	for _, source := range a.sources {
		sections, err := source.Sections()
		if err != nil {
			report.Failed = append(report.Failed, Omitted{
				Source: source.Name(), Name: source.Name(),
				Reason: fmt.Sprintf("contributor failed: %v", err),
			})
			continue
		}
		for _, section := range sections {
			if strings.TrimSpace(section.Text) == "" {
				report.Omitted = append(report.Omitted, Omitted{
					Name: section.Name, Source: source.Name(), Reason: "contributed no content",
				})
				continue
			}
			entries = append(entries, entry{section: section, source: source.Name()})
		}
	}

	// Deterministic order: two runs with the same inputs must produce the same
	// bytes, or the prompt cannot be diffed across runs or cached by a provider.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].section.Order != entries[j].section.Order {
			return entries[i].section.Order < entries[j].section.Order
		}
		return entries[i].section.Name < entries[j].section.Name
	})

	var builder strings.Builder
	usedRunes := 0
	usedTokens := 0
	for _, e := range entries {
		text := strings.TrimSpace(e.section.Text)
		length := len([]rune(text))
		tokens := EstimateTokens(text)

		overRuneBudget := a.MaxRunes > 0 && usedRunes+length > a.MaxRunes
		overTokenBudget := a.MaxTokens > 0 && usedTokens+tokens > a.MaxTokens

		if overRuneBudget || overTokenBudget {
			if e.section.Essential {
				// An essential section is never trimmed. A model told it may
				// write anywhere, because the scope section did not fit, is a
				// model that will try — so the budget yields instead.
				reason := ""
				if overTokenBudget {
					reason = fmt.Sprintf("exceeded the %d-token budget but was kept: an essential section is never trimmed", a.MaxTokens)
				} else {
					reason = fmt.Sprintf("exceeded the %d-rune budget but was kept: an essential section is never trimmed", a.MaxRunes)
				}
				report.Omitted = append(report.Omitted, Omitted{
					Name: e.section.Name, Source: e.source,
					Reason: reason,
				})
			} else {
				reason := ""
				if overTokenBudget {
					reason = fmt.Sprintf("dropped: %d tokens would exceed the %d-token budget", tokens, a.MaxTokens)
				} else {
					reason = fmt.Sprintf("dropped: %d runes would exceed the %d-rune budget", length, a.MaxRunes)
				}
				report.Omitted = append(report.Omitted, Omitted{
					Name: e.section.Name, Source: e.source,
					Reason: reason,
				})
				continue
			}
		}

		if builder.Len() > 0 {
			builder.WriteString("\n\n")
		}
		builder.WriteString(text)
		usedRunes += length
		usedTokens += tokens
		report.Included = append(report.Included, Included{
			Name: e.section.Name, Source: e.source, Runes: length, Tokens: tokens,
		})
	}

	assembledText := builder.String()
	report.Runes = len([]rune(assembledText))
	report.Tokens = EstimateTokens(assembledText)
	return assembledText, report
}

// Density controls the verbosity of prompt guidance.
type Density string

const (
	DensityFull    Density = "full"
	DensityCompact Density = "compact"
)

// RouterConfig specifies routing parameters for dynamic system prompts.
type RouterConfig struct {
	Density      Density
	Provider     string
	ActiveGroups []string
}

// ConditionFunc decides whether a prompt source should be routed.
type ConditionFunc func(cfg RouterConfig) bool

// RoutedSource pairs a prompt Source with an optional ConditionFunc.
type RoutedSource struct {
	Source    Source
	Condition ConditionFunc
}

// Router filters and selects prompt sources dynamically based on context and profile.
type Router struct {
	config  RouterConfig
	sources []RoutedSource
	varying []varyingSource
}

// varyingSource is a section whose text the router computes from its own
// config, rather than one fixed at registration.
//
// It exists because density is a property of the run and not of the section: a
// caller that had to register the full and compact wordings as two Sources
// would be maintaining two parallel lists that drift, which is the shape this
// package was written to avoid. One registration that knows both wordings
// cannot drift from itself.
type varyingSource struct {
	name      string
	order     int
	essential bool
	text      func(cfg RouterConfig) string
	cond      ConditionFunc
}

// NewRouter creates a new guidance router.
func NewRouter(cfg RouterConfig) *Router {
	if cfg.Density == "" {
		if cfg.Provider == "local" {
			cfg.Density = DensityCompact
		} else {
			cfg.Density = DensityFull
		}
	}
	return &Router{config: cfg}
}

// Add adds a source with optional routing conditions.
func (r *Router) Add(src Source, conds ...ConditionFunc) {
	var cond ConditionFunc
	if len(conds) > 0 {
		cond = conds[0]
	}
	r.sources = append(r.sources, RoutedSource{Source: src, Condition: cond})
}

// Vary registers a section whose wording the router chooses from its config —
// the full text for a frontier model, a compact one for a local model whose
// context window and prefill cost make every token count.
//
// text is called once, at Sources time, with the config the router was built
// with. It must return the whole section; returning an empty string means the
// section contributes nothing and is reported as omitted, the same as any other
// empty contribution.
func (r *Router) Vary(name string, order int, essential bool, text func(cfg RouterConfig) string, conds ...ConditionFunc) {
	var cond ConditionFunc
	if len(conds) > 0 {
		cond = conds[0]
	}
	r.varying = append(r.varying, varyingSource{
		name: name, order: order, essential: essential, text: text, cond: cond,
	})
}

// Sources evaluates all conditions against the router's config and returns the active sources.
func (r *Router) Sources() []Source {
	var out []Source
	for _, rs := range r.sources {
		if rs.Condition == nil || rs.Condition(r.config) {
			out = append(out, rs.Source)
		}
	}
	for _, v := range r.varying {
		if v.cond != nil && !v.cond(r.config) {
			continue
		}
		out = append(out, Static(v.name, v.order, v.essential, v.text(r.config)))
	}
	return out
}

// Config returns the routing config this router was built with, so a caller
// that derived a budget or a density from it can report what it used.
func (r *Router) Config() RouterConfig { return r.config }

// Assemble filters active sources and builds the final system prompt.
func (r *Router) Assemble(maxTokens int) (string, Report) {
	a := New()
	a.MaxTokens = maxTokens
	for _, src := range r.Sources() {
		_ = a.Add(src)
	}
	return a.Assemble()
}

// WhenProvider matches specific provider names (e.g. "local", "anthropic").
func WhenProvider(names ...string) ConditionFunc {
	return func(cfg RouterConfig) bool {
		for _, name := range names {
			if strings.EqualFold(cfg.Provider, name) {
				return true
			}
		}
		return false
	}
}

// WhenDensity matches the density mode.
func WhenDensity(d Density) ConditionFunc {
	return func(cfg RouterConfig) bool {
		return cfg.Density == d
	}
}

// WhenGroupActive checks if any of the specified tool groups are active.
func WhenGroupActive(groups ...string) ConditionFunc {
	return func(cfg RouterConfig) bool {
		for _, g := range groups {
			for _, active := range cfg.ActiveGroups {
				if strings.EqualFold(g, active) {
					return true
				}
			}
		}
		return false
	}
}
