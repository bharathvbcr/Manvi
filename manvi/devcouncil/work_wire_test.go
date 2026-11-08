package devcouncil

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
)

// WorkBindingFor is how a host turns its own session id into a binding. An id
// the store's identifier rule would refuse must cost the binding its run id,
// never the session its tools: New fails on an invalid RunID, so passing a
// resumed session's 64-character id straight through would make the whole
// harness fail to build.
func TestWorkBindingForKeepsAFittingIDAndDropsOthers(t *testing.T) {
	cases := []struct {
		id, want string
	}{
		{"", ""},
		{"0123456789abcdef", "0123456789abcdef"},
		{strings.Repeat("a", 32), strings.Repeat("a", 32)},
		{strings.Repeat("a", 33), ""},
		{strings.Repeat("a", 64), ""},
		{"has space", ""},
		{"semi;colon", ""},
		{"line\nbreak", ""},
		{"ünï", ""},
	}
	for _, tc := range cases {
		binding := WorkBindingFor(tc.id)
		if binding == nil {
			t.Fatalf("WorkBindingFor(%q) = nil: a nil binding registers no tools", tc.id)
		}
		if binding.RunID != tc.want {
			t.Errorf("WorkBindingFor(%q).RunID = %q, want %q", tc.id, binding.RunID, tc.want)
		}
		if err := binding.validate(); err != nil {
			t.Errorf("WorkBindingFor(%q) is invalid: %v", tc.id, err)
		}
	}
}

// IsWorkTool is what a host's sub-agent floor asks to take the board out of a
// child's registry, so it has to hold for every tool workTools registers and
// for no other tool the catalogue carries.
func TestIsWorkToolMatchesExactlyTheBoardTools(t *testing.T) {
	r := agentSurfaceRegistry(&Registry{session: &Session{}})
	board := map[string]bool{}
	for _, tl := range r.workTools() {
		board[tl.Schema.Name] = true
		if !IsWorkTool(tl.Schema.Name) {
			t.Errorf("%s is a board tool that IsWorkTool does not recognise", tl.Schema.Name)
		}
	}
	if len(board) != 6 {
		t.Fatalf("the board registers %d tools, want 6: %v", len(board), board)
	}
	for _, tl := range r.Tools() {
		if !board[tl.Schema.Name] && IsWorkTool(tl.Schema.Name) {
			t.Errorf("%s is not a board tool and IsWorkTool claims it", tl.Schema.Name)
		}
	}
	// With no binding the catalogue holds none, and the predicate has nothing to
	// claim; this is the default every non-agent surface relies on.
	bare := &Registry{deps: Deps{Store: &store.Client{}}, session: &Session{}}
	for _, tl := range bare.Tools() {
		if IsWorkTool(tl.Schema.Name) {
			t.Errorf("a Registry with no Work binding registers %s", tl.Schema.Name)
		}
	}
}
