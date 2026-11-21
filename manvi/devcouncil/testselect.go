package devcouncil

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
)

// Test selection: which tests the code index says a turn's changes reach.
//
// This is selection and nothing else. Nothing here, or downstream of it, starts
// a test: the operator's own MANVI_VERIFY_COMMAND is the only thing that runs
// tests at the end of a turn, and it runs exactly as it did before this
// existed. The selection is information put beside that command's verdict (or
// beside its absence) so a reader can see which tests the change touches and
// that nobody ran them.

const (
	// testSelectTimeout bounds the lookup: the end-of-turn check must not wait
	// on the index for longer than a lookup is worth.
	testSelectTimeout = 10 * time.Second
	// testSelectBudget is the token budget of the affected query. The report
	// names a capped handful, so a larger answer would be discarded.
	testSelectBudget = 4000
	// testSelectDepth is how far from a changed file the walk may go to reach a
	// test. Deeper than a write's impact summary because a test is usually
	// several callers away from the code it covers.
	testSelectDepth = 4
	// MaxSelectedTests is how many test names a report carries; Total carries
	// the real count.
	MaxSelectedTests = 20
	// selectNoteBytes bounds each caveat, whatever the index put in it.
	selectNoteBytes = 240
)

// TestSelection is the tests the index reached from a turn's changed files.
//
// It is a claim about the index, not about the tree: Stale and Notes say when
// the two may differ. A selection that could not be obtained has Ran false and
// is never rendered as "no affected tests".
type TestSelection struct {
	// Ran is true when the index answered. False means the question was not
	// answered at all, and Notes says why.
	Ran bool `json:"ran"`
	// Tests are the selected test files, sorted, at most MaxSelectedTests.
	Tests []string `json:"tests,omitempty"`
	// Total is how many tests the index reached, which can exceed len(Tests).
	Total int `json:"total"`
	// Generation is the index generation that answered; 0 when none did.
	Generation int `json:"generation,omitempty"`
	// Stale is true when the index is behind the tree.
	Stale bool `json:"stale,omitempty"`
	// Notes are the reasons the selection may be incomplete, or did not run.
	Notes []string `json:"notes,omitempty"`
}

// Caveat adds a reason the selection may be incomplete.
func (s *TestSelection) Caveat(note string) {
	if s != nil && note != "" {
		s.Notes = append(s.Notes, note)
	}
}

// Incomplete reports whether the selection may be missing tests.
func (s *TestSelection) Incomplete() bool {
	return s != nil && (!s.Ran || s.Stale || len(s.Notes) > 0)
}

// Summary renders the selection as one statement that is always about
// selection: it says the tests were not run, and it says when it could not
// select at all.
func (s *TestSelection) Summary() string {
	if s == nil {
		return ""
	}
	if !s.Ran {
		return "Test selection did not run (" + strings.Join(s.Notes, "; ") +
			"). Which tests this change affects is unknown; that is not the same as none."
	}
	index := fmt.Sprintf("index generation %d, %s", s.Generation, map[bool]string{true: "stale", false: "fresh"}[s.Stale])
	var b strings.Builder
	if s.Total == 0 {
		fmt.Fprintf(&b, "Test selection (%s): the index reaches no tests from the changed files. Selected, not run.", index)
	} else {
		fmt.Fprintf(&b, "Selected, not run (%s): %d test(s) reachable from the changed files: %s",
			index, s.Total, strings.Join(s.Tests, ", "))
		if more := s.Total - len(s.Tests); more > 0 {
			fmt.Fprintf(&b, " and %d more", more)
		}
		b.WriteString(".")
	}
	if s.Stale {
		b.WriteString(" The index is behind the tree, so tests or edges added this turn may be missing: this selection may be incomplete.")
	}
	for _, n := range s.Notes {
		b.WriteString(" ")
		b.WriteString(n)
		if !strings.HasSuffix(n, ".") {
			b.WriteString(".")
		}
	}
	return b.String()
}

// notRun is a selection that could not be obtained, with the reason.
func notRun(reason string) *TestSelection {
	return &TestSelection{Notes: []string{clipLine(reason, selectNoteBytes)}}
}

// selectTests asks the index which tests the examined paths reach.
//
// Every failure — no index configured, a stale or unready index the client
// refuses, a timeout, an answer it cannot read — returns a selection with Ran
// false. None of them returns an empty Ran selection, because an empty
// selection reads as "nothing is affected".
func (r *Registry) selectTests(ctx context.Context, paths []string) *TestSelection {
	client, err := r.navigator()
	if err != nil {
		return notRun(err.Error())
	}
	return selectTestsFrom(ctx, client, paths)
}

func selectTestsFrom(ctx context.Context, client *devmap.Client, paths []string) *TestSelection {
	if len(paths) == 0 {
		return notRun("no changed path could be examined")
	}
	ctx, cancel := context.WithTimeout(devmap.WithCall(ctx), testSelectTimeout)
	defer cancel()
	res, err := client.Advanced(ctx, devmap.AdvancedQuery{
		Kind: devmap.QueryAffected, Targets: paths, Depth: testSelectDepth, Budget: testSelectBudget})
	if err != nil {
		return notRun("the index could not answer: " + err.Error())
	}
	var answer struct {
		Tests struct {
			Items []struct {
				Path string `json:"path"`
			} `json:"items"`
			Total          int            `json:"total"`
			Truncated      bool           `json:"truncated"`
			Resolution     jsontext.Value `json:"resolution"`
			WalkIncomplete jsontext.Value `json:"walk_incomplete"`
		} `json:"tests"`
		BlastRadius struct {
			Unmatched []string `json:"unmatched_targets"`
		} `json:"blast_radius"`
	}
	if err := json.Unmarshal(res.Data, &answer); err != nil {
		return notRun("the index's answer was unreadable: " + err.Error())
	}
	if res.Index == nil {
		return notRun("the index did not report its generation")
	}

	sel := &TestSelection{Ran: true, Generation: res.Index.GenerationID, Stale: !res.Index.IsFresh}
	seen := map[string]bool{}
	var names []string
	for _, item := range answer.Tests.Items {
		name := clipLine(item.Path, 200)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	sel.Total = max(answer.Tests.Total, len(names))
	sel.Tests = names
	if len(sel.Tests) > MaxSelectedTests {
		sel.Tests = sel.Tests[:MaxSelectedTests]
	}
	if resolution := strings.TrimSpace(string(answer.Tests.Resolution)); resolution != `"Available"` {
		sel.Caveat("The index did not mark the answer available (" + clipLine(resolution, 80) + ")")
	}
	if answer.Tests.Truncated {
		sel.Caveat("The index cut the list short, so the total is a lower bound")
	}
	if wi := strings.TrimSpace(string(answer.Tests.WalkIncomplete)); wi != "" && wi != "null" && wi != "false" {
		sel.Caveat("The walk was incomplete: " + clipLine(strings.Trim(wi, `"`), selectNoteBytes))
	}
	if n := len(answer.BlastRadius.Unmatched); n > 0 {
		sel.Caveat(fmt.Sprintf("%d changed file(s) are not in the index (%s), so nothing could be selected for them",
			n, clipLine(strings.Join(answer.BlastRadius.Unmatched, ", "), 120)))
	}
	return sel
}
