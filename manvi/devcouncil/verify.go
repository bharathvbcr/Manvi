package devcouncil

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
	"github.com/bharathvbcr/Manvi/manvi/childenv"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// Gap is one verification finding.
//
// The shape mirrors DevCouncil's typed next-actions contract on purpose: an
// agent that already routes on `category` keeps working against this surface
// without learning a second vocabulary.
type Gap struct {
	ID       string `json:"id"`
	Type     string `json:"gap_type"`
	Severity string `json:"severity"`
	Blocking bool   `json:"blocking"`
	File     string `json:"file,omitempty"`
	Detail   string `json:"description"`
}

// NextAction is a machine-routable repair step. Category is the field to branch
// on; the prose is for the transcript, not for parsing.
type NextAction struct {
	GapID     string `json:"gap_id"`
	Category  string `json:"category"`
	Action    string `json:"action"`
	File      string `json:"file,omitempty"`
	Blocking  bool   `json:"blocking"`
	Tool      string `json:"suggested_tool,omitempty"`
	Arguments any    `json:"suggested_arguments,omitempty"`
}

// Report is the verifier's verdict.
type Report struct {
	TaskID      string       `json:"task_id"`
	Passed      bool         `json:"passed"`
	ChangedFile []string     `json:"changed_files"`
	InScope     []string     `json:"in_scope"`
	Orphans     []string     `json:"orphan_files"`
	Untouched   []string     `json:"untouched_planned"`
	Gaps        []Gap        `json:"gaps"`
	NextActions []NextAction `json:"next_actions"`
	// Degraded names checks that could not run. A pass with a non-empty
	// Degraded is not the same as a clean pass, and a caller must be able to
	// tell — this is the "a check that could not run must never look like one
	// that ran and passed" rule, in report form.
	Degraded []string `json:"degraded,omitempty"`
	// Verdict is failed when a gap blocks, degraded when nothing blocks but the
	// diff the gates read did not cover the whole change, and passed otherwise.
	// Passed is true only for the last.
	Verdict        string `json:"verdict"`
	diffIncomplete bool
}

// BlockingCount is how many gaps stop the task.
func (r *Report) BlockingCount() int {
	n := 0
	for _, g := range r.Gaps {
		if g.Blocking {
			n++
		}
	}
	return n
}

