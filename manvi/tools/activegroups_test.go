package tools

import (
	"context"
	"testing"

	"manvi/core/bus"
	"manvi/llm"
)

func groupRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry(bus.New())
	for _, spec := range []struct {
		name     string
		group    string
		extended bool
	}{
		{"core_read", GroupCore, false},
		{"core_write", GroupCore, false},
		{"nav_query", GroupNav, true},
		{"sub_spawn", GroupSubagent, true},
		{"mcp_call", GroupMCP, true},
	} {
		if err := r.Register(Tool{
			Schema:   llm.ToolSchema{Name: spec.name, Description: "d"},
			Group:    spec.group,
			Extended: spec.extended,
			Handler:  func(context.Context, Call) Result { return Result{} },
		}); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func has(groups []string, want string) bool {
	for _, g := range groups {
		if g == want {
			return true
		}
	}
	return false
}

// Outside dynamic mode nothing is narrowed, so every group is reported. A
// caller that never narrowed anything must not be told its groups are absent.
func TestActiveGroupsReportsEveryGroupWhenNotNarrowing(t *testing.T) {
	got := groupRegistry(t).ActiveGroups()
	for _, want := range []string{GroupCore, GroupNav, GroupSubagent, GroupMCP} {
		if !has(got, want) {
			t.Errorf("group %q missing from %v", want, got)
		}
	}
}

// Under dynamic loading the answer must track the offered set, which is the
// whole reason guidance can follow capability at all.
func TestActiveGroupsTracksTheOfferedSet(t *testing.T) {
	r := groupRegistry(t)
	r.EnableDynamic()

	got := r.ActiveGroups()
	if !has(got, GroupCore) {
		t.Fatalf("core group is not active under dynamic mode: %v", got)
	}
	for _, gone := range []string{GroupNav, GroupSubagent, GroupMCP} {
		if has(got, gone) {
			t.Errorf("group %q is reported active but its tools were not offered: %v", gone, got)
		}
	}

	if _, err := r.Activate(GroupNav); err != nil {
		t.Fatal(err)
	}
	got = r.ActiveGroups()
	if !has(got, GroupNav) {
		t.Errorf("nav was activated but is not reported active: %v", got)
	}
	if has(got, GroupSubagent) {
		t.Errorf("activating nav also reported subagent active: %v", got)
	}
}

// Deterministic order: the prompt is assembled from this, and a prompt whose
// bytes change run to run cannot be diffed or cached.
func TestActiveGroupsIsSorted(t *testing.T) {
	got := groupRegistry(t).ActiveGroups()
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("ActiveGroups is not sorted: %v", got)
		}
	}
}
