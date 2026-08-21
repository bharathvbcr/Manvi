package devcouncil

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"manvi/dc"
	"manvi/flags"
	"manvi/policy"
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

	for _, file := range changed {
		if policy.MatchesPlannedPath(file, inScope) {
			report.InScope = append(report.InScope, file)
			for _, p := range planned {
				if policy.MatchesPlannedPath(file, []dc.PlannedFile{{Path: p}}) {
					touched[p] = true
				}
			}
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

	report.Passed = report.BlockingCount() == 0
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

	client := rigorClient{Binary: r.deps.VerifierBinary}
	if r.deps.VerifierBinary == "" {
		return nil, nil, []string{owned + ": no dcverify binary configured — these checks did not run"}
	}
	// The coverage file is optional, and its absence is reported rather than
	// silently producing "everything unmeasured" — which is what the gate said
	// for every file before any measurements could be supplied.
	coverage := r.deps.CoverageFile
	result, err := client.run(ctx, diff, planned, coverage)
	if err != nil {
		return nil, nil, []string{fmt.Sprintf("%s: did not run (%v)", owned, err)}
	}
	if coverage == "" {
		degraded = append(degraded,
			"diff_coverage: no coverage file was supplied, so every changed file is reported unmeasured; "+
				"pass one with --coverage or MANVI_COVERAGE to get a real answer")
	}

	// verify.rigor.enabled, applied.
	//
	// This is the one setting in the harness that can make a verification
	// quieter without making the code better, so it is also the one that most
	// needs to say what it did. The gate's findings are dropped *and named*: an
	// operator who switches this off gets a run that checked less, not a run
	// that claims more than it checked, and a `passed: true` produced this way
	// is separable from one where every gate ran. Dropping the findings without
	// the degradation entry would reproduce the exact defect this setting was
	// inert for — a control that changes the answer and leaves no trace.
	if !r.settingOn(flags.VerifyRigorEnabled, true) {
		result.Findings = withoutGate(result.Findings, rigorStubGate)
		degraded = append(degraded, rigorStubGate+": suppressed by "+flags.VerifyRigorEnabled+
			"=false — this gate did not run, so nothing in this report is evidence "+
			"the added lines are free of placeholders or unimplemented bodies")
	}

	// verify.diff_coverage.enforce decides only whether a coverage finding
	// blocks. Both severities are computed from the same read so a gap and its
	// next action cannot end up disagreeing about whether the task is finished.
	gaps, actions := gapsFrom(taskID, result, r.settingOn(flags.VerifyDiffCoverageEnforce, false))
	return gaps, actions, degraded
}

// rigorStubGate is the dcverify gate verify.rigor.enabled governs.
//
// One gate, not the three that arrive over this boundary, and the narrowness is
// the decision. The setting's own description — stub, effort and coarse
// acceptance-proof detection on added diff lines — names crates/dc-verify's
// detect_stubs and nothing else. The other two are not its to switch off:
//
//   - secret_scan is the credential check. Wiring this setting as "skip the
//     verifier" would have taken that down with it, so an operator quieting a
//     noisy TODO gate would have stopped credential detection without being
//     told and without anything in the report connecting the two.
//   - diff_coverage answers a different question and has a setting of its own.
//
// If dcverify ever grows a second placeholder gate, it belongs in this list
// rather than in a second flag.
const rigorStubGate = "stub_detection"

// withoutGate drops every finding a named gate produced.
//
// It returns a new slice rather than filtering in place: the result it is
// handed is decoded from the verifier's reply, and a caller later reading the
// original for anything else should not find it silently shortened.
func withoutGate(findings []Finding, gate string) []Finding {
	kept := make([]Finding, 0, len(findings))
	for _, f := range findings {
		if f.Gate != gate {
			kept = append(kept, f)
		}
	}
	return kept
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

	tracked, err := runGit(ctx, root, "diff", "HEAD")
	if err != nil {
		// A repository with no commits yet has no HEAD; fall back to the index.
		tracked, err = runGit(ctx, root, "diff")
		if err != nil {
			return "", nil, err
		}
	}

	// -z is not a detail. Without it git separates paths with newlines and
	// quotes anything non-ASCII, and splitting that on whitespace loses every
	// path containing a space: the file vanishes from the diff, and a file that
	// is not in the diff is one the secret scanner and the coverage gate never
	// see. NUL-separated output has no such ambiguity.
	untracked, err := runGit(ctx, root, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return tracked, nil, nil
	}

	paths := make([]string, 0, 16)
	for _, path := range strings.Split(untracked, "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}

	var notes []string
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
	for _, path := range rendered {
		// Render an untracked file as an addition so one parser handles both.
		// The literal, not os.DevNull: this is git's own convention for the
		// empty side of a diff and it means the same thing on every platform,
		// whereas os.DevNull is "NUL" on Windows and git would read that as a
		// filename.
		added, err := runGit(ctx, root, "diff", "--no-index", "--", "/dev/null", path)
		if err != nil && added == "" {
			continue
		}
		b.WriteString(added)
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

func runGit(ctx context.Context, root string, args ...string) (string, error) {
	// core.quotePath=false makes git emit non-ASCII paths as raw UTF-8 instead
	// of C-style octal escapes. It is set here rather than in either parser
	// because crates/dc-verify reads this same format: unquoting on one side
	// only would give the two readers different answers for the same file.
	full := append([]string{"-c", "core.quotePath=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	// Without this, cancelling the context kills git but Output() keeps waiting
	// on a pipe any grandchild — a hook, a credential helper — still holds open,
	// so a bounded context still produces an unbounded wait.
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = root
	out, err := cmd.Output()
	// `git diff --no-index` exits 1 when files differ, which is the normal
	// case here rather than a failure.
	if err != nil && len(out) > 0 {
		return string(out), nil
	}
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
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