// report runs the deterministic verifier over the working tree.
//
// Two layers. Orphan-diff detection runs here in Go, from the ported policy
// engine's own path matching, so scope is judged by exactly the rule the write
// gate enforced. The content gates — secret scanning, stub and effort
// heuristics, the diff↔coverage intersection — run in the Rust analysis plane
// and arrive over the dcverify boundary.
//
// When that boundary is unreachable, the gates it owns are named in Degraded
// rather than skipped in silence. That is the difference between "these checks
// ran and found nothing" and "these checks did not run", and a caller reading
// `passed: true` has to be able to tell which it is holding.
func (r *Registry) report(ctx context.Context) (*Report, error) {
	task, err := r.currentTask(ctx)
	if err != nil {
		return nil, err
	}

	diff, diffNotes, err := gitDiff(ctx, r.deps.Root)
	if err != nil {
		return nil, err
	}
	changed, err := changedFiles(diff)
	if err != nil {
		return nil, err
	}

	report := &Report{TaskID: task.ID, ChangedFile: changed}
	// A diff that could not cover the whole tree is named here rather than
	// folded into a pass: the gates below read this diff, so what is missing
	// from it is missing from them.
	report.Degraded = append(report.Degraded, diffNotes...)
	// An incomplete diff is a check that could not see everything. It is
	// neither a pass nor a failure: the verdict is degraded, with the reasons
	// (each carrying how many files were covered) in Degraded. Making it a
	// blocking gap instead would send any change past the untracked-file cap
	// back for repair forever, with nothing the agent could repair.
	report.diffIncomplete = len(diffNotes) > 0

	// The union, not the plan alone. This asks "was this file in scope", and a
	// path an executor argued into its own scope is in scope — the fact that it
	// was widened is carried on the write's own decision, which is where a
	// reviewer looks for it, rather than by reporting the file as an orphan
	// here.
	inScope := task.AllPlannedFiles()
	planned := make([]string, 0, len(inScope))
	for _, pf := range inScope {
		planned = append(planned, pf.Path)
	}
	touched := map[string]bool{}

	// One pass per file over the planned entries answers both questions: a
	// file is in scope exactly when some entry matches it (MatchesPlannedPath
	// is that any-of), and every entry it matches has been touched.
	for _, file := range changed {
		matched := false
		for i, p := range planned {
			if policy.MatchesPlannedPath(file, inScope[i:i+1]) {
				touched[p] = true
				matched = true
			}
		}
		if matched {
			report.InScope = append(report.InScope, file)
			continue
		}
		report.Orphans = append(report.Orphans, file)
	}
	for _, p := range planned {
		if !touched[p] {
			report.Untouched = append(report.Untouched, p)
		}
	}

	// The content gates, from the analysis plane.
	rigorGaps, rigorActions, degraded := r.runRigor(ctx, diff, planned, task.ID)
	report.Gaps = append(report.Gaps, rigorGaps...)
	report.NextActions = append(report.NextActions, rigorActions...)
	report.Degraded = append(report.Degraded, degraded...)

	for _, orphan := range report.Orphans {
		gap := Gap{
			ID:       fmt.Sprintf("GAP-%s-ORPHAN-%s", task.ID, strings.ReplaceAll(orphan, "/", "-")),
			Type:     "orphan_diff",
			Severity: "high",
			Blocking: true,
			File:     orphan,
			Detail: fmt.Sprintf("%s was changed but is not in task %s's planned files",
				orphan, task.ID),
		}
		report.Gaps = append(report.Gaps, gap)
		report.NextActions = append(report.NextActions, NextAction{
			GapID:    gap.ID,
			Category: "scope",
			Blocking: true,
			File:     orphan,
			Action: fmt.Sprintf("Either revert %s, or bring it into scope — request an override "+
				"with a reason, or have the task's planned files updated.", orphan),
			Tool: "devcouncil_request_override",
			Arguments: map[string]string{
				"path": orphan, "rule": string(policy.RuleUnplannedScope),
				"reason": "explain why this file had to change",
			},
		})
	}

	// An empty diff is not success. A task that changed nothing has not done
	// its work, and reporting it as passed is exactly the "green means done"
	// failure the verifier exists to prevent.
	if len(changed) == 0 {
		gap := Gap{
			ID:       fmt.Sprintf("GAP-%s-EMPTY", task.ID),
			Type:     "no_changes",
			Severity: "high",
			Blocking: true,
			Detail:   "the working tree has no changes; the task has not been implemented",
		}
		report.Gaps = append(report.Gaps, gap)
		report.NextActions = append(report.NextActions, NextAction{
			GapID:    gap.ID,
			Category: "fix_code",
			Blocking: true,
			Action:   "Implement the task before verifying.",
		})
	}

	report.Passed = report.BlockingCount() == 0 && !report.diffIncomplete
	switch {
	case report.BlockingCount() > 0:
		report.Verdict = VerdictFailed
	case report.diffIncomplete:
		report.Verdict = VerdictDegraded
	default:
		report.Verdict = VerdictPassed
	}
	return report, nil
}

