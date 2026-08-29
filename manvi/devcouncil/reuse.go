package devcouncil

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"manvi/policy"
	"manvi/tools"
)

// "Extend existing seams rather than standing up duplicate implementations" has
// been in this harness's system prompt since it shipped, in two densities,
// carefully worded. Nothing behind it ever checked. There is no duplicate
// detection anywhere in the tree, so the instruction was a hope about the
// model's diligence, and a model that has not read the file it is duplicating
// has no way to comply with it even in principle.
//
// This is the mechanism. When a turn creates a *new* file, the harness asks the
// code graph what already answers to that name and puts the answer in the tool
// result — which the model cannot skip, unlike a line in a prompt it read
// nine thousand tokens ago.
//
// Three properties keep it from becoming a nuisance:
//
// It is advisory. It never refuses a write. A name collision is evidence, not a
// verdict: `handler.go` exists in six packages of any large repository and
// every one of them is correct. Refusing on this signal would block ordinary
// work to enforce a guess.
//
// It runs only on creation. Editing a file that already exists cannot be
// duplicating it, and paying for a graph query on every write would be a cost
// with no question attached.
//
// It fails visibly. No index, a stale index, a query that errors — each comes
// back as a stated degradation rather than as an empty candidate list, because
// "nothing like this exists" and "nothing looked" are the two readings of an
// empty list and only one of them is a reason to proceed.

// reuseQueryTimeout bounds the graph lookup.
//
// Short on purpose: this runs inside a write, and a write that hangs because
// its advisory lookup hung would be an advisory feature taking the critical
// path down with it. Past this the answer is "not known", which is a fine
// answer for something nothing depends on.
const reuseQueryTimeout = 3 * time.Second

// maxReuseCandidates bounds what is put in front of the model.
//
// Five. The list answers "does something like this already exist"; a reader
// needs two or three examples to answer it, and a wall of forty is a wall the
// model skims. Bounded rather than complete, and the excess is counted so the
// note never presents a sample as the whole.
const maxReuseCandidates = 5

// reuseReport is what the check found.
type reuseReport struct {
	// Path is the file that was created.
	Path string
	// Area is the subsystem it landed in, when the repository map can say.
	Area string
	// Candidates are existing files or symbols answering to a similar name.
	Candidates []string
	// More counts candidates beyond the cap, so the note can say the list is a
	// sample rather than the answer.
	More int
	// Degraded names why the check could not run. A report carrying this has
	// found nothing *because it did not look*, which is the opposite of having
	// looked and found nothing.
	Degraded []string
}

// Empty reports a check that ran and found nothing.
func (r reuseReport) Empty() bool { return len(r.Candidates) == 0 && len(r.Degraded) == 0 }

// Note renders the report for the model, or empty when there is nothing worth
// saying.
//
// A clean check says nothing at all. Announcing "no existing implementation was
// found" on every new file would train the reader to skip the line, and the
// line that matters is the one that is not there most of the time.
func (r reuseReport) Note() string {
	if r.Empty() {
		return ""
	}
	var b strings.Builder
	if len(r.Degraded) > 0 {
		b.WriteString("\n[reuse check: did not run — ")
		b.WriteString(strings.Join(r.Degraded, "; "))
		b.WriteString(". Nothing here says this is not a duplicate]")
		return b.String()
	}
	b.WriteString("\n[reuse check: this is a new file")
	if r.Area != "" {
		fmt.Fprintf(&b, " in %s", r.Area)
	}
	b.WriteString(", and these already answer to a similar name:")
	for _, c := range r.Candidates {
		b.WriteString("\n  - ")
		b.WriteString(c)
	}
	if r.More > 0 {
		fmt.Fprintf(&b, "\n  … and %d more", r.More)
	}
	b.WriteString("\n  Extend one of these if it is the same behaviour. If it is not, carry on]")
	return b.String()
}

