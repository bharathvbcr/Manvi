// Package flags is the harness's master settings registry: every feature
// toggle and tunable in one typed, self-describing place.
//
// Three rules shape the design, and each one exists because the alternative
// fails silently:
//
//   - An unknown key is an error, never a no-op. A mistyped flag in a config
//     file or environment variable that quietly does nothing is the same class
//     of defect as a check that could not run reporting success.
//
//   - A value carries its Origin. "Why is enforcement off?" must be answerable
//     without guessing which of four layers won.
//
//   - Turning a safety feature off is visible in the output it affects. A
//     Decision produced under a demoted gate says so, and names the flag. A
//     disabled check must never be indistinguishable from a passing check.
//
// Mutability is an authority question, not a convenience one: an agent that can
// switch off its own write gate has no write gate. Flags marked Safety can only
// ever be moved by a human, and only outside a running turn.
package flags

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Kind is a flag's value type.
type Kind string

const (
	KindBool     Kind = "bool"
	KindString   Kind = "string"
	KindInt      Kind = "int"
	KindDuration Kind = "duration"
	KindEnum     Kind = "enum"
)

// Mutability says who may change a flag, and when.
type Mutability string

const (
	// Startup values are fixed once Boot completes.
	Startup Mutability = "startup"
	// HumanOnly values may change at runtime, but only on human authority.
	HumanOnly Mutability = "human"
	// AgentSettable values may be changed by an agent within its own turn.
	AgentSettable Mutability = "agent"
)

// Origin records which layer supplied the value in force.
type Origin string

const (
	OriginDefault  Origin = "default"
	OriginConfig   Origin = "config"
	OriginEnv      Origin = "env"
	OriginOverride Origin = "override"
)

// Def declares one flag. Definitions are registered at boot so the full set is
// discoverable — `manvi flags` is generated from these, never hand-kept.
type Def struct {
	// Safest is the value of this flag that represents the strictest posture,
	// when that is not the same as Default.
	//
	// Weakened compares against this rather than against Default, because the
	// two came apart the moment a safety flag shipped with a deliberately
	// relaxed default. harness.posture defaults to dev so the harness is
	// usable while it is being built; strict is still the safe value, and a
	// report that called dev "unchanged, therefore fine" would hide the single
	// most important fact about the run. Empty means Default is the safe value,
	// which is true of every other flag in the catalogue.
	Safest string

	Key         string
	Kind        Kind
	Default     string
	Description string
	// Values enumerates the legal values for KindEnum.
	Values []string
	// Min and Max bound a KindInt value, inclusive, and are checked wherever a
	// value enters — the config file, the environment, and Set — so an
	// out-of-range setting is refused by the layer that supplied it instead of
	// being accepted here and clamped later by whichever consumer thought of
	// it. A consumer that clamps silently turns "the operator asked for 100000
	// concurrent children" into a number nobody chose and nothing reports.
	//
	// A Max of zero means unbounded, which is how every flag that has never
	// needed a ceiling keeps its current contract: the range check runs only
	// for a flag that declares one. A setting whose real ceiling is zero is a
	// constant, not a setting, so nothing in the catalogue needs that spelling.
	Min, Max int
	// Mutable says who may change this flag after boot.
	Mutable Mutability
	// Safety marks a flag whose off/demoted state weakens a gate. Safety flags
	// are never agent-settable, and consumers must surface their state in the
	// decisions they affect.
	Safety bool
}

// Value is a resolved flag value and where it came from.
type Value struct {
	Key    string
	Raw    string
	Origin Origin
}

// Registry holds definitions and layered values.
type Registry struct {
	mu        sync.RWMutex
	defs      map[string]Def
	order     []string
	config    map[string]string
	env       map[string]string
	overrides map[string]string
	sealed    bool
	// envLookup is injected so tests do not mutate the process environment.
	envLookup func(string) (string, bool)
}

// New returns an empty registry reading environment variables from the process.
func New() *Registry {
	return &Registry{
		defs:      map[string]Def{},
		config:    map[string]string{},
		env:       map[string]string{},
		overrides: map[string]string{},
		envLookup: os.LookupEnv,
	}
}

// envPrefix namespaces every variable this harness reads. legacyEnvPrefix is
// what it was before the harness was named manvi.
const (
	envPrefix       = "MANVI_"
	legacyEnvPrefix = "DEVHARNESS_"
)