// gitDiff returns the working-tree diff, including untracked files.
//
// Untracked files are included deliberately: a new file that the plan never
// authorised is exactly the orphan the scope check is looking for, and `git
// diff` alone would not see it.
// runRigor invokes the Rust gates, returning their findings or naming them as
// degraded.
//
// Every failure path here produces a degradation entry rather than an empty
// result. A verifier that cannot be reached, a diff that cannot be parsed, and
// a diff that is genuinely clean would otherwise all produce the same report.
func (r *Registry) runRigor(ctx context.Context, diff string, planned []string, taskID string) ([]Gap, []NextAction, []string) {
	const owned = "secret_scan, stub_detection, diff_coverage"
	var degraded []string

	client := rigorClient{Binary: r.deps.VerifierBinary, Root: r.deps.Root}
	if r.deps.VerifierBinary == "" {
		return nil, nil, []string{owned + ": no dcverify binary configured — these checks did not run"}
	}
	// The coverage file is optional, and its absence is reported rather than
	// silently producing "everything unmeasured" — which is what the gate said
	// for every file before any measurements could be supplied.
	coverage := r.deps.CoverageFile
	result, err := client.run(ctx, diff, planned, coverage)
	if err != nil {
		// Blocking, not merely degraded. The branch above already answered the
		// one case that is about the machine rather than about this change —
		// no verifier configured at all — so reaching here means a verifier was
		// configured and did not produce an answer: missing on disk, timed out,
		// or reached and refusing the input. In every one of those the secret
		// scanner, the stub detector and the coverage gate did not look at a
		// change that exists.
		//
		// It used to return a degradation and no gap, so `passed` — the field
		// callers branch on — came back true with the credential scanner having
		// never run. The test named TestAnUnreachableVerifierIsDegradedNotAPass
		// asserted the degradation was present and never asserted the pass, so
		// the name described an intent the suite did not check.
		//
		// This is also what closes the shape as a class rather than as the one
		// trigger found: a diff the verifier refuses is one way to stop the
		// gates running, and any future one lands on the same branch.
		gap := Gap{
			ID:       fmt.Sprintf("GAP-%s-RIGOR-UNAVAILABLE", taskID),
			Type:     "rigor_did_not_run",
			Severity: "high",
			Blocking: true,
			Detail: fmt.Sprintf("%s: did not run (%v); a change nothing scanned cannot be reported as verified",
				owned, err),
		}
		return []Gap{gap}, []NextAction{{
			GapID:    gap.ID,
			Category: "fix_environment",
			Blocking: true,
			Action: "Repair or unset the dcverify binary, then verify again. " +
				"Leaving it configured and broken stops the credential scan without stopping the run.",
		}}, []string{fmt.Sprintf("%s: did not run (%v)", owned, err)}
	}
	if coverage == "" {
		degraded = append(degraded,
			"diff_coverage: no coverage file was supplied, so every changed file is reported unmeasured; "+
				"pass one with --coverage or MANVI_COVERAGE to get a real answer")
	}

	// Every gate dcverify ran is reported, and no setting here changes that.
	//
	// verify.rigor.enabled and verify.diff_coverage.enforce were read on this
	// path until DevCouncil retired both (f7f427b, "retire the two
	// verification switches that nothing read"). The gates they named run in
	// dcverify, which this function reaches by finding the binary; neither key
	// ever decided whether a gate ran. A team that set enforce: true got no
	// change in behaviour and a clean report — a check that could not run
	// answering exactly as a check that ran and passed.
	//
	// So the reads are deleted rather than re-homed on a Manvi-owned key. Both
	// resolved to their defaults on every run that did not set them, and those
	// defaults are now the whole behaviour: stub_detection findings are always
	// kept, and an unmeasured or uncovered diff is always reported and never
	// blocks. Re-adding either switch would re-advertise control over
	// something it does not control, which is the defect the retirement closed.
	gaps, actions := gapsFrom(taskID, result)
	return gaps, actions, degraded
}

// gitTimeout bounds the whole diff, and each git invocation inside it.
//
// The tool dispatch path hands these calls a context with no deadline, so
// without a bound of its own a single wedged git — an index.lock another
// process is holding, a hook that never returns, a stalled filesystem — hangs
// the turn with nothing to report.
const gitTimeout = 2 * time.Minute

// maxUntrackedRendered caps the per-file fan-out below.
//
// Untracked files are rendered one subprocess each, and how many there are is
// decided by whatever happens to be sitting in the working tree — an unignored
// build directory or a stray dependency tree is thousands. The cap is what
// stops that being unbounded work; reporting it is what stops the result being
// a truncated diff that reads as a complete one.
const maxUntrackedRendered = 256

