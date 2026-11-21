package devcouncil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
)

// affectedDoc is a devmap affected envelope listing these test files, with the
// counts devmap's own completeness check requires of it.
func affectedDoc(total int, tests ...string) string {
	items := make([]string, len(tests))
	for i, p := range tests {
		items[i] = fmt.Sprintf(`{"depth":1,"path":%q,"reached_symbols":1,"symbols":[]}`, p)
	}
	return fmt.Sprintf(`{"targets":["a.go"],"tests":{"items":[%s],"resolution":"Available","shown":%d,"total":%d,"hidden":%d,"truncated":%t,"tokens_used":10},`+
		`"blast_radius":{"layers":{"items":[],"resolution":"Available","shown":0,"total":0,"hidden":0,"truncated":false,"tokens_used":0},"seeds":[],"unmatched_targets":[]}}`,
		strings.Join(items, ","), len(tests), total, total-len(tests), total > len(tests))
}

func (s *stubIndex) calls(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.dir, "calls"))
	if err != nil {
		return nil
	}
	return strings.Fields(string(raw))
}

// onlyQueries fails when the index stub was asked for anything but a status or
// an affected lookup, which is the whole of what selection may do.
func onlyQueries(t *testing.T, calls []string) {
	t.Helper()
	for _, c := range calls {
		if c != "status" && c != "affected" {
			t.Fatalf("selection invoked %q; only status and affected are lookups", c)
		}
	}
	if spawns(calls, "affected") == 0 {
		t.Fatalf("the affected query never ran: calls = %v", calls)
	}
}

func TestVerifyPathsReportsSelectedTestsAndRunsNone(t *testing.T) {
	f := newFixture(t)
	idx := newStubIndex(t, f, 7, true)
	idx.set(t, "affected", affectedDoc(2, "pkg/b_test.go", "pkg/a_test.go"))

	got := f.reg.VerifyPaths(context.Background(), []string{"a.go"}, "", Baseline{})

	sel := got.Selection
	if sel == nil || !sel.Ran || sel.Generation != 7 || sel.Stale || sel.Total != 2 {
		t.Fatalf("selection = %+v", sel)
	}
	if want := []string{"pkg/a_test.go", "pkg/b_test.go"}; strings.Join(sel.Tests, ",") != strings.Join(want, ",") {
		t.Fatalf("tests = %v, want %v", sel.Tests, want)
	}
	text := sel.Summary()
	for _, want := range []string{"Selected, not run", "generation 7, fresh", "2 test(s)", "pkg/a_test.go"} {
		if !strings.Contains(text, want) {
			t.Errorf("summary %q lacks %q", text, want)
		}
	}
	onlyQueries(t, idx.calls(t))
}

// The operator's command still runs exactly once and still decides the
// verdict; the selection beside it changes neither.
func TestVerifyPathsKeepsRunningTheOperatorCommandAndAddsTheSelection(t *testing.T) {
	f := newFixture(t)
	idx := newStubIndex(t, f, 3, true)
	idx.set(t, "affected", affectedDoc(1, "pkg/a_test.go"))
	marker := filepath.Join(f.root, "ran")

	got := f.reg.VerifyPaths(context.Background(), []string{"a.go"}, "echo x >> ran", Baseline{})

	raw, err := os.ReadFile(marker)
	if err != nil || string(raw) != "x\n" {
		t.Fatalf("the operator command wrote %q (err %v), want exactly one run", raw, err)
	}
	if got.Source != "operator verification command" {
		t.Fatalf("source = %q", got.Source)
	}
	if got.Selection == nil || !got.Selection.Ran || got.Selection.Total != 1 {
		t.Fatalf("selection = %+v", got.Selection)
	}
	onlyQueries(t, idx.calls(t))
}