// EnvKey is the environment variable that sets a flag: policy.file.mode is
// read from MANVI_POLICY_FILE_MODE.
func EnvKey(key string) string {
	return envPrefix + strings.ToUpper(strings.NewReplacer(".", "_", "-", "_").Replace(key))
}

// Rename pairs a variable's retired name with the one that replaced it.
type Rename struct{ Old, New string }

// StaleEnv reports variables in environ that still carry the retired prefix.
//
// The rename would otherwise be silent: an operator who set
// DEVHARNESS_POLICY_FILE_MODE would get the default instead, with the registry
// reporting the value's origin as "default" and nothing anywhere reporting that
// an instruction had been dropped. Safety flags are set this way, so the failure
// mode is a run that is weaker than the environment asked for and describes
// itself as strict. The caller turns this into an error at startup rather than
// a warning, because a warning about a setting that is already not applying is
// a warning about a decision already made.
//
// It takes the environment rather than reading it so a test does not have to
// mutate the process.
func StaleEnv(environ []string) []Rename {
	var out []Rename
	for _, kv := range environ {
		key, _, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(key, legacyEnvPrefix) {
			continue
		}
		out = append(out, Rename{Old: key, New: envPrefix + strings.TrimPrefix(key, legacyEnvPrefix)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Old < out[j].Old })
	return out
}

// Define registers a flag. Redefining a key, or declaring an agent-settable
// safety flag, is a programming error and fails at boot.
func (r *Registry) Define(defs ...Def) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return fmt.Errorf("flags: registry is sealed; define flags before Seal")
	}
	for _, d := range defs {
		if d.Key == "" {
			return fmt.Errorf("flags: definition with empty key")
		}
		if _, dup := r.defs[d.Key]; dup {
			return fmt.Errorf("flags: %q is already defined", d.Key)
		}
		if d.Safety && d.Mutable == AgentSettable {
			return fmt.Errorf("flags: %q is a safety flag and cannot be agent-settable", d.Key)
		}
		// Mutability is a string, so omitting it is not a compile error — it is
		// the zero value, and the authority switch in Set matched none of the
		// three cases and fell through to the assignment. "I forgot to say who
		// may change this" therefore meant "anyone may, including the agent",
		// which is the one default this package cannot have.
		switch d.Mutable {
		case Startup, HumanOnly, AgentSettable:
		case "":
			return fmt.Errorf("flags: %q declares no mutability; say startup, human, or agent", d.Key)
		default:
			return fmt.Errorf("flags: %q declares unknown mutability %q", d.Key, d.Mutable)
		}
		if d.Kind == KindEnum && len(d.Values) == 0 {
			return fmt.Errorf("flags: enum %q declares no values", d.Key)
		}
		// Bounds on anything but an int would be declared, reported by nothing,
		// and enforced nowhere — a limit an operator can read in the catalogue
		// and that does not hold.
		if d.Kind != KindInt && (d.Min != 0 || d.Max != 0) {
			return fmt.Errorf("flags: %q declares bounds %d..%d but is a %s, not an int", d.Key, d.Min, d.Max, d.Kind)
		}
		if d.Max != 0 && d.Min > d.Max {
			return fmt.Errorf("flags: %q declares an empty range %d..%d", d.Key, d.Min, d.Max)
		}
		if err := validate(d, d.Default); err != nil {
			return fmt.Errorf("flags: default for %q: %w", d.Key, err)
		}
		r.defs[d.Key] = d
		r.order = append(r.order, d.Key)
	}
	sort.Strings(r.order)
	return nil
}

// isHarnessNamespace reports whether key belongs to a namespace defined by the harness.
// Unknown keys in harness namespaces are typos that must fail; keys in foreign or shared
// namespaces (e.g. DevCouncil core settings in .devcouncil/config.yaml) are preserved.
func (r *Registry) isHarnessNamespace(key string) bool {
	for definedKey := range r.defs {
		if idx := strings.IndexByte(definedKey, '.'); idx >= 0 {
			prefix := definedKey[:idx+1]
			if strings.HasPrefix(key, prefix) {
				return true
			}
		} else if key == definedKey || strings.HasPrefix(key, definedKey+".") {
			return true
		}
	}
	return false
}

func resolveFlagAlias(key string) string {
	switch key {
	case "verification.rigor.enabled":
		return "verify.rigor.enabled"
	case "verification.diff_coverage.enforce":
		return "verify.diff_coverage.enforce"
	default:
		return ""
	}
}