// gitDiff renders the working tree as one unified diff.
//
// The second return names what could not be covered. It is not an error: the
// diff is still usable, and the caller surfaces the shortfall as a degradation
// rather than letting a partial answer pass for a whole one.
func gitDiff(ctx context.Context, root string) (string, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	var notes []string

	tracked, trackedNote, err := runGitCapped(ctx, root, maxGitCaptureBytes, "diff", "HEAD")
	if err != nil {
		// A repository with no commits yet has no HEAD; fall back to the index.
		tracked, trackedNote, err = runGitCapped(ctx, root, maxGitCaptureBytes, "diff")
		if err != nil {
			return "", nil, err
		}
	}
	if trackedNote != "" {
		// Cut at the cap, then back to the last complete line: a header cut in
		// half parses as a header naming a file that does not exist, so the
		// gates would report on a path nobody wrote.
		tracked = trimPartialLine(tracked)
		notes = append(notes, fmt.Sprintf(
			"tracked_diff: %s; the gates read only the part above that cap, so the rest of the "+
				"change was not covered — commit or split the change to get a complete answer",
			trackedNote))
	}

	// -z is not a detail. Without it git separates paths with newlines and
	// quotes anything non-ASCII, and splitting that on whitespace loses every
	// path containing a space: the file vanishes from the diff, and a file that
	// is not in the diff is one the secret scanner and the coverage gate never
	// see. NUL-separated output has no such ambiguity.
	untracked, err := runGit(ctx, root, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		// Not fatal — the tracked diff is still worth gating — but not silent
		// either: no untracked file is in what follows, and a caller reading a
		// pass has to be able to tell that from "there were none".
		return tracked, append(notes, fmt.Sprintf(
			"untracked_diff: the untracked-file listing failed (%v), so no untracked file is in "+
				"this diff and the gates that read it did not see any", err)), nil
	}

	paths := make([]string, 0, 16)
	for _, path := range strings.Split(untracked, "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}

	rendered := paths
	if len(paths) > maxUntrackedRendered {
		rendered = paths[:maxUntrackedRendered]
		notes = append(notes, fmt.Sprintf(
			"untracked_diff: %d of %d untracked files were rendered (cap %d); "+
				"the remaining %d are not in this diff and the gates that read it did not see them — "+
				"ignore them in .gitignore or commit them to get a complete answer",
			len(rendered), len(paths), maxUntrackedRendered, len(paths)-len(rendered)))
	}

	var b strings.Builder
	b.WriteString(tracked)
	// One budget across the whole fan-out, not one cap per file: 256 files at
	// their own ceiling is 256 ceilings, which is not a bound on anything.
	renders, unrendered := renderUntracked(ctx, root, rendered, maxGitCaptureBytes)
	var cutFiles, failedFiles []string
	for _, u := range renders {
		if u.err != nil {
			// Listed by git and then not rendered: a permission error, a file
			// that vanished, a deadline that ran out. Skipping it silently
			// would hand the gates a diff without it and say nothing, so it is
			// named with its reason.
			failedFiles = append(failedFiles, fmt.Sprintf("%s (%s)", u.path, renderFailure(u.err)))
			continue
		}
		if u.note != "" {
			// The path and the byte counts together: "some of this file is
			// missing" is not actionable, and a note that carries only one of
			// the two numbers is how a capped sample gets read as a whole one.
			cutFiles = append(cutFiles, fmt.Sprintf("%s %s", u.path, u.note))
		}
		b.WriteString(u.added)
	}
	if len(cutFiles) > 0 {
		notes = append(notes, fmt.Sprintf(
			"untracked_diff: %d untracked file(s) were cut at the %d-byte capture budget: %s; "+
				"the gates saw only the part above the cut",
			len(cutFiles), maxGitCaptureBytes, strings.Join(cutFiles, "; ")))
	}
	if len(failedFiles) > 0 {
		notes = append(notes, fmt.Sprintf(
			"untracked_diff: rendered %d of %d untracked file(s); %d could not be rendered and are not in this diff: %s; "+
				"the gates that read it did not see them",
			len(renders)-len(failedFiles), len(paths), len(failedFiles), summariseList(failedFiles, maxNamedShortfalls)))
	}
	if unrendered > 0 {
		notes = append(notes, fmt.Sprintf(
			"untracked_diff: the %d-byte capture budget ran out; rendered %d of %d untracked file(s), %d still "+
				"unrendered; they are not in this diff and the gates did not see them",
			maxGitCaptureBytes, len(renders)-len(failedFiles), len(paths), unrendered))
	}
	diff := b.String()
	if quoted := quotedHeaderPaths(diff); len(quoted) > 0 {
		// quotePath=false covers non-ASCII, but git still escapes a path
		// containing a quote, a backslash or a control character. Those parse
		// as a literal that names no file on disk, so the gates would report on
		// a path that does not exist. Naming them is the difference between a
		// gap and a wrong answer.
		notes = append(notes, fmt.Sprintf(
			"diff_paths: %d path(s) came back escaped and cannot be resolved to a file (%s); "+
				"the gates did not cover them — rename them to drop quotes, backslashes and control characters",
			len(quoted), strings.Join(quoted, ", ")))
	}
	return diff, notes, nil
}

// maxNamedShortfalls bounds how many files one diff note names; the count is
// always the whole number, so a long list is never read as a complete one.
const maxNamedShortfalls = 10

// maxFailureReason bounds the reason carried per unrendered file.
const maxFailureReason = 160

// renderFailure is the reason a render failed, cut to something a note can
// carry: git's own message names the command, which the path beside it
// already says.
func renderFailure(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, "--no-index -- /dev/null "); i >= 0 {
		if j := strings.Index(msg[i:], ": "); j >= 0 {
			msg = msg[i+j+2:]
		}
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if runes := []rune(msg); len(runes) > maxFailureReason {
		msg = string(runes[:maxFailureReason]) + "…"
	}
	return msg
}

// summariseList names the first limit entries and counts the rest.
func summariseList(items []string, limit int) string {
	if len(items) <= limit {
		return strings.Join(items, "; ")
	}
	return fmt.Sprintf("%s; and %d more", strings.Join(items[:limit], "; "), len(items)-limit)
}

// untrackedWorkers is how many untracked-file renders run at once.
//
// Each render is its own git process, and almost all of its cost is starting
// git rather than diffing anything, so running them one after another made a
// tree with a few hundred new files cost about a second per diff — paid by
// every get_diff and every verify — for no reason but the shape of a loop.
const untrackedWorkers = 8

// untrackedBatchMin is the fewest untracked files worth the batch route. The
// batch is a fixed three git processes (about 33 ms) whatever the count; the
// per-file route is one process per file, eight at a time, about 1.8 ms a file
// once the pool is busy and one 8 ms process for a single file. They cross
// near 16 files, so below 16 the per-file route is the faster of the two. A
// variable only so the tests can reach the batch with a small tree.
var untrackedBatchMin = 16

// untrackedSpeculativeCap bounds what one render captures before its place in
// the budget is known.
//
// A render started ahead of its turn does not yet know how much budget the
// files before it will leave, so it captures up to the budget as it stands
// when it starts, held to this cap so eight renders in flight cannot each
// hold the whole 16 MiB. A file whose share turns out larger than what was
// captured is rendered again, the old way, when its turn comes. A variable
// only so the tests can make that path ordinary.
var untrackedSpeculativeCap = 1 << 20

// untrackedRender is one untracked file as the fan-out rendered it.
type untrackedRender struct {
	path string
	// added is the file's diff, already cut back to a whole line when note
	// says the budget cut it.
	added string
	// note is the capture's truncation note, empty when the file is whole.
	note string
	// err is set when nothing could be rendered; added is then empty and the
	// file took nothing from the budget.
	err error
}

// untrackedDiffArgs renders an untracked file as an addition, so one parser
// handles both halves of the diff. The literal /dev/null, not os.DevNull: this
// is git's own convention for the empty side of a diff and it means the same
// thing on every platform, whereas os.DevNull is "NUL" on Windows and git
// would read that as a filename.
func untrackedDiffArgs(path string) []string {
	return []string{"diff", "--no-index", "--", "/dev/null", path}
}

// renderUntracked renders each path as an addition, in order, under one byte
// budget shared by all of them, and returns the renders it reached and how
// many paths the budget left unrendered.
//
// The answer is the sequential one, exactly. That loop ran one git per file
// with the budget still left as its cap, cut a capped file back to a whole
// line, and charged what it kept; it stopped once the budget was spent. The
// renders here run untrackedWorkers at a time, but they are the same commands
// and they are settled strictly in order against the same running budget, so
// what each file contributes — its bytes, its truncation note, its error —
// is what the loop would have produced. A render that started with a cap at
// least as large as its eventual share is cut to that share here, with the
// note the smaller cap would have written; one that captured less than its
// share, or failed, is run again with the exact old call before its answer
// is believed. The parallelism changes when git runs, never what the gates
// read: a degradation note that depended on scheduling would make the same
// tree verify differently twice.
func renderUntracked(ctx context.Context, root string, paths []string, budget int) ([]untrackedRender, int) {
	if len(paths) == 0 {
		return nil, 0
	}
	if budget <= 0 {
		return nil, len(paths)
	}
	if len(paths) >= untrackedBatchMin {
		if renders, unrendered, ok := renderUntrackedBatch(ctx, root, paths, budget); ok {
			return renders, unrendered
		}
	}
	return renderUntrackedPerFile(ctx, root, paths, budget)
}

// renderUntrackedPerFile is renderUntracked's answer by the route that is
// always available: one `git diff --no-index` per file, eight at a time. It
// is what the batch declines to, and the A side of the benchmark that compares
// the two.
func renderUntrackedPerFile(ctx context.Context, root string, paths []string, budget int) ([]untrackedRender, int) {
	if len(paths) == 0 {
		return nil, 0
	}
	if budget <= 0 {
		return nil, len(paths)
	}

	// Renders started ahead of their turn are this function's alone; once
	// the budget is spent or every file is settled, what is still running is
	// waste, and it is stopped and waited for rather than left behind.
	specCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()

	type speculative struct {
		capture gitCapture
		err     error
	}
	pending := make([]chan speculative, len(paths))
	launched := 0
	launch := func(limit int) {
		i := launched
		launched++
		done := make(chan speculative, 1)
		pending[i] = done
		wg.Go(func() {
			c, err := runGitCapture(specCtx, root, limit, untrackedDiffArgs(paths[i])...)
			done <- speculative{capture: c, err: err}
		})
	}

	renders := make([]untrackedRender, 0, len(paths))
	for i, path := range paths {
		if budget <= 0 {
			return renders, len(paths) - i
		}
		// The budget only shrinks as files are settled, so the budget now is
		// at least the share any file not yet settled will get. That is what
		// makes a capture started with it big enough to cut down from.
		for launched < len(paths) && launched < i+untrackedWorkers {
			launch(min(budget, untrackedSpeculativeCap))
		}
		s := <-pending[i]
		pending[i] = nil
		u := settleUntracked(ctx, root, path, budget, s.capture, s.err)
		if u.err == nil {
			budget -= len(u.added)
		}
		renders = append(renders, u)
	}
	return renders, 0
}

// settleUntracked turns a capture taken under some cap into the render the
// sequential loop would have produced with share bytes of budget left.
func settleUntracked(ctx context.Context, root, path string, share int, c gitCapture, err error) untrackedRender {
	if err == nil {
		whole := len(c.out) == c.total
		switch {
		case c.total <= share && whole:
			return untrackedRender{path: path, added: c.out}
		case c.total > share && len(c.out) >= share:
			// What a cap of share bytes would have kept and said: the first
			// share bytes, and the rest counted as discarded.
			return untrackedRender{
				path:  path,
				added: trimPartialLine(c.out[:share]),
				note:  truncationNoteFor(share, c.total-share),
			}
		}
	}
	// The capture was smaller than this file's share, or it failed. A failure
	// is not believed from a render that ran alongside others — eight at once
	// is not the conditions the old loop ran under, and a file skipped for a
	// spawn that only failed under load is a file the gates never see — so
	// both cases ask git again, exactly as the loop did.
	added, note, err := runGitCapped(ctx, root, share, untrackedDiffArgs(path)...)
	if err != nil && added == "" {
		return untrackedRender{path: path, err: err}
	}
	if note != "" {
		added = trimPartialLine(added)
	}
	return untrackedRender{path: path, added: added, note: note}
}

// trimPartialLine drops an incomplete final line from a capture that was cut at
// a byte boundary, so half a diff header cannot be read as a whole one.
func trimPartialLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[:i+1]
	}
	return ""
}

