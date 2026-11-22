package devcouncil

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// What the code index knows about this session's own edits.
//
// The index is built from the tree and is not rebuilt per write, so after the
// agent writes a file it is behind by exactly the paths the agent touched. Two
// things follow, and this file owns both:
//
//   - noteWriteImpact tells the agent, on the write's own result, who calls
//     into the file it just changed, labelled with the generation that said so
//     and with the staleness stated when the index has not seen the write.
//   - writtenSet remembers which paths were written and under which generation,
//     so devcouncil_code can say first which paths its answer cannot contain.

const (
	// writeImpactTimeout bounds the impact check: a write must not wait on the
	// index for longer than a lookup is worth. One lookup is three devmap
	// invocations (status, impact, status). Measured against this repository's
	// index at host load average 46: status p50 1.1 s, max 1.9 s and impact p50
	// 1.0 s, max 1.1 s (10 samples each); the whole sequence p50 3.4 s, p90
	// 5.2 s, max 6.1 s (20 runs). A bound below that max would report healthy
	// lookups as "did not run", so the cap stays above it. The worst case a
	// write waits is this context timeout plus the 2 s WaitDelay DevCouncil's
	// devmap client allows after killing a stuck process, about 10 s. A lookup
	// that does not finish in time is reported on the write's result as "impact check did not run ...
	// callers are unknown", never as a clean or empty answer.
	writeImpactTimeout = 8 * time.Second
	// writeImpactBudget is the token budget of the impact query. The summary
	// keeps only a handful of names, so a larger answer would be discarded.
	writeImpactBudget = 800
	// writeImpactCallers is how many callers the summary names.
	writeImpactCallers = 10
	// writeImpactMaxBytes bounds the summary line, whatever the symbol names.
	writeImpactMaxBytes = 700
	// maxWrittenPaths bounds the set; see writtenSet.record.
	maxWrittenPaths = 1024
	// noticePathsShown is how many written paths a stale-index header names.
	noticePathsShown = 10
)

// writtenSet is the repository paths this registry wrote, each with the index
// generation current when it was written (0 when the index could not say).
type writtenSet struct {
	mu      sync.Mutex
	paths   map[string]int
	dropped int
}

