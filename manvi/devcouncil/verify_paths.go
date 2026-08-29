package devcouncil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"manvi/policy"
)

// This file is the check for a turn that holds no task lease.
//
// devcouncil_verify_task cannot be that check, and the reason is structural
// rather than a missing feature: report() begins by resolving the current task
// and fails without one. Most work does not run under a lease — `manvi run -p
// "fix the flaky test"` has no task, and under a relaxed posture its writes
// land anyway — so making the leased verifier the universal end-of-turn check
// would degrade or error on the ordinary path, and a check that errors on the
// ordinary path gets switched off.
//
// The other half of the same problem is what the check runs against. gitDiff
// diffs the whole tree against HEAD, which includes whatever the operator had
// uncommitted before the turn began. Reporting on that is reporting on someone
// else's work: the turn gets blamed for edits it did not make, and — worse —
// a turn that wrote nothing looks busy. So this one is scoped to the paths the
// turn's own handlers said they changed. That set is exact where `git diff` is
// merely nearby.
//
// One rule runs through all of it: a check that could not run never reports
// what a check that ran and passed reports. Every path out of here that did not
// actually examine something says so, in Degraded, and the verdict is degraded
// rather than passed.

// PathReport is a lease-free verification of one turn's own changes.
type PathReport struct {
	// Verdict is passed, failed or degraded. It is never empty: this type is
	// only produced by a check that was attempted.
	Verdict string `json:"verdict"`
	// Source names what produced the verdict, so a reader can tell a project's
	// own command from the built-in gates.
	Source string `json:"source"`
	// Examined lists the paths the check actually looked at.
	Examined []string `json:"examined,omitempty"`
	// Skipped lists paths deliberately not examined, each with its reason.
	// Carried rather than dropped: the difference between a short Examined list
	// and a short list of changes is the difference between partial coverage
	// and a quiet turn, and nothing downstream can reconstruct which it has.
	Skipped []string `json:"skipped,omitempty"`
	// Findings are what the check objected to, in the words of whatever
	// produced them.
	Findings []string `json:"findings,omitempty"`
	// Degraded names checks that were owed and did not run.
	Degraded []string `json:"degraded,omitempty"`
}

// Passed reports a clean verdict. A degraded report is not one, whatever else
// it found: the checks that did not run might have been the ones that mattered.
func (p PathReport) Passed() bool { return p.Verdict == VerdictPassed }

// The verdicts this check can reach. Degraded is a distinct value rather than a
// flag on a pass, because every caller that has to remember to check a flag
// eventually forgets to.
const (
	VerdictPassed   = "passed"
	VerdictFailed   = "failed"
	VerdictDegraded = "degraded"
)

// verifyCommandTimeout bounds an operator-supplied verification command.
//
// Five minutes, matching devcouncil_exec_command: this runs a project's own
// test or lint command, which is the same class of work with the same tail. It
// is a bound rather than a guideline — an unbounded check at the end of every
// mutating turn is a harness that can hang on its own verification.
const verifyCommandTimeout = 5 * time.Minute

// maxVerifyCommandOutputRunes bounds how much of a failing command's output
// reaches the model. Enough for a compiler's first errors, far short of a test
// suite's full log — which would evict the conversation the fix depends on.
const maxVerifyCommandOutputRunes = 4000

// maxVerifiedPaths bounds how many paths one check examines.
//
// Argument lists are finite and diffs are not free, and past a point the
// answer stops being actionable anyway. The cap reports itself in Skipped: a
// truncated examination presented as a complete one is precisely how files come
// to be recorded as checked without anything having read them.
const maxVerifiedPaths = 128

