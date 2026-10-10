package devcouncil

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// renderUntrackedBatch renders every untracked path with one `git diff`
// instead of one `git diff --no-index` per file, and reports whether it could.
//
// It marks the paths intent-to-add in a scratch copy of the repository's
// index, so git sees each as a new file, and diffs only those paths. That is
// the same addition `git diff --no-index -- /dev/null <path>` prints — the
// same header, mode, binary notice and hunks — for three processes in place of
// one per file: a tree with 256 new files was 258 spawns and is 5.
//
// It is an optimisation that must be exactly the old answer or not taken at
// all, so every doubt returns false and renderUntracked runs the per-file path:
//   - a path git lists as a directory (a nested repository), or a symlink to a
//     directory, which `--no-index` fails on and the batch would render: the
//     per-file path's failure is the answer to keep, not to improve on here;
//   - any git failure, including a diff that does not fit the capture bound;
//   - output whose per-file sections do not line up one-to-one, in order, with
//     the paths asked for. A section is found by its "diff --git" header, which
//     content cannot forge because content lines start with +, - or a space.
//
// The sections are then settled against the shared budget by settleUntracked,
// the function the per-file path uses, so a file cut by the budget carries the
// same bytes and the same note either way.
func renderUntrackedBatch(ctx context.Context, root string, paths []string, budget int) ([]untrackedRender, int, bool) {
	for _, path := range paths {
		if strings.HasSuffix(path, "/") || linksToDirectory(root, path) {
			return nil, 0, false
		}
	}
	dir, err := os.MkdirTemp("", "manvi-untracked-")
	if err != nil {
		return nil, 0, false
	}
	// The scratch index is this function's alone; see writeWorktreeTree.
	defer func() { _ = os.RemoveAll(dir) }()
	index := filepath.Join(dir, "index")

	// A repository with nothing staged has no index to copy; git reads a
	// missing GIT_INDEX_FILE as empty, which is the right starting point.
	seedScratchIndex(ctx, root, index)

	// --literal-pathspecs: these are file names, and a name like *star.go or
	// [brackets].go would otherwise be read as a pattern.
	add := append([]string{"--literal-pathspecs", "add", "--intent-to-add", "--"}, paths...)
	if _, _, err := runGitEnv(ctx, root, index, add...); err != nil {
		return nil, 0, false
	}
	diff := append([]string{"--literal-pathspecs", "diff", "--"}, paths...)
	out, _, err := runGitEnv(ctx, root, index, diff...)
	if err != nil {
		return nil, 0, false
	}

	sections := splitDiffSections(out)
	if len(sections) != len(paths) {
		return nil, 0, false
	}
	for i, section := range sections {
		if !strings.HasPrefix(section, "diff --git a/"+paths[i]+" b/"+paths[i]+"\n") {
			return nil, 0, false
		}
	}

	renders := make([]untrackedRender, 0, len(paths))
	for i, path := range paths {
		if budget <= 0 {
			return renders, len(paths) - i, true
		}
		section := sections[i]
		u := settleUntracked(ctx, root, path, budget, gitCapture{out: section, total: len(section)}, nil)
		if u.err == nil {
			budget -= len(u.added)
		}
		renders = append(renders, u)
	}
	return renders, 0, true
}

// splitDiffSections cuts a diff into one section per file. A header line is the
// only line that begins "diff --git ": every content line carries a +, - or
// space prefix, and a "\ No newline" marker begins with a backslash.
func splitDiffSections(diff string) []string {
	if diff == "" {
		return nil
	}
	var sections []string
	start := 0
	for i := 1; i < len(diff); i++ {
		if diff[i-1] == '\n' && strings.HasPrefix(diff[i:], "diff --git ") {
			sections = append(sections, diff[start:i])
			start = i
		}
	}
	return append(sections, diff[start:])
}

// linksToDirectory reports whether path is a symlink whose target is a
// directory, which `git diff --no-index` cannot render as an addition.
func linksToDirectory(root, path string) bool {
	full := filepath.Join(root, path)
	info, err := os.Lstat(full)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return false
	}
	target, err := os.Stat(full)
	return err == nil && target.IsDir()
}