// quotedHeaderPaths finds diff headers whose path git had to escape.
func quotedHeaderPaths(diff string) []string {
	var found []string
	seen := map[string]bool{}
	for _, line := range strings.Split(diff, "\n") {
		rest, ok := strings.CutPrefix(line, "+++ ")
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, `"`) || seen[rest] {
			continue
		}
		seen[rest] = true
		found = append(found, rest)
	}
	return found
}

// Bounds on one git invocation's captured output.
//
// cmd.Output() had none: it accumulates everything the child writes into a
// bytes.Buffer that grows until the process runs out of memory, and every diff
// the verifier takes goes through this function. The size of that diff is not
// the harness's choice — it is decided by whatever is in the working tree,
// which on this path is content an agent has just written. The sibling paths
// already had ceilings (patchFile at maxPatchReadBytes, gatedGit at
// maxGitOutputBytes); this one is the one that did not.
//
// 16 MiB is far past any honest working-tree diff and far short of what a
// deliberately large one costs. stderr gets its own, much smaller cap: it is
// diagnostics, and a server that answers a failure with megabytes of chatter
// must not have those megabytes copied into an error string.
const (
	maxGitCaptureBytes = 16 << 20
	maxGitStderrBytes  = 64 << 10
	// gitStderrPreview bounds how much captured stderr travels on an error.
	gitStderrPreview = 400
)