// VerifyPaths runs the end-of-turn check against the paths a turn changed.
//
// command, when non-empty, is a verification command supplied by the operator.
// It is a parameter rather than a setting this package reads for itself, and
// that is a security boundary, not a style choice: this harness's settings load
// from .devcouncil/config.yaml, the restricted-path rung that protects that
// file lives inside the hard-rules block, and a relaxed posture turns that
// block off. A command sourced from the settings registry would therefore be a
// command the agent can write and the harness then executes with its own
// authority, every turn, outside the gate the agent's own commands pass. The
// caller must take it from operator scope — the process environment or the
// command line — and nowhere else.
func (r *Registry) VerifyPaths(ctx context.Context, paths []string, command string) PathReport {
	report := PathReport{Verdict: VerdictPassed, Source: "path-scoped gates"}

	examined, skipped := r.partitionPaths(paths)
	report.Examined = examined
	report.Skipped = skipped

	// A command the operator supplied is the strongest signal available: it is
	// the project's own definition of "this works". It runs whether or not any
	// path survived filtering, because a turn that only deleted files, or only
	// changed something this cannot diff, can still break a build.
	if strings.TrimSpace(command) != "" {
		report.Source = "operator verification command"
		r.runVerifyCommand(ctx, command, &report)
	}

	if len(examined) == 0 {
		if report.Source == "path-scoped gates" {
			// Nothing to look at and no command to run. Not a pass: the turn
			// reported changes, and every one of them was filtered out, so
			// nothing here is evidence about any of them.
			report.Degraded = append(report.Degraded,
				"no changed path could be examined, so nothing was verified")
		}
		return report.settle()
	}

	diff, notes, err := r.scopedDiff(ctx, examined)
	if err != nil {
		report.Degraded = append(report.Degraded,
			fmt.Sprintf("the scoped diff could not be produced (%v), so the content gates did not run", err))
		return report.settle()
	}
	report.Degraded = append(report.Degraded, notes...)
	if strings.TrimSpace(diff) == "" {
		// The handlers said they wrote these files and git sees no change in
		// them. That is not nothing: a write of identical bytes, or a path
		// already committed by something else in the turn. Reported as a
		// degradation rather than a pass, because the gates below read this
		// diff and an empty one gives them nothing to judge.
		report.Degraded = append(report.Degraded,
			"the changed paths produced an empty diff, so the content gates had nothing to read")
		return report.settle()
	}

	// The same gates the leased verifier uses, from the same function. planned
	// is the examined set, so nothing here is an orphan: this check asks
	// whether the content is sound, not whether it was in a plan — there is no
	// plan without a task.
	gaps, _, degraded := r.runRigor(ctx, diff, examined, "turn")
	report.Degraded = append(report.Degraded, degraded...)
	for _, gap := range gaps {
		if !gap.Blocking {
			continue
		}
		detail := gap.Detail
		if gap.File != "" {
			detail = gap.File + ": " + detail
		}
		report.Findings = append(report.Findings, detail)
	}

	return report.settle()
}

// settle decides the final verdict from what the report actually holds.
//
// One place, called on every path out, because the alternative was every branch
// assigning a verdict for itself — and one of them did exactly the damage that
// arrangement invites: a branch reached for "degraded" and overwrote a failure
// that had already been recorded, so a build that did not compile was reported
// as merely unverified.
//
// The ordering is the rule this file exists for, in three lines. A finding is a
// failure, whatever else is true. A check that was owed and could not run is
// never a pass. Only a report with neither passes.
func (p PathReport) settle() PathReport {
	switch {
	case len(p.Findings) > 0:
		p.Verdict = VerdictFailed
	case len(p.Degraded) > 0:
		p.Verdict = VerdictDegraded
	default:
		p.Verdict = VerdictPassed
	}
	return p
}

// partitionPaths splits a turn's changed paths into what this check can examine
// and what it cannot, keeping the reason with each exclusion.
func (r *Registry) partitionPaths(paths []string) (examined, skipped []string) {
	seen := map[string]bool{}
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true

		if len(examined) >= maxVerifiedPaths {
			skipped = append(skipped, fmt.Sprintf(
				"%s: more than %d paths changed; this check examined the first %d",
				p, maxVerifiedPaths, maxVerifiedPaths))
			continue
		}

		normalized, outside := policy.NormalizeRepoPath(r.deps.Root, p)
		if outside {
			// Reachable only with hard rules off. The gates read a repository
			// diff, and a path outside the repository is not in one.
			skipped = append(skipped, p+": outside the repository root")
			continue
		}
		// The harness's own bookkeeping. Artifacts are plan documents, not
		// source, and running a stub detector over a plan produces findings
		// about prose. Named rather than silently dropped so a reader can see
		// the filter rather than infer it from a short list.
		if isHarnessPath(normalized) {
			skipped = append(skipped, normalized+": harness bookkeeping, not repository source")
			continue
		}
		examined = append(examined, normalized)
	}
	sort.Strings(examined)
	return examined, skipped
}

