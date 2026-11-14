package devcouncil

import (
	"reflect"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/dcgrep"
)

// keepListedMatches decides from paths alone: a match in a name the listing did
// not hold, or held as refused, is dropped; the rest keep the engine's order.
func TestW21KeepListedMatchesDecidesFromPathsAlone(t *testing.T) {
	m := func(p string, n int) dcgrep.Match { return dcgrep.Match{Path: p, LineNumber: n, Line: "x"} }
	listed := []string{"a.go", "b.go", "c.go"}
	refused := map[string]string{"c.go": "path.secret_read"}
	in := []dcgrep.Match{m("b.go", 2), m("unlisted.txt", 1), m("c.go", 1), m("a.go", 3), m("b.go", 1)}
	want := []dcgrep.Match{m("b.go", 2), m("a.go", 3), m("b.go", 1)}
	if got := keepListedMatches(listed, refused, in); !reflect.DeepEqual(got, want) {
		t.Errorf("kept %v, want %v", got, want)
	}
	if got := keepListedMatches(listed, refused, nil); len(got) != 0 {
		t.Errorf("nil input kept %v", got)
	}
	if got := keepListedMatches(listed, refused, []dcgrep.Match{}); len(got) != 0 {
		t.Errorf("empty input kept %v", got)
	}
	if got := keepListedMatches(nil, nil, in); len(got) != 0 {
		t.Errorf("an empty listing kept %v", got)
	}
}