// runGit runs one git command and returns its stdout, refusing rather than
// silently trimming when the output runs past the cap.
//
// Callers that can carry a degradation should use runGitCapped and report the
// note; this wrapper exists for the callers that cannot, and for them a
// truncated answer is worse than no answer — a diff cut mid-hunk still parses,
// still names files, and reads exactly like a complete one.
func runGit(ctx context.Context, root string, args ...string) (string, error) {
	out, note, err := runGitCapped(ctx, root, maxGitCaptureBytes, args...)
	if err != nil {
		return "", err
	}
	if note != "" {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), note)
	}
	return out, nil
}

// gitSpec builds every git child this package runs.
//
// One construction site rather than one per caller, so the arguments and the
// environment cannot drift between the command that reads a diff and the
// command that writes a tree, or the commit an agent asked for. The process
// group, the caps and the wait delay are runProc's, which every child shares.
//
// index, when non-empty, points git at a scratch index through GIT_INDEX_FILE,
// which is what lets CaptureBaseline record the tree without touching the
// repository's real one.
//
// The environment is git's allowlist, never this process's: a hook, filter or
// fsmonitor in the repository runs with it, and the repository is not trusted
// with the operator's provider keys.
//
// core.quotePath=false makes git emit non-ASCII paths as raw UTF-8 instead of
// C-style octal escapes. It is set here rather than in either parser because
// crates/dc-verify reads this same format: unquoting on one side only would
// give the two readers different answers for the same file.
func gitSpec(root, index string, args []string) procSpec {
	spec := procSpec{
		argv:        append([]string{"git", "-c", "core.quotePath=false"}, args...),
		dir:         root,
		timeout:     gitTimeout,
		stderrLimit: maxGitStderrBytes,
		env:         childenv.Git(),
	}
	if index != "" {
		spec.env = childenv.Git("GIT_INDEX_FILE=" + index)
	}
	return spec
}