// isHarnessPath reports a path under this harness's own state directory.
//
// Compared by path component rather than by string prefix: ".devcouncilish/x"
// starts with ".devcouncil" and is an ordinary repository file, and a filter
// that excluded it would quietly stop verifying real source.
func isHarnessPath(normalized string) bool {
	parts := strings.Split(filepath.ToSlash(normalized), "/")
	return len(parts) > 0 && parts[0] == ".devcouncil"
}

// scopedDiff produces the diff for exactly these paths.
//
// `--` is load-bearing and not defensive habit: without it a path that happens
// to match a branch or tag name is read by git as a revision, and the diff
// comes back describing something else entirely.
func (r *Registry) scopedDiff(ctx context.Context, paths []string) (string, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	args := append([]string{"diff", "HEAD", "--"}, paths...)
	out, note, err := runGitCapped(ctx, r.deps.Root, maxGitCaptureBytes, args...)
	if err != nil {
		// No HEAD yet — a repository with no commits. The index is the only
		// baseline there is.
		args = append([]string{"diff", "--"}, paths...)
		out, note, err = runGitCapped(ctx, r.deps.Root, maxGitCaptureBytes, args...)
		if err != nil {
			return "", nil, err
		}
	}
	var notes []string
	budget := maxGitCaptureBytes
	if note != "" {
		// Cut back to a whole line first: a diff header sliced in half names a
		// file that does not exist, and the gates would report on a path
		// nobody wrote.
		out = trimPartialLine(out)
		notes = append(notes, fmt.Sprintf(
			"scoped_diff: %s; the gates read only the part above that cap, so the rest of the "+
				"change was not covered", note))
	}
	budget -= len(out)

	// A file the turn created is untracked, and `git diff HEAD` says nothing
	// about an untracked file. Leaving it there would have made the most
	// ordinary thing an agent does — write a new file — produce an empty diff
	// and therefore a report that verified nothing, on every turn, while
	// looking like a check that ran.
	untracked, err := r.untrackedAmong(ctx, paths)
	if err != nil {
		notes = append(notes, fmt.Sprintf(
			"scoped_diff: the untracked-file listing failed (%v), so any file this turn created "+
				"is not in this diff and the gates did not see it", err))
		return out, notes, nil
	}

	var b strings.Builder
	b.WriteString(out)
	var unrendered int
	for i, path := range untracked {
		if budget <= 0 {
			unrendered = len(untracked) - i
			break
		}
		// The literal /dev/null, which is git's own convention for the empty
		// side of a diff and means the same on every platform — os.DevNull is
		// "NUL" on Windows and git would read that as a filename.
		added, addNote, addErr := runGitCapped(ctx, r.deps.Root, budget,
			"diff", "--no-index", "--", "/dev/null", path)
		if addErr != nil && added == "" {
			// --no-index exits non-zero whenever it finds a difference, which
			// is every time here, so an error with output is the ordinary
			// case. An error with no output is a real one.
			notes = append(notes, fmt.Sprintf(
				"scoped_diff: %s could not be rendered (%v), so the gates did not read it",
				path, addErr))
			continue
		}
		if addNote != "" {
			added = trimPartialLine(added)
			notes = append(notes, fmt.Sprintf(
				"scoped_diff: %s %s; the gates saw only the part above the cut", path, addNote))
		}
		budget -= len(added)
		b.WriteString(added)
	}
	if unrendered > 0 {
		notes = append(notes, fmt.Sprintf(
			"scoped_diff: the %d-byte capture budget ran out with %d new file(s) still unrendered; "+
				"they are not in this diff and the gates did not see them",
			maxGitCaptureBytes, unrendered))
	}
	return b.String(), notes, nil
}

