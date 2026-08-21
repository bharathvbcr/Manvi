package tools

import (
	"context"
	"strings"
	"testing"

	"manvi/core/bus"
	"manvi/llm"
)

func reg(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry(bus.New())
	add := func(name string, readOnly, extended bool) {
		if err := r.Register(Tool{
			Schema:   llm.ToolSchema{Name: name, InputSchema: []byte(`{"type":"object"}`)},
			Handler:  func(context.Context, Call) Result { return Result{} },
			ReadOnly: readOnly, Extended: extended,
		}); err != nil {
			t.Fatal(err)
		}
	}
	add("core_read", true, false)
	add("core_write", false, false)
	add("ext_read", true, true)
	add("ext_write", false, true)
	return r
}

// TestUnsatisfiedInFindsAProfileThatStrandsATool is the mechanism behind the
// dead end `llm.local.core_tools_only` shipped with: a core tool whose only
// path to success ran through an Extended one, so narrowing the offer removed
// the recovery and left the tool that needed it.
//
// It is checked over declarations rather than over a list of tool pairs. A list
// beside the registry is true when it is written and silently wrong the next
// time a tool is added, which is exactly how the original went unnoticed.
func TestUnsatisfiedInFindsAProfileThatStrandsATool(t *testing.T) {
	r := NewRegistry(bus.New())
	add := func(name string, extended bool, requires ...string) {
		if err := r.Register(Tool{
			Schema:   llm.ToolSchema{Name: name, InputSchema: []byte(`{"type":"object"}`)},
			Handler:  func(context.Context, Call) Result { return Result{} },
			Extended: extended, Requires: requires,
		}); err != nil {
			t.Fatal(err)
		}
	}
	add("verify", false, "checkout")  // core, needs a lease
	add("checkout", true)             // the only tool that takes one — dropped by the core profile
	add("stray", false, "not_a_tool") // a prerequisite that does not exist

	gaps := r.UnsatisfiedIn(r.CoreSchemas())
	if len(gaps) != 2 {
		t.Fatalf("gaps = %v, want the stranded tool and the unregistered prerequisite", gaps)
	}
	if !strings.Contains(gaps[0], "stray") || !strings.Contains(gaps[0], "not_a_tool") {
		t.Errorf("a prerequisite naming no registered tool must be reported: %q", gaps[0])
	}
	if !strings.Contains(gaps[1], "verify") || !strings.Contains(gaps[1], "checkout") {
		t.Errorf("a core tool stranded by the core profile must be reported: %q", gaps[1])
	}

	// The full surface offers checkout, so only the typo survives there. A
	// prerequisite that names nothing is wrong in every profile.
	full := r.UnsatisfiedIn(r.Schemas())
	if len(full) != 1 || !strings.Contains(full[0], "not_a_tool") {
		t.Fatalf("full surface gaps = %v, want only the unregistered prerequisite", full)
	}

	// And a registry whose tools declare nothing is trivially closed, so the
	// check never invents work for a surface that has no prerequisites.
	if gaps := reg(t).UnsatisfiedIn(reg(t).CoreSchemas()); len(gaps) != 0 {
		t.Errorf("a registry with no declared prerequisites reported %v", gaps)
	}
}

func names(schemas []llm.ToolSchema) map[string]bool {
	out := map[string]bool{}
	for _, s := range schemas {
		out[s.Name] = true
	}
	return out
}

func TestProfilesSelectTheRightSubsets(t *testing.T) {
	r := reg(t)
	for _, tc := range []struct {
		label  string
		got    map[string]bool
		want   []string
		absent []string
	}{
		{"all", names(r.Schemas()), []string{"core_read", "core_write", "ext_read", "ext_write"}, nil},
		{"core", names(r.CoreSchemas()), []string{"core_read", "core_write"}, []string{"ext_read", "ext_write"}},
		{"read-only", names(r.ReadOnlySchemas()), []string{"core_read", "ext_read"}, []string{"core_write", "ext_write"}},
		{"core read-only", names(r.CoreReadOnlySchemas()), []string{"core_read"}, []string{"ext_read", "core_write", "ext_write"}},
	} {
		for _, want := range tc.want {
			if !tc.got[want] {
				t.Errorf("%s: missing %s", tc.label, want)
			}
		}
		for _, absent := range tc.absent {
			if tc.got[absent] {
				t.Errorf("%s: unexpectedly includes %s", tc.label, absent)
			}
		}
	}
}

// Subsetting must not change dispatch. A tool omitted from the offered schemas
// is still registered, so a call that arrives for it still runs rather than
// reporting "unknown tool" — which would be a different and much more confusing
// failure.
func TestOmittingAToolFromTheOfferDoesNotUnregisterIt(t *testing.T) {
	r := reg(t)
	if !r.Has("ext_write") {
		t.Fatal("an extended tool was unregistered rather than merely unoffered")
	}
	result := r.Run(context.Background(), Call{Name: "ext_write"})
	if result.IsError {
		t.Fatalf("an extended tool failed to dispatch: %s", result.Text)
	}
}

func TestEveryProfileIsSortedForAStablePrompt(t *testing.T) {
	r := reg(t)
	for _, schemas := range [][]llm.ToolSchema{
		r.Schemas(), r.CoreSchemas(), r.ReadOnlySchemas(), r.CoreReadOnlySchemas(),
	} {
		for i := 1; i < len(schemas); i++ {
			if schemas[i-1].Name > schemas[i].Name {
				t.Fatalf("unsorted schemas break byte-identical requests: %v", schemas)
			}
		}
	}
}

// A subset is a smaller tool surface, not a smaller menu.
//
// Hiding a schema only discourages a model from naming a tool; Run dispatches
// by name against whatever is registered, so a tool that is merely unadvertised
// still executes when named. A restricted child needs the tool to be absent.
func TestSubsetRemovesTheToolBodyNotJustItsSchema(t *testing.T) {
	b := bus.New()
	reg := NewRegistry(b)
	mustRegister := func(name string, readOnly bool) {
		t.Helper()
		if err := reg.Register(Tool{
			Schema:   llm.ToolSchema{Name: name},
			ReadOnly: readOnly,
			Handler: func(context.Context, Call) Result {
				return Result{Text: name + " ran"}
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	mustRegister("read_thing", true)
	mustRegister("write_thing", false)

	child := reg.Subset(func(tool Tool) bool { return tool.ReadOnly })

	if !child.Has("read_thing") {
		t.Error("the subset dropped a tool that passed the predicate")
	}
	if child.Has("write_thing") {
		t.Error("the subset kept a tool that failed the predicate")
	}

	// The property that matters: naming the excluded tool does not run it.
	res := child.Run(context.Background(), Call{ID: "1", Name: "write_thing"})
	if !res.IsError {
		t.Fatalf("a tool excluded from the subset still executed: %q", res.Text)
	}
	if !strings.Contains(res.Text, "unknown tool") {
		t.Errorf("unexpected refusal for an excluded tool: %q", res.Text)
	}

	// The parent is untouched — a subset is a view, not a mutation.
	if !reg.Has("write_thing") {
		t.Error("building a subset removed a tool from the parent registry")
	}
	if res := reg.Run(context.Background(), Call{ID: "2", Name: "write_thing"}); res.IsError {
		t.Errorf("the parent registry lost a tool to its own subset: %q", res.Text)
	}
}
