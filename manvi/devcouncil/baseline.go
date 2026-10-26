package devcouncil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Baseline is the working tree as it stood at a moment in time, recorded as a
// git tree object so a later state can be diffed against it.
//
// It exists because `git diff HEAD` answers the wrong question. HEAD is the
// last commit, not the state the turn started from, so a diff taken against it
// carries whatever the operator had already changed and not committed. The
// agent edits shared.txt, the operator had edited shared.txt an hour earlier,
// and the change receipt claims both — which is wrong about attribution, wrong
// about review, and wrong about what "the agent's change passed" certifies.
//
// A tree object is the right shape for this. Writing one costs an index pass
// and no commit, nothing in the repository moves, and the operator's staged and
// unstaged work is left exactly where it was: this records the tree, it does
// not stash, reset or absorb it.
type Baseline struct {
	// Tree is a git tree object id, empty when none could be captured.
	Tree string
	// Note says why there is no tree, when there is none. It is never empty
	// while Tree is: a caller has to be able to tell "no changes to attribute"
	// from "attribution is unavailable", and an empty struct says neither.
	Note string
}

// Captured reports whether this baseline can be diffed against.
func (b Baseline) Captured() bool { return b.Tree != "" }

// CaptureBaseline records the current working tree.
//
// The whole operation runs against a temporary index file, so the repository's
// real index is untouched — an agent turn must not stage the operator's work as
// a side effect of being observed. The tree objects it writes are unreachable
// and are collected by git's own gc, like any other transient object.
//
// A failure is returned as a Baseline carrying a Note rather than as an error.
// The caller's next move is the same either way — fall back and say the
// attribution is degraded — and a check that could not run must report that,
// not abort the turn it was observing.
func (r *Registry) CaptureBaseline(ctx context.Context) Baseline {
	tree, err := r.writeWorktreeTree(ctx)
	if err != nil {
		return Baseline{Note: fmt.Sprintf(
			"the pre-turn baseline could not be recorded (%v), so this turn's changes are "+
				"attributed against the last commit and may include work that was already "+
				"in the tree", err)}
	}
	return Baseline{Tree: tree}
}

// writeWorktreeTree writes the working tree — tracked changes and untracked
// files alike — into a tree object, using a scratch index.
//
// The scratch index is seeded from a copy of the repository's own, so `add -A`
// finds the stat data git already recorded and re-hashes only the files that
// changed. Seeding from HEAD instead gives an index with no stat cache, and
// `add -A` then reads and hashes every file in the tree at the start of every
// turn. Both routes end at the same tree — `add -A` makes the index equal the
// working tree whatever it started as — so a seed that cannot be taken is not
// an error: the baseline falls back to the cold route rather than failing.
func (r *Registry) writeWorktreeTree(ctx context.Context) (string, error) {
	return r.worktreeTree(ctx, true)
}

// worktreeTree is writeWorktreeTree with the seed optional. The cold route
// (seed false) is the oracle the tree-id test and the benchmark compare the
// seeded route against.
func (r *Registry) worktreeTree(ctx context.Context, seed bool) (string, error) {
	dir, err := os.MkdirTemp("", "manvi-baseline-")
	if err != nil {
		return "", err
	}
	// The scratch index and its directory are this function's alone; a failure
	// to remove them leaks a temp directory and nothing else, and there is no
	// caller who could act on it.
	defer func() { _ = os.RemoveAll(dir) }()
	index := filepath.Join(dir, "index")

	if seed && seedScratchIndex(ctx, r.deps.Root, index) {
		if tree, err := r.addAllAndWriteTree(ctx, index); err == nil {
			return tree, nil
		}
		// A seeded index that git then refuses is not trusted: start over from
		// HEAD, which is the route that has no inherited state to be wrong.
		_ = os.Remove(index)
	}

	// read-tree seeds the scratch index from HEAD so that files unchanged since
	// the last commit keep their existing blobs and `add` has little to do. A
	// repository with no commits has no HEAD, and starting from an empty index
	// is the correct baseline there rather than a failure.
	if _, _, err := runGitEnv(ctx, r.deps.Root, index, "read-tree", "HEAD"); err != nil {
		if _, _, err := runGitEnv(ctx, r.deps.Root, index, "read-tree", "--empty"); err != nil {
			return "", err
		}
	}
	return r.addAllAndWriteTree(ctx, index)
}

// addAllAndWriteTree turns whatever the scratch index holds into the working
// tree's tree object.
func (r *Registry) addAllAndWriteTree(ctx context.Context, index string) (string, error) {
	// -A so an untracked file the operator left in the tree is part of the
	// baseline. Without it the agent's first write to that path would read as
	// the agent having created it.
	if _, stderr, err := runGitEnv(ctx, r.deps.Root, index, "add", "-A"); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr))
	}
	out, stderr, err := runGitEnv(ctx, r.deps.Root, index, "write-tree")
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr))
	}
	tree := strings.TrimSpace(out)
	if tree == "" {
		return "", fmt.Errorf("git write-tree produced no object id")
	}
	return tree, nil
}

// seedScratchIndex copies the repository's index to dst and reports whether it
// did. The path comes from git, not from joining ".git/index": in a linked
// worktree the index lives under the shared git dir's worktrees/ entry, and a
// guessed path would seed from another checkout's index.
func seedScratchIndex(ctx context.Context, root, dst string) bool {
	out, _, err := runGitEnv(ctx, root, "", "rev-parse", "--git-path", "index")
	if err != nil {
		return false
	}
	src := strings.TrimSpace(out)
	if src == "" {
		return false
	}
	if !filepath.IsAbs(src) {
		src = filepath.Join(root, src)
	}
	info, err := os.Stat(src)
	if err != nil {
		return false
	}
	raw, err := os.ReadFile(src) // #nosec G304 -- the path is git's own answer for this repository
	if err != nil {
		return false
	}
	if os.WriteFile(dst, raw, 0o600) != nil {
		return false
	}
	// The copy keeps the original's modified time. Git decides whether an
	// entry's stat data can be trusted by comparing it to the index file's own
	// time, so a copy stamped "now" turns every entry written in the last second
	// before the original into one that looks settled — and a same-sized edit
	// made in that second is then hashed as the old content. A seed that cannot
	// be stamped is not taken: the cold route is slower and has no such state.
	return os.Chtimes(dst, info.ModTime(), info.ModTime()) == nil
}