// untrackedAmong returns which of these paths git does not track.
//
// Scoped to the given paths rather than listing the whole tree: the operator's
// own new files are none of this check's business, and rendering them would put
// work the turn did not do in front of the gates.
func (r *Registry) untrackedAmong(ctx context.Context, paths []string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	// -z because git otherwise separates paths with newlines and quotes
	// anything non-ASCII, and splitting that loses every path with a space in
	// it — a file that is not in the diff is one the gates never see.
	args := append([]string{"ls-files", "-z", "--others", "--exclude-standard", "--"}, paths...)
	out, err := runGit(ctx, r.deps.Root, args...)
	if err != nil {
		return nil, err
	}
	var found []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			found = append(found, p)
		}
	}
	return found, nil
}

// runVerifyCommand runs the operator's own check and folds its result into the
// report.
//
// Exit status is the verdict. A command that cannot be started at all is
// degraded rather than failed: "the build is broken" and "the checker is not
// installed" are different facts, and collapsing them teaches an operator to
// ignore the one that matters.
func (r *Registry) runVerifyCommand(ctx context.Context, command string, report *PathReport) {
	cmdCtx, cancel := context.WithTimeout(ctx, verifyCommandTimeout)
	defer cancel()

	out, exit, timedOut, err := runShell(cmdCtx, r.deps.Root, command)
	switch {
	case timedOut:
		report.Verdict = VerdictDegraded
		report.Degraded = append(report.Degraded, fmt.Sprintf(
			"the verification command did not finish within %s, so it proved nothing",
			verifyCommandTimeout))
		return
	case err != nil:
		report.Verdict = VerdictDegraded
		report.Degraded = append(report.Degraded, fmt.Sprintf(
			"the verification command could not be started (%v), so it did not run", err))
		return
	case exit != 0:
		report.Verdict = VerdictFailed
		report.Findings = append(report.Findings, fmt.Sprintf(
			"the verification command exited %d:\n%s",
			exit, truncateOutput(strings.TrimSpace(out))))
		return
	}
	// Exit zero. The verdict stays as it was — a later gate may still fail it —
	// and this is deliberately not recorded as evidence about paths the command
	// did not look at, because nothing here knows what it looked at.
}

// truncateOutput bounds a command's output on rune boundaries, keeping the tail
// as well as the head: a compiler prints the summary last, and a head-only cut
// throws away the count of what went wrong.
func truncateOutput(s string) string {
	runes := []rune(s)
	if len(runes) <= maxVerifyCommandOutputRunes {
		return s
	}
	head := maxVerifyCommandOutputRunes * 2 / 3
	tail := maxVerifyCommandOutputRunes - head
	return string(runes[:head]) +
		fmt.Sprintf("\n… (%d characters omitted) …\n", len(runes)-maxVerifyCommandOutputRunes) +
		string(runes[len(runes)-tail:])
}

// ExistingPaths keeps only the paths that are still present on disk, which is
// what a checker that opens files needs and what a deletion breaks.
//
// Exported because the caller assembling a check has to be able to tell a
// deleted file from an unreadable one, and doing that by stat'ing paths itself
// would put a second answer to "where is the repository root" outside this
// package.
func (r *Registry) ExistingPaths(paths []string) (present, missing []string) {
	for _, p := range paths {
		normalized, outside := policy.NormalizeRepoPath(r.deps.Root, p)
		if outside {
			missing = append(missing, p)
			continue
		}
		full := filepath.Join(r.deps.Root, filepath.FromSlash(normalized))
		if _, err := os.Stat(full); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				// An unreadable path is not a deleted one, and the two must not
				// be folded: a permission error that reads as "deleted" is a
				// file dropped silently out of every check downstream.
				missing = append(missing, fmt.Sprintf("%s (unreadable: %v)", normalized, err))
				continue
			}
			missing = append(missing, normalized)
			continue
		}
		if !slices.Contains(present, normalized) {
			present = append(present, normalized)
		}
	}
	return present, missing
}