// record notes a write. Past maxWrittenPaths a new path is counted, not kept,
// and the notice says so: forgetting here would hide a path the index has not
// seen, which is the one direction this set must not err in.
func (s *writtenSet) record(path string, generation int) {
	if path == "" {
		return
	}
	key := inspectedKey(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, known := s.paths[key]; !known && len(s.paths) >= maxWrittenPaths {
		s.dropped++
		return
	}
	if s.paths == nil {
		s.paths = make(map[string]int)
	}
	s.paths[key] = generation
}

// unseen returns the written paths the index at generation current cannot yet
// contain, and how many written paths the set could not keep. A fresh index has
// seen everything; a path written under an earlier generation than the current
// one was in the tree when that generation was built, so it is forgotten.
func (s *writtenSet) unseen(current int, fresh bool) (paths []string, dropped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fresh {
		s.paths, s.dropped = nil, 0
		return nil, 0
	}
	for p, gen := range s.paths {
		if gen > 0 && current > gen {
			delete(s.paths, p)
			continue
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, s.dropped
}

// staleIndexNotice is the header devcouncil_code puts first when its answer is
// from an index behind the tree. It returns "" when the index is current and
// holds nothing this session wrote.
func (r *Registry) staleIndexNotice(status *devmap.Status) string {
	if status == nil {
		return ""
	}
	paths, dropped := r.written.unseen(status.GenerationID, status.IsFresh)
	if status.IsFresh && len(paths) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The index (generation %d) is behind the tree", status.GenerationID)
	if len(paths) > 0 || dropped > 0 {
		b.WriteString(": it has not seen what this session wrote to ")
		shown := paths
		if len(shown) > noticePathsShown {
			shown = shown[:noticePathsShown]
		}
		b.WriteString(strings.Join(shown, ", "))
		if more := len(paths) - len(shown) + dropped; more > 0 {
			fmt.Fprintf(&b, " and %d more", more)
		}
	}
	b.WriteString(". An empty or partial answer below is not a negative: a symbol you just added is not in it " +
		"and one you changed may be described as it was. The file wins over this answer; read it.")
	return b.String()
}

// noteWriteImpact appends the impact summary to a successful write or patch
// result and records the path as written. It does nothing when the session has
// no navigator, and never turns a successful write into a failure: when the
// index cannot answer, the result says the check did not run.
func (r *Registry) noteWriteImpact(ctx context.Context, res tools.Result, target string) tools.Result {
	client, err := r.navigator()
	if err != nil || res.IsError || target == "" || filepath.IsAbs(target) || strings.HasPrefix(target, "..") {
		return res
	}
	line, generation := writeImpactLine(ctx, client, target)
	r.written.record(target, generation)
	res.Text += "\n" + line
	return res
}

// writeImpactLine asks the index who depends on target and renders one bounded
// line, with the generation it came from (0 when no answer was obtained).
func writeImpactLine(ctx context.Context, client *devmap.Client, target string) (string, int) {
	ctx, cancel := context.WithTimeout(devmap.WithCall(ctx), writeImpactTimeout)
	defer cancel()
	res, err := client.Advanced(ctx, devmap.AdvancedQuery{
		Kind: devmap.QueryImpact, Query: target, Depth: 1, Budget: writeImpactBudget})
	if err != nil {
		return "impact check did not run: " + clipLine(err.Error(), 160) + ". Callers of " + target + " are unknown.", 0
	}
	generation := res.Index.GenerationID
	stale := ""
	if !res.Index.IsFresh {
		stale = " The index is stale: it does not include this write, so this is who called into the file before it."
	}
	var answer struct {
		Items []struct {
			SourceFile   string `json:"source_file"`
			SourceSymbol string `json:"source_symbol"`
		} `json:"items"`
		Truncated  bool           `json:"truncated"`
		Resolution jsontext.Value `json:"resolution"`
	}
	if err := json.Unmarshal(res.Data, &answer); err != nil {
		return fmt.Sprintf("impact check did not run: the answer was unreadable (%s). Callers of %s are unknown.",
			clipLine(err.Error(), 100), target), generation
	}
	if string(answer.Resolution) != `"Available"` {
		return fmt.Sprintf("impact (index generation %d): %s is not in the index yet, so its callers are unknown.%s",
			generation, target, stale), generation
	}
	seen := map[string]bool{}
	var callers []string
	for _, item := range answer.Items {
		if item.SourceFile == target || item.SourceSymbol == "" || seen[item.SourceSymbol] {
			continue
		}
		seen[item.SourceSymbol] = true
		callers = append(callers, item.SourceSymbol)
	}
	sort.Strings(callers)
	if len(callers) == 0 {
		return fmt.Sprintf("impact (index generation %d): no callers of %s outside it in the index%s.%s",
			generation, target, truncatedNote(answer.Truncated), stale), generation
	}
	shown := callers
	if len(shown) > writeImpactCallers {
		shown = shown[:writeImpactCallers]
	}
	line := fmt.Sprintf("impact (index generation %d): %d caller(s) of %s outside it%s: %s",
		generation, len(callers), target, truncatedNote(answer.Truncated), strings.Join(shown, ", "))
	if len(shown) < len(callers) {
		line += fmt.Sprintf(" and %d more", len(callers)-len(shown))
	}
	return clipLine(line, writeImpactMaxBytes-len(stale)) + "." + stale, generation
}

func truncatedNote(truncated bool) string {
	if truncated {
		return " (the walk was cut short, so this is a lower bound)"
	}
	return ""
}

// clipLine folds untrusted text onto one line (see oneLine) of at most max
// bytes, marking a cut.
func clipLine(s string, max int) string {
	s = oneLine(s)
	if len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "") + "..."
}