func TestVerifyPathsSelectionNamesAreCappedAndCounted(t *testing.T) {
	f := newFixture(t)
	idx := newStubIndex(t, f, 7, true)
	names := make([]string, 25)
	for i := range names {
		names[i] = fmt.Sprintf("pkg/t%02d_test.go", i)
	}
	idx.set(t, "affected", affectedDoc(25, names...))

	sel := f.reg.VerifyPaths(context.Background(), []string{"a.go"}, "", Baseline{}).Selection

	if len(sel.Tests) != MaxSelectedTests || sel.Total != 25 {
		t.Fatalf("shown %d of %d, want %d of 25", len(sel.Tests), sel.Total, MaxSelectedTests)
	}
	if !strings.Contains(sel.Summary(), "and 5 more") {
		t.Fatalf("summary does not carry the count: %s", sel.Summary())
	}
}

// A stale index is stated on the selection, and an empty answer from one is not
// the same as no affected tests.
func TestVerifyPathsSelectionOnAStaleIndexSaysItMayBeIncomplete(t *testing.T) {
	f := newFixture(t)
	idx := newStubIndex(t, f, 9, false)
	idx.set(t, "affected", affectedDoc(0))

	sel := f.reg.VerifyPaths(context.Background(), []string{"a.go"}, "", Baseline{}).Selection

	if sel == nil || !sel.Ran || !sel.Stale || !sel.Incomplete() {
		t.Fatalf("selection = %+v", sel)
	}
	for _, want := range []string{"generation 9, stale", "may be incomplete", "Selected, not run"} {
		if !strings.Contains(sel.Summary(), want) {
			t.Errorf("summary %q lacks %q", sel.Summary(), want)
		}
	}
}

func TestVerifyPathsSelectionReportsAnUnmatchedChangedFile(t *testing.T) {
	f := newFixture(t)
	idx := newStubIndex(t, f, 7, true)
	idx.set(t, "affected", strings.Replace(affectedDoc(0), `"unmatched_targets":[]`, `"unmatched_targets":["a.go"]`, 1))

	sel := f.reg.VerifyPaths(context.Background(), []string{"a.go"}, "", Baseline{}).Selection

	if !sel.Incomplete() || !strings.Contains(sel.Summary(), "not in the index") {
		t.Fatalf("an unmatched file was reported as a clean empty selection: %s", sel.Summary())
	}
}

// A selection that could not be obtained says so; it never reads as "none".
func TestVerifyPathsSelectionThatCouldNotRunIsNeverNoAffectedTests(t *testing.T) {
	cases := map[string]func(t *testing.T) *Registry{
		"no index configured": func(t *testing.T) *Registry { return newFixture(t).reg },
		"the index refuses the query": func(t *testing.T) *Registry {
			f := newFixture(t)
			idx := newStubIndex(t, f, 7, true)
			idx.set(t, "affected", "{}")
			idx.set(t, "affected.fail", "")
			return f.reg
		},
		"the index answers something unreadable": func(t *testing.T) *Registry {
			f := newFixture(t)
			idx := newStubIndex(t, f, 7, true)
			idx.set(t, "affected", "not json")
			return f.reg
		},
		"the index is not ready": func(t *testing.T) *Registry {
			f := newFixture(t)
			idx := newStubIndex(t, f, 0, false)
			idx.set(t, "affected", affectedDoc(0))
			return f.reg
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			sel := build(t).VerifyPaths(context.Background(), []string{"a.go"}, "", Baseline{}).Selection
			if sel == nil || sel.Ran || !sel.Incomplete() || sel.Total != 0 {
				t.Fatalf("selection = %+v, want a selection that did not run", sel)
			}
			text := sel.Summary()
			if !strings.Contains(text, "did not run") || strings.Contains(text, "no tests") {
				t.Fatalf("summary = %q", text)
			}
		})
	}
}

func TestSelectTestsFromRefusesAnEmptyPathList(t *testing.T) {
	if sel := selectTestsFrom(context.Background(), devmap.New("/nonexistent", t.TempDir()), nil); sel.Ran {
		t.Fatalf("selection over no paths ran: %+v", sel)
	}
}