// checkReuse asks what already exists under a name close to this one.
//
// normalized is a repository-relative path that has just been created.
func (r *Registry) checkReuse(ctx context.Context, normalized string) reuseReport {
	report := reuseReport{Path: normalized}
	if r.deps.Subsystems != nil {
		if area, ok := r.deps.Subsystems.AreaForPath(normalized); ok {
			report.Area = area
		}
	}

	stem := reuseStem(normalized)
	if stem == "" {
		// Nothing to search on. Not a degradation: a path with no usable stem
		// is a question this check cannot ask, not one it failed to ask.
		return report
	}

	client, err := r.navigator()
	if err != nil {
		report.Degraded = append(report.Degraded, "no code index is configured")
		return report
	}
	// Bounded independently of the caller's context. A write is on the critical
	// path and this is not; inheriting an unbounded turn context would let an
	// advisory lookup hold a write open.
	queryCtx, cancel := context.WithTimeout(ctx, reuseQueryTimeout)
	defer cancel()

	if _, err := client.Available(queryCtx); err != nil {
		report.Degraded = append(report.Degraded, fmt.Sprintf("the code index is unavailable (%v)", err))
		return report
	}
	result, err := client.Search(queryCtx, stem)
	if err != nil {
		report.Degraded = append(report.Degraded, fmt.Sprintf("the code index query failed (%v)", err))
		return report
	}

	seen := map[string]bool{}
	var all []string
	for _, sym := range result.Items {
		// The file being created is not a candidate for its own replacement.
		// It can already be in the index when a build ran mid-turn, and
		// "consider extending the file you are writing" is noise.
		if samePath(sym.FilePath, normalized) {
			continue
		}
		label := sym.FilePath
		if sym.Name != "" {
			label = sym.FilePath + ":" + sym.Name
		}
		if seen[label] {
			continue
		}
		seen[label] = true
		all = append(all, label)
	}
	sort.Strings(all)
	if len(all) > maxReuseCandidates {
		report.More = len(all) - maxReuseCandidates
		all = all[:maxReuseCandidates]
	}
	report.Candidates = all
	return report
}

// reuseStem is the name a new file is searched under.
//
// The base name without its extension, and nothing else. Searching the whole
// path would match every file in the directory, and searching the extension
// would match the language.
//
// Names too short or too generic to discriminate are refused rather than
// searched: "main", "util" and "test" match half a repository, and a note
// listing half a repository is a note nobody reads twice.
func reuseStem(normalized string) string {
	base := path.Base(normalized)
	if i := strings.Index(base, "."); i > 0 {
		base = base[:i]
	}
	base = strings.TrimSpace(base)
	if len(base) < 4 {
		return ""
	}
	switch strings.ToLower(base) {
	case "main", "util", "utils", "test", "tests", "index", "init", "types", "common", "helper", "helpers":
		return ""
	}
	return base
}

// samePath compares two repository-relative paths for identity.
func samePath(a, b string) bool {
	return strings.EqualFold(strings.TrimPrefix(a, "./"), strings.TrimPrefix(b, "./"))
}

// annotateReuse folds a reuse report into a write's result.
//
// The note goes on the result text and nowhere else, and that is the whole
// storage design. The model is guaranteed to read the result, and the result's
// text is already written to the session log as a ToolResult event — so the
// note is durable, survives resume, and appears in an evidence report without a
// second event carrying a second copy of the same fact. A dedicated event was
// drafted and removed for exactly that reason: two records of one finding is
// two records that can disagree.
func annotateReuse(result tools.Result, report reuseReport) tools.Result {
	if note := report.Note(); note != "" {
		result.Text += note
	}
	return result
}

// createdPath reports whether a write to this path would create a new file.
//
// Asked before the write, because afterwards every path exists. A path that
// cannot be resolved is reported as not-new: the check is advisory, and
// guessing "new" on an unresolvable path would produce a note about a file
// nobody can look at.
func (r *Registry) createdPath(rawPath string) (string, bool) {
	normalized, outside := policy.NormalizeRepoPath(r.deps.Root, rawPath)
	if outside {
		return "", false
	}
	present, _ := r.ExistingPaths([]string{normalized})
	return normalized, len(present) == 0
}
