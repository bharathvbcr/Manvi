// Package plugin is the harness kernel: a service registry resolved by key,
// boot ordered by declared service requirements, and teardown that unwinds
// registrations in reverse.
//
// The three invariants that make the dependency graph real rather than
// decorative, each enforced at runtime rather than by convention:
//
//   - A plugin may only Provide a key it declared in Provides().
//   - A plugin may only Resolve a key it declared in Deps().
//   - Unloading a plugin unwinds its dependents first, in reverse load order.
//
// Without the first two the graph is a comment; a plugin could smuggle in a
// service nobody ordered, or reach one it never declared, and boot order would
// be correct by luck.
package plugin

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Dispose unwinds one registration. Every effect returns one.
type Dispose func() error

// Plugin is a service with declared requirements. Apply installs its
// registrations and returns a disposer that removes them.
type Plugin interface {
	// Name identifies the plugin for load order and teardown.
	Name() string
	// Provides lists the service keys this plugin registers, e.g. "cx.llm".
	Provides() []string
	// Deps lists the service keys that must exist before Apply runs.
	Deps() []string
	// Apply installs the plugin against the registry context.
	Apply(*Ctx) (Dispose, error)
}

// Ctx is a plugin's handle on the registry. It carries the owning plugin's
// declarations so Provide and Resolve can be checked against them.
type Ctx struct {
	reg      *Registry
	owner    string
	provides map[string]bool
	deps     map[string]bool
	effects  []Dispose
}

// Provide registers a service under key. The key must have been declared in
// the plugin's Provides(), and must not already be held by another plugin.
func (c *Ctx) Provide(key string, svc any) error {
	if !c.provides[key] {
		return fmt.Errorf("plugin %q provides undeclared service %q: add it to Provides()", c.owner, key)
	}
	if existing, ok := c.reg.owners[key]; ok {
		return fmt.Errorf("service %q already provided by plugin %q", key, existing)
	}
	c.reg.services[key] = svc
	c.reg.owners[key] = c.owner
	c.effects = append(c.effects, func() error {
		delete(c.reg.services, key)
		delete(c.reg.owners, key)
		return nil
	})
	return nil
}

// Effect records a reversible registration. Effects unwind LIFO within a
// plugin, and plugins unwind in reverse load order.
func (c *Ctx) Effect(d Dispose) {
	if d != nil {
		c.effects = append(c.effects, d)
	}
}

// Owner is the name of the plugin this context belongs to.
func (c *Ctx) Owner() string { return c.owner }

// Resolve returns the service registered under key, typed as T.
//
// It is a package-level function rather than a method because Go does not
// allow type parameters on methods.
func Resolve[T any](c *Ctx, key string) (T, error) {
	var zero T
	if !c.deps[key] {
		return zero, fmt.Errorf("plugin %q resolves undeclared dependency %q: add it to Deps()", c.owner, key)
	}
	raw, ok := c.reg.services[key]
	if !ok {
		return zero, fmt.Errorf("service %q is not registered", key)
	}
	typed, ok := raw.(T)
	if !ok {
		return zero, fmt.Errorf("service %q is %T, not the requested type", key, raw)
	}
	return typed, nil
}

type loaded struct {
	name    string
	deps    []string
	dispose Dispose
	effects []Dispose
}

// Registry holds services and the plugins that provided them.
type Registry struct {
	services map[string]any
	owners   map[string]string
	order    []loaded
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		services: map[string]any{},
		owners:   map[string]string{},
	}
}

// Has reports whether a service key is registered.
func (r *Registry) Has(key string) bool {
	_, ok := r.services[key]
	return ok
}

// LoadOrder returns plugin names in the order they were applied.
func (r *Registry) LoadOrder() []string {
	names := make([]string, 0, len(r.order))
	for _, l := range r.order {
		names = append(names, l.name)
	}
	return names
}

// Boot applies plugins in dependency order, independent of the order given.
// A dependency cycle is a boot error, not a hang. A dependency no plugin
// provides is a boot error, not a nil service discovered at first call.
func (r *Registry) Boot(plugins ...Plugin) error {
	order, err := topoSort(plugins, r)
	if err != nil {
		return err
	}
	for _, p := range order {
		if err := r.apply(p); err != nil {
			// Unwind what booted so a failed boot leaves nothing installed.
			_ = r.Close()
			return fmt.Errorf("plugin %q failed to apply: %w", p.Name(), err)
		}
	}
	return nil
}

func (r *Registry) apply(p Plugin) error {
	cx := &Ctx{
		reg:      r,
		owner:    p.Name(),
		provides: toSet(p.Provides()),
		deps:     toSet(p.Deps()),
	}
	dispose, err := p.Apply(cx)
	if err != nil {
		// Roll back partial effects from the failed Apply.
		unwind(cx.effects)
		return err
	}
	for _, key := range p.Provides() {
		if _, ok := r.services[key]; !ok {
			unwind(cx.effects)
			return fmt.Errorf("declared service %q was never provided", key)
		}
	}
	r.order = append(r.order, loaded{
		name:    p.Name(),
		deps:    p.Deps(),
		dispose: dispose,
		effects: cx.effects,
	})
	return nil
}