// LoadConfig applies a config-file layer. Every harness key must be defined and every
// value must be legal — a typo in harness settings is reported, never ignored.
// Shared repository settings outside harness namespaces are passed over safely.
func (r *Registry) LoadConfig(values map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var unknown []string
	toSet := map[string]string{}

	for k, v := range values {
		targetKey := k
		d, ok := r.defs[targetKey]
		if !ok {
			if alias := resolveFlagAlias(k); alias != "" {
				if ad, aok := r.defs[alias]; aok {
					d = ad
					targetKey = alias
					ok = true
				}
			}
		}

		if !ok {
			if r.isHarnessNamespace(k) {
				unknown = append(unknown, k)
			}
			continue
		}

		if err := validate(d, normalize(v)); err != nil {
			return fmt.Errorf("flags: config %q: %w", k, err)
		}
		toSet[targetKey] = normalize(v)
	}

	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("flags: unknown key(s) in config: %s", strings.Join(unknown, ", "))
	}
	for k, v := range toSet {
		r.config[k] = v
	}
	return nil
}

// LoadEnv reads the environment for every defined flag. An environment
// variable that fails validation is an error, not a silently ignored value.
func (r *Registry) LoadEnv() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, d := range r.defs {
		raw, ok := r.envLookup(EnvKey(key))
		if !ok {
			continue
		}
		raw = normalize(raw)
		if err := validate(d, raw); err != nil {
			return fmt.Errorf("flags: %s: %w", EnvKey(key), err)
		}
		r.env[key] = raw
	}
	return nil
}

// Seal freezes Startup flags. Call it once boot is complete.
func (r *Registry) Seal() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sealed = true
}

// Authority is who is attempting a runtime change.
type Authority string

const (
	Human Authority = "human"
	Agent Authority = "agent"
)

// Set changes a flag at runtime under the given authority. It refuses changes
// the flag's Mutability does not permit, so an agent cannot widen its own
// permissions by writing to the registry.
func (r *Registry) Set(auth Authority, key, raw string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.defs[key]
	if !ok {
		return fmt.Errorf("flags: unknown key %q", key)
	}
	raw = normalize(raw)
	if err := validate(d, raw); err != nil {
		return fmt.Errorf("flags: %q: %w", key, err)
	}
	switch d.Mutable {
	case Startup:
		if r.sealed {
			return fmt.Errorf("flags: %q is startup-only and the registry is sealed", key)
		}
	case HumanOnly:
		if auth != Human {
			return fmt.Errorf("flags: %q may only be changed on human authority (attempted by %s)", key, auth)
		}
	case AgentSettable:
		// Either authority may set it.
	default:
		// Define refuses this at boot; this is the same rule at the other end,
		// for a registry assembled by a path that did not go through Define.
		// An unrecognised authority must never resolve to "permitted".
		return fmt.Errorf("flags: %q declares mutability %q, which grants no authority; refusing to set it", key, d.Mutable)
	}
	r.overrides[key] = raw
	return nil
}

// Lookup resolves a flag through the layers: override, env, config, default.
func (r *Registry) Lookup(key string) (Value, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.defs[key]
	if !ok {
		return Value{}, fmt.Errorf("flags: unknown key %q", key)
	}
	for _, layer := range []struct {
		src    map[string]string
		origin Origin
	}{
		{r.overrides, OriginOverride},
		{r.env, OriginEnv},
		{r.config, OriginConfig},
	} {
		if raw, ok := layer.src[key]; ok {
			return Value{Key: key, Raw: raw, Origin: layer.origin}, nil
		}
	}
	return Value{Key: key, Raw: d.Default, Origin: OriginDefault}, nil
}

// Bool resolves a bool flag. An undefined key returns false and an error; call
func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "t", "true", "yes", "on", "y":
		return true, nil
	case "0", "f", "false", "no", "off", "n":
		return false, nil
	default:
		return strconv.ParseBool(s)
	}
}

// sites that ignore the error get the safe value, not a permissive one.
func (r *Registry) Bool(key string) (bool, Origin, error) {
	v, err := r.Lookup(key)
	if err != nil {
		return false, OriginDefault, err
	}
	b, err := parseBool(v.Raw)
	if err != nil {
		return false, v.Origin, fmt.Errorf("flags: %q is not a bool: %q", key, v.Raw)
	}
	return b, v.Origin, nil
}

