package devcouncil

import (
	"context"
	"fmt"
	"strings"
)

// The untracked-file fan-out in gitDiff and scopedDiff used to run one git
// after another. What follows is that code as it stood, kept verbatim and
// renamed, so the equivalence tests in untracked_render_test.go compare the
// parallel renderer against the real thing rather than against a description
// of it. Do not "fix" anything here: its value is that it is the old answer.

func gitDiffSequential(ctx context.Context, root string) (string, []string, error) {
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
	budget := maxGitCaptureBytes
	var cutFiles []string
	unrendered := 0
	for i, path := range rendered {
		if budget <= 0 {
			unrendered = len(rendered) - i
			break
		}
		// Render an untracked file as an addition so one parser handles both.
		// The literal, not os.DevNull: this is git's own convention for the
		// empty side of a diff and it means the same thing on every platform,
		// whereas os.DevNull is "NUL" on Windows and git would read that as a
		// filename.
		added, note, err := runGitCapped(ctx, root, budget, "diff", "--no-index", "--", "/dev/null", path)
		if err != nil && added == "" {
			continue
		}
		if note != "" {
			added = trimPartialLine(added)
			// The path and the byte counts together: "some of this file is
			// missing" is not actionable, and a note that carries only one of
			// the two numbers is how a capped sample gets read as a whole one.
			cutFiles = append(cutFiles, fmt.Sprintf("%s %s", path, note))
		}
		budget -= len(added)
		b.WriteString(added)
	}
	if len(cutFiles) > 0 {
		notes = append(notes, fmt.Sprintf(
			"untracked_diff: %d untracked file(s) were cut at the %d-byte capture budget: %s; "+
				"the gates saw only the part above the cut",
			len(cutFiles), maxGitCaptureBytes, strings.Join(cutFiles, "; ")))
	}
	if unrendered > 0 {
		notes = append(notes, fmt.Sprintf(
			"untracked_diff: the %d-byte capture budget ran out with %d untracked file(s) still "+
				"unrendered; they are not in this diff and the gates did not see them",
			maxGitCaptureBytes, unrendered))
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

func (r *Registry) scopedDiffSequential(ctx context.Context, paths []string, baseline Baseline) (string, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	// With a baseline, the comparison is tree against tree: the state the turn
	// started from against the state it left. That is the agent's change and
	// nothing else — the operator's earlier edits are on both sides and cancel,
	// and a file the turn created is in the second tree, so the untracked
	// special case below is not needed for this path either.
	if baseline.Captured() {
		if after, err := r.writeWorktreeTree(ctx); err == nil {
			args := append([]string{"diff", baseline.Tree, after, "--"}, paths...)
			out, note, err := runGitCapped(ctx, r.deps.Root, maxGitCaptureBytes, args...)
			if err == nil {
				var notes []string
				if note != "" {
					out = trimPartialLine(out)
					notes = append(notes, fmt.Sprintf(
						"scoped_diff: %s; the gates read only the part above that cap, so the "+
							"rest of the change was not covered", note))
				}
				return out, notes, nil
			}
		}
		// Falling through is not silent: the note appended below says the
		// diff that follows is against the last commit, not the baseline.
	}

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
	if baseline.Captured() {
		notes = append(notes, "scoped_diff: the post-turn tree could not be written, so this "+
			"diff is taken against the last commit and may include work that was already in "+
			"the tree when the turn began")
	}
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

// renderUntrackedSequential is gitDiff's old loop lifted out on its own, the
// budget made a parameter so a test can make it run out after a few hundred
// bytes rather than sixteen megabytes. The body is the loop as it was.
func renderUntrackedSequential(ctx context.Context, root string, rendered []string, budget int) ([]untrackedRender, int) {
	var renders []untrackedRender
	unrendered := 0
	for i, path := range rendered {
		if budget <= 0 {
			unrendered = len(rendered) - i
			break
		}
		added, note, err := runGitCapped(ctx, root, budget, "diff", "--no-index", "--", "/dev/null", path)
		if err != nil && added == "" {
			renders = append(renders, untrackedRender{path: path, err: err})
			continue
		}
		if note != "" {
			added = trimPartialLine(added)
		}
		budget -= len(added)
		renders = append(renders, untrackedRender{path: path, added: added, note: note})
	}
	return renders, unrendered
}