// Unload removes one plugin and every plugin that transitively depends on it,
// unwinding in reverse load order so a dependent never outlives its dependency.
func (r *Registry) Unload(name string) error {
	idx := -1
	for i, l := range r.order {
		if l.name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("plugin %q is not loaded", name)
	}

	doomed := map[string]bool{name: true}
	// Load order is topological, so one forward pass reaches every dependent.
	for _, l := range r.order[idx:] {
		for _, dep := range l.deps {
			if doomed[r.owners[dep]] {
				doomed[l.name] = true
				break
			}
		}
	}

	var errs []error
	kept := make([]loaded, 0, len(r.order))
	for i := len(r.order) - 1; i >= 0; i-- {
		l := r.order[i]
		if !doomed[l.name] {
			continue
		}
		if l.dispose != nil {
			if err := l.dispose(); err != nil {
				errs = append(errs, fmt.Errorf("dispose %q: %w", l.name, err))
			}
		}
		if err := unwind(l.effects); err != nil {
			errs = append(errs, fmt.Errorf("unwind %q: %w", l.name, err))
		}
	}
	for _, l := range r.order {
		if !doomed[l.name] {
			kept = append(kept, l)
		}
	}
	r.order = kept
	return errors.Join(errs...)
}

// Close unwinds every loaded plugin in reverse load order.
func (r *Registry) Close() error {
	var errs []error
	for i := len(r.order) - 1; i >= 0; i-- {
		l := r.order[i]
		if l.dispose != nil {
			if err := l.dispose(); err != nil {
				errs = append(errs, fmt.Errorf("dispose %q: %w", l.name, err))
			}
		}
		if err := unwind(l.effects); err != nil {
			errs = append(errs, fmt.Errorf("unwind %q: %w", l.name, err))
		}
	}
	r.order = nil
	return errors.Join(errs...)
}

// unwind runs disposers LIFO, collecting every error rather than stopping at
// the first — a failed disposer must not strand the ones behind it.
func unwind(effects []Dispose) error {
	var errs []error
	for i := len(effects) - 1; i >= 0; i-- {
		if err := effects[i](); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// topoSort orders plugins so every dependency is applied before its dependent.
// Ties break on plugin name so boot order is deterministic across runs.
func topoSort(plugins []Plugin, r *Registry) ([]Plugin, error) {
	byName := make(map[string]Plugin, len(plugins))
	providerOf := map[string]string{}
	for _, p := range plugins {
		if _, dup := byName[p.Name()]; dup {
			return nil, fmt.Errorf("duplicate plugin name %q", p.Name())
		}
		byName[p.Name()] = p
		for _, key := range p.Provides() {
			if prior, ok := providerOf[key]; ok {
				return nil, fmt.Errorf("service %q provided by both %q and %q", key, prior, p.Name())
			}
			providerOf[key] = p.Name()
		}
	}

	indegree := make(map[string]int, len(plugins))
	dependents := make(map[string][]string, len(plugins))
	for _, p := range plugins {
		indegree[p.Name()] += 0
		for _, dep := range p.Deps() {
			provider, ok := providerOf[dep]
			if !ok {
				// Already satisfied by an earlier Boot call on this registry.
				if r != nil && r.Has(dep) {
					continue
				}
				return nil, fmt.Errorf("plugin %q requires service %q, which no plugin provides", p.Name(), dep)
			}
			if provider == p.Name() {
				continue
			}
			dependents[provider] = append(dependents[provider], p.Name())
			indegree[p.Name()]++
		}
	}

	ready := make([]string, 0, len(plugins))
	for name, deg := range indegree {
		if deg == 0 {
			ready = append(ready, name)
		}
	}
	sort.Strings(ready)

	out := make([]Plugin, 0, len(plugins))
	for len(ready) > 0 {
		name := ready[0]
		ready = ready[1:]
		out = append(out, byName[name])
		next := append([]string(nil), dependents[name]...)
		sort.Strings(next)
		for _, dependent := range next {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = append(ready, dependent)
				sort.Strings(ready)
			}
		}
	}

	if len(out) != len(plugins) {
		stuck := make([]string, 0)
		for name, deg := range indegree {
			if deg > 0 {
				stuck = append(stuck, name)
			}
		}
		sort.Strings(stuck)
		return nil, fmt.Errorf("dependency cycle among plugins: %s", strings.Join(stuck, ", "))
	}
	return out, nil
}

func toSet(keys []string) map[string]bool {
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[k] = true
	}
	return set
}
