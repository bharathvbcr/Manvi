package devcouncil

import (
	"encoding/json"
	"testing"
)

// The escape hatch has to actually work. Dynamic loading defaults on for local
// models, and it is only safe because a tool that is not offered can still be
// fetched: without that, narrowing the set is llm.local.core_tools_only, which
// was measured scoring 0/32 on tasks needing an omitted tool.
func TestActivatingAGroupGrowsTheOfferedSet(t *testing.T) {
	f := newFixtureRunner(t, &recordingRunner{})
	f.pipe.EnableDynamic()

	before := len(f.pipe.ActiveSchemas())
	all := len(f.pipe.Schemas())
	if before >= all {
		t.Fatalf("dynamic mode offered %d of %d tools; it is not narrowing anything", before, all)
	}

	// The discovery pair must survive the narrowing, or nothing below is
	// reachable in a real turn.
	for _, name := range []string{"devcouncil_search_tools", "devcouncil_activate_tools"} {
		found := false
		for _, s := range f.pipe.ActiveSchemas() {
			if s.Name == name {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s is not offered under dynamic mode; the set can never grow", name)
		}
	}

	// Search must find something that is not currently offered.
	res := f.call("devcouncil_search_tools", map[string]any{"query": "subagent"})
	if res.IsError {
		t.Fatalf("search_tools failed: %s", res.Text)
	}
	var found struct {
		Count   int `json:"count"`
		Results []struct {
			Name   string `json:"name"`
			Active bool   `json:"active"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Text), &found); err != nil {
		t.Fatalf("decoding search_tools result: %v (%s)", err, res.Text)
	}
	if found.Count == 0 {
		t.Fatal("search_tools found no subagent tools")
	}
	inactive := 0
	for _, r := range found.Results {
		if !r.Active {
			inactive++
		}
	}
	if inactive == 0 {
		t.Fatal("every matched tool was already active; this proves nothing about activation")
	}

	// Activating by group must make them callable.
	res = f.call("devcouncil_activate_tools", map[string]any{"tools": []string{"subagent"}})
	if res.IsError {
		t.Fatalf("activate_tools failed: %s", res.Text)
	}
	after := len(f.pipe.ActiveSchemas())
	if after <= before {
		t.Fatalf("activating the subagent group did not grow the offered set: %d -> %d", before, after)
	}
	t.Logf("offered set grew %d -> %d of %d", before, after, all)
}

// Activating an unknown name must fail loudly. A silent success would tell the
// model the tool it needs is now available when it is not, and the next step
// would be an unknown-tool error it has no way to connect to this call.
func TestActivatingAnUnknownToolIsRefused(t *testing.T) {
	f := newFixtureRunner(t, &recordingRunner{})
	f.pipe.EnableDynamic()

	res := f.call("devcouncil_activate_tools", map[string]any{"tools": []string{"no_such_tool"}})
	if !res.IsError {
		t.Fatalf("activating an unknown tool reported success: %s", res.Text)
	}
}