// String resolves a string or enum flag.
func (r *Registry) String(key string) (string, Origin, error) {
	v, err := r.Lookup(key)
	if err != nil {
		return "", OriginDefault, err
	}
	return v.Raw, v.Origin, nil
}

// Int resolves an int flag.
func (r *Registry) Int(key string) (int, Origin, error) {
	v, err := r.Lookup(key)
	if err != nil {
		return 0, OriginDefault, err
	}
	n, err := strconv.Atoi(v.Raw)
	if err != nil {
		return 0, v.Origin, fmt.Errorf("flags: %q is not an int: %q", key, v.Raw)
	}
	return n, v.Origin, nil
}

// Duration resolves a duration flag.
func (r *Registry) Duration(key string) (time.Duration, Origin, error) {
	v, err := r.Lookup(key)
	if err != nil {
		return 0, OriginDefault, err
	}
	d, err := time.ParseDuration(v.Raw)
	if err != nil {
		return 0, v.Origin, fmt.Errorf("flags: %q is not a duration: %q", key, v.Raw)
	}
	return d, v.Origin, nil
}

// Def returns a flag's definition.
func (r *Registry) Def(key string) (Def, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.defs[key]
	return d, ok
}

// Keys lists every defined flag, sorted.
func (r *Registry) Keys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// Weakened lists every safety flag that is not at its safest value, with the
// origin responsible. This is what a run report prints so a green result
// produced under a relaxed configuration cannot be mistaken for a strict one.
//
// Note that it reports the value, not the change: a safety flag sitting on a
// relaxed *default* is still listed. Nothing having been touched is not the
// same as nothing having been relaxed.
func (r *Registry) Weakened() []Value {
	r.mu.RLock()
	keys := append([]string(nil), r.order...)
	defs := make(map[string]Def, len(r.defs))
	for k, d := range r.defs {
		defs[k] = d
	}
	r.mu.RUnlock()

	var out []Value
	for _, k := range keys {
		d := defs[k]
		if !d.Safety {
			continue
		}
		safest := d.Safest
		if safest == "" {
			safest = d.Default
		}
		v, err := r.Lookup(k)
		if err != nil || v.Raw == safest {
			continue
		}
		out = append(out, v)
	}
	return out
}

// normalize is what a value looks like once it is in the registry.
//
// Surrounding whitespace is never meaningful in this catalogue — every value is
// a model id, an address, an enum, or a number — and leaving it in produced an
// asymmetry that read as a bug from either side: MANVI_LLM_LOCAL_TEMPERATURE=" 0.7"
// was accepted because its consumer trimmed before parsing, while
// MANVI_LLM_LOCAL_CORE_TOOLS_ONLY=" true" was a hard startup error because
// KindBool parsed the raw string. A trailing space picked up from a shell
// heredoc or a YAML value should not decide which of those an operator gets.
//
// Trimming here rather than in each accessor means Raw is the canonical value:
// what validate checked, what Lookup reports, and what every consumer reads are
// the same string. A whitespace-only value therefore becomes empty, which is
// what the optional sampling flags already document as "unset, omit the field".
func normalize(raw string) string { return strings.TrimSpace(raw) }

func validate(d Def, raw string) error {
	switch d.Kind {
	case KindBool:
		if _, err := parseBool(raw); err != nil {
			return fmt.Errorf("expected a bool, got %q", raw)
		}
	case KindInt:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("expected an int, got %q", raw)
		}
		// The range is checked only for a flag that declares a ceiling; see
		// Def.Max. A value outside it is refused rather than clamped, because
		// the two are not the same answer: a refusal names the setting and the
		// limit, and a clamp runs the harness on a number the operator did not
		// choose and cannot see in the flag table.
		if d.Max != 0 && (n < d.Min || n > d.Max) {
			return fmt.Errorf("expected an int in %d..%d, got %d", d.Min, d.Max, n)
		}
	case KindDuration:
		if _, err := time.ParseDuration(raw); err != nil {
			return fmt.Errorf("expected a duration, got %q", raw)
		}
	case KindEnum:
		for _, v := range d.Values {
			if v == raw {
				return nil
			}
		}
		return fmt.Errorf("expected one of [%s], got %q", strings.Join(d.Values, ", "), raw)
	case KindString:
	default:
		return fmt.Errorf("unknown kind %q", d.Kind)
	}
	return nil
}