// runGitEnv runs git against a scratch index, returning stdout and stderr.
//
// Separate from runGitCapped because the two have opposite needs: that one
// captures a diff whose size is the model's to abuse and reports a shortfall,
// while these commands produce an object id or nothing, so any truncation is
// a failure rather than a degradation. GIT_INDEX_FILE is what keeps the
// repository's real index out of it — see CaptureBaseline.
func runGitEnv(ctx context.Context, root, index string, args ...string) (string, string, error) {
	spec := gitSpec(root, index, args)
	spec.stdoutLimit = maxGitCaptureBytes
	res := runProc(ctx, spec)
	if res.timedOut {
		return "", "", fmt.Errorf("git %s: timed out", strings.Join(args, " "))
	}
	if res.stdoutNote != "" {
		return "", "", fmt.Errorf("git %s: %s", strings.Join(args, " "), res.stdoutNote)
	}
	return string(res.stdout), string(res.stderr), res.err
}

// runGitCapped runs one git command with stdout bounded at limit bytes.
//
// The second return is empty when the whole of git's output was captured, and
// otherwise says what was dropped. It is not an error: a caller with a
// degradation channel of its own can report the shortfall and carry on with
// the part it has. What no caller may do is treat a non-empty note as nothing.
func runGitCapped(ctx context.Context, root string, limit int, args ...string) (string, string, error) {
	c, err := runGitCapture(ctx, root, limit, args...)
	return c.out, c.note, err
}

// gitCapture is runGitCapped's answer with the one number it leaves out: the
// whole size of what git wrote, captured or not.
type gitCapture struct {
	out, note string
	// total is len(out) plus whatever the cap discarded.
	total int
}

