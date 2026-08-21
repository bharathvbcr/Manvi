package plugin

import (
	"errors"
	"strings"
	"testing"
)

// fake is a test plugin that records its registrations into a shared trace.
type fake struct {
	name     string
	provides []string
	deps     []string
	trace    *[]string
	applyErr error
	// resolve is a key the plugin tries to Resolve during Apply.
	resolve string
}

func (f *fake) Name() string       { return f.name }
func (f *fake) Provides() []string { return f.provides }
func (f *fake) Deps() []string     { return f.deps }

func (f *fake) Apply(cx *Ctx) (Dispose, error) {
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	if f.resolve != "" {
		if _, err := Resolve[string](cx, f.resolve); err != nil {
			return nil, err
		}
	}
	for _, key := range f.provides {
		if err := cx.Provide(key, f.name+":service"); err != nil {
			return nil, err
		}
		cx.Effect(func() error {
			*f.trace = append(*f.trace, "unwind:"+f.name)
			return nil
		})
	}
	*f.trace = append(*f.trace, "apply:"+f.name)
	return func() error {
		*f.trace = append(*f.trace, "dispose:"+f.name)
		return nil
	}, nil
}

// TestBootOrderIsIndependentOfConfigOrder is the Phase 1 gate: two plugins,
// one depending on the other, boot in dependency order whichever way they are
// listed.
func TestBootOrderIsIndependentOfConfigOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		order func(a, b Plugin) []Plugin
	}{
		{"dependency first", func(a, b Plugin) []Plugin { return []Plugin{a, b} }},
		{"dependent first", func(a, b Plugin) []Plugin { return []Plugin{b, a} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var trace []string
			base := &fake{name: "base", provides: []string{"cx.base"}, trace: &trace}
			user := &fake{
				name: "user", provides: []string{"cx.user"},
				deps: []string{"cx.base"}, resolve: "cx.base", trace: &trace,
			}

			reg := New()
			if err := reg.Boot(tc.order(base, user)...); err != nil {
				t.Fatalf("boot: %v", err)
			}
			got := strings.Join(reg.LoadOrder(), ",")
			if got != "base,user" {
				t.Fatalf("load order = %q, want \"base,user\"", got)
			}
		})
	}
}

// TestUnloadUnwindsDependentsInReverse is the second half of the Phase 1 gate:
// unloading a dependency unwinds the dependent too, in reverse load order.
func TestUnloadUnwindsDependentsInReverse(t *testing.T) {
	var trace []string
	base := &fake{name: "base", provides: []string{"cx.base"}, trace: &trace}
	user := &fake{
		name: "user", provides: []string{"cx.user"},
		deps: []string{"cx.base"}, resolve: "cx.base", trace: &trace,
	}

	reg := New()
	if err := reg.Boot(user, base); err != nil {
		t.Fatalf("boot: %v", err)
	}
	trace = nil

	if err := reg.Unload("base"); err != nil {
		t.Fatalf("unload: %v", err)
	}

	want := "dispose:user,unwind:user,dispose:base,unwind:base"
	if got := strings.Join(trace, ","); got != want {
		t.Fatalf("teardown trace = %q, want %q", got, want)
	}
	if reg.Has("cx.base") || reg.Has("cx.user") {
		t.Fatal("services survived unload")
	}
	if len(reg.LoadOrder()) != 0 {
		t.Fatalf("load order = %v, want empty", reg.LoadOrder())
	}
}

func TestCycleIsABootErrorNotAHang(t *testing.T) {
	var trace []string
	a := &fake{name: "a", provides: []string{"cx.a"}, deps: []string{"cx.b"}, trace: &trace}
	b := &fake{name: "b", provides: []string{"cx.b"}, deps: []string{"cx.a"}, trace: &trace}

	err := New().Boot(a, b)
	if err == nil {
		t.Fatal("expected a cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("error = %v, want it to name the cycle", err)
	}
}

func TestMissingProviderIsABootError(t *testing.T) {
	var trace []string
	orphan := &fake{name: "orphan", provides: []string{"cx.orphan"}, deps: []string{"cx.absent"}, trace: &trace}

	err := New().Boot(orphan)
	if err == nil || !strings.Contains(err.Error(), "cx.absent") {
		t.Fatalf("error = %v, want it to name the unprovided service", err)
	}
}

func TestProvidingAnUndeclaredServiceIsAnError(t *testing.T) {
	smuggler := pluginFunc{
		name:     "smuggler",
		provides: []string{"cx.declared"},
		apply: func(cx *Ctx) (Dispose, error) {
			if err := cx.Provide("cx.declared", "ok"); err != nil {
				return nil, err
			}
			return nil, cx.Provide("cx.smuggled", "surprise")
		},
	}
	err := New().Boot(smuggler)
	if err == nil || !strings.Contains(err.Error(), "undeclared service") {
		t.Fatalf("error = %v, want an undeclared-service error", err)
	}
}

func TestResolvingAnUndeclaredDependencyIsAnError(t *testing.T) {
	var trace []string
	base := &fake{name: "base", provides: []string{"cx.base"}, trace: &trace}
	// Declares no deps but reaches for cx.base anyway.
	reacher := pluginFunc{
		name:     "reacher",
		provides: []string{"cx.reacher"},
		apply: func(cx *Ctx) (Dispose, error) {
			_, err := Resolve[string](cx, "cx.base")
			return nil, err
		},
	}
	err := New().Boot(base, reacher)
	if err == nil || !strings.Contains(err.Error(), "undeclared dependency") {
		t.Fatalf("error = %v, want an undeclared-dependency error", err)
	}
}

func TestFailedBootLeavesNothingInstalled(t *testing.T) {
	var trace []string
	base := &fake{name: "base", provides: []string{"cx.base"}, trace: &trace}
	broken := &fake{
		name: "broken", provides: []string{"cx.broken"}, deps: []string{"cx.base"},
		trace: &trace, applyErr: errors.New("boom"),
	}

	reg := New()
	if err := reg.Boot(base, broken); err == nil {
		t.Fatal("expected boot to fail")
	}
	if reg.Has("cx.base") {
		t.Fatal("cx.base survived a failed boot")
	}
	if len(reg.LoadOrder()) != 0 {
		t.Fatalf("load order = %v, want empty after failed boot", reg.LoadOrder())
	}
}

func TestDuplicateServiceProviderIsABootError(t *testing.T) {
	var trace []string
	a := &fake{name: "a", provides: []string{"cx.dup"}, trace: &trace}
	b := &fake{name: "b", provides: []string{"cx.dup"}, trace: &trace}

	err := New().Boot(a, b)
	if err == nil || !strings.Contains(err.Error(), "cx.dup") {
		t.Fatalf("error = %v, want a duplicate-provider error", err)
	}
}

// pluginFunc adapts a bare function into a Plugin for tests that need to drive
// Apply directly.
type pluginFunc struct {
	name     string
	provides []string
	deps     []string
	apply    func(*Ctx) (Dispose, error)
}

func (p pluginFunc) Name() string                   { return p.name }
func (p pluginFunc) Provides() []string             { return p.provides }
func (p pluginFunc) Deps() []string                 { return p.deps }
func (p pluginFunc) Apply(cx *Ctx) (Dispose, error) { return p.apply(cx) }