// runGitCapture is runGitCapped, also reporting the size of the uncapped
// output. That size is what lets renderUntracked capture under one limit and
// answer, byte for byte, as a capture under a smaller one would have.
func runGitCapture(ctx context.Context, root string, limit int, args ...string) (gitCapture, error) {
	spec := gitSpec(root, "", args)
	spec.stdoutLimit = limit
	res := runProc(ctx, spec)
	if res.timedOut {
		return gitCapture{}, fmt.Errorf("git %s: timed out", strings.Join(args, " "))
	}
	out, err := string(res.stdout), res.err
	c := gitCapture{out: out, note: res.stdoutNote, total: len(out) + res.stdoutDropped}

	// `git diff --no-index` exits 1 when files differ, which is the normal
	// case here rather than a failure.
	if err != nil && len(out) > 0 {
		return c, nil
	}
	if err != nil {
		if msg := strings.TrimSpace(string(res.stderr)); msg != "" {
			if len(msg) > gitStderrPreview {
				msg = msg[:gitStderrPreview] + "…"
			}
			return gitCapture{}, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return gitCapture{}, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return c, nil
}

// changedFiles extracts the post-change paths from a unified diff.
//
// This is the Go-side reader of the same format crates/dc-verify parses. The
// duplication is deliberate and bounded: the Rust crate owns diff analysis for
// the verifier's CPU-bound gates, while this walks header lines only, to answer
// "which files" without a process boundary on every tool call.
func changedFiles(diff string) ([]string, error) {
	var files []string
	seen := map[string]bool{}
	add := func(p string) {
		path := policy.NormalizePlannedCandidate(p)
		if path != "" && !seen[path] {
			seen[path] = true
			files = append(files, path)
		}
	}

	var lastOldPath string
	for _, line := range strings.Split(diff, "\n") {
		if rest, found := strings.CutPrefix(line, "rename to "); found {
			add(strings.TrimSpace(rest))
			continue
		}
		if rest, found := strings.CutPrefix(line, "--- "); found {
			rest = strings.TrimSpace(rest)
			if rest != "/dev/null" {
				p := strings.TrimPrefix(rest, "a/")
				if tab := strings.IndexByte(p, '\t'); tab >= 0 {
					p = p[:tab]
				}
				lastOldPath = p
			} else {
				lastOldPath = ""
			}
			continue
		}
		if rest, found := strings.CutPrefix(line, "+++ "); found {
			rest = strings.TrimSpace(rest)
			if rest == "/dev/null" {
				// Deletion: the file that changed is the one in lastOldPath.
				if lastOldPath != "" {
					add(lastOldPath)
				}
			} else {
				p := strings.TrimPrefix(rest, "b/")
				if tab := strings.IndexByte(p, '\t'); tab >= 0 {
					p = p[:tab]
				}
				add(p)
			}
			lastOldPath = ""
			continue
		}
	}
	return files, nil
}

// --- evidence ---

func (r *Registry) getDiff(ctx context.Context, call tools.Call) tools.Result {
	diff, notes, err := gitDiff(ctx, r.deps.Root)
	if err != nil {
		return unavailable("working-tree diff", err)
	}
	// What the model is shown is filtered here and not in gitDiff, which the
	// verifier also reads: its secret scanner has to see a credential to flag
	// one. Untracked files are rendered into the same text, so a new .env is
	// withheld by the rule that withholds a modified one.
	diff, withheld := r.withholdDiff(diff)
	files, err := changedFiles(diff)
	if err != nil {
		return unavailable("diff parse", err)
	}
	out := map[string]any{"files": files, "diff": diff}
	if w := withheldPayload(withheld); w != nil {
		out["withheld"] = w
	}
	if len(notes) > 0 {
		// The model has to be able to tell a complete diff from a capped one;
		// acting on a partial diff as though it were whole is how a change is
		// reported finished with files nobody looked at.
		out["degraded"] = notes
	}
	return ok(out)
}

func (r *Registry) verify(ctx context.Context, call tools.Call) tools.Result {
	report, err := r.report(ctx)
	if err != nil {
		return unavailable("verification", err)
	}
	return reported(report, report)
}

func (r *Registry) getGaps(ctx context.Context, call tools.Call) tools.Result {
	report, err := r.report(ctx)
	if err != nil {
		return unavailable("gaps", err)
	}
	return reported(map[string]any{"gaps": report.Gaps, "blocking": report.BlockingCount()}, report)
}

func (r *Registry) getNextActions(ctx context.Context, call tools.Call) tools.Result {
	report, err := r.report(ctx)
	if err != nil {
		return unavailable("next actions", err)
	}
	return reported(map[string]any{"next_actions": report.NextActions}, report)
}

// reported carries a verification's degradations onto the tool result.
//
// The JSON body already names them, and that is what the model reads. This is
// the other reader: the session log and the run report are assembled from
// tools.Result, and a verification whose degradations lived only in the payload
// would arrive there looking exactly like one where every gate ran —
// Result.Qualified() would answer false for a run that skipped a check. The
// three tools below all project the same report, so all three carry it; a gap
// list assembled with a gate switched off is as incomplete as the verdict is.
func reported(payload any, report *Report) tools.Result {
	result := ok(payload)
	result.Degraded = report.Degraded
	return result
}
