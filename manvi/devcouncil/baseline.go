package devcouncil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	// seed is the scratch index Tree was written from, kept so the end-of-turn
	// diff can start from it instead of re-reading the whole working tree. Nil
	// when none was kept, which is every baseline not recorded by
	// CaptureBaseline, and then the diff takes the full route.
	seed *baselineSeed
}

// baselineSeed is the scratch index a baseline was recorded through: the
// working tree as it stood, with git's stat data for every file in it. It is
// held as bytes rather than as a file so that nothing on disk outlives the
// capture and there is no owner for a temp directory to be cleaned by.
type baselineSeed struct {
	// root is the repository the index belongs to. An index is meaningless in
	// any other.
	root  string
	index []byte
	// mtime is the modified time of the index file as git left it. It travels
	// with the bytes because git's racy-timestamp rule reads it: an entry whose
	// file changed in the same second the index was written cannot be trusted
	// on its stat data alone, and git knows that by comparing the entry to the
	// index file's own time. A copy that takes a newer time makes such an entry
	// look settled, and a same-sized edit made within that second is then never
	// seen.
	mtime time.Time
}

// maxSeedIndexBytes bounds what a baseline holds on to. An index past it is
// not kept, and the end-of-turn diff takes the full route instead; 32 MiB is
// a tree of several hundred thousand files, far past what a turn's check is
// meant to carry in memory.
const maxSeedIndexBytes = 32 << 20

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
	tree, index, mtime, err := r.recordWorktree(ctx, true, true)
	if err != nil {
		return Baseline{Note: fmt.Sprintf(
			"the pre-turn baseline could not be recorded (%v), so this turn's changes are "+
				"attributed against the last commit and may include work that was already "+
				"in the tree", err)}
	}
	b := Baseline{Tree: tree}
	if index != nil {
		b.seed = &baselineSeed{root: r.deps.Root, index: index, mtime: mtime}
	}
	return b
}

// treeAfter writes the tree to diff b.Tree against, for these paths only.
//
// Where the baseline kept its scratch index, that index is the start: it holds
// the whole tree as it stood, so only the named paths need reading again, and
// `add -A -- paths` brings exactly them up to date. The tree that comes out is
// b.Tree everywhere except those paths and the working tree on them, which is
// the whole of what a diff limited to those paths can see — so the diff is the
// one the full route gives on every tree whose index flags did not change
// during the turn, while the rest of the tree is not walked, hashed or compared.
//
// It is not the full route's answer when `update-index --assume-unchanged` or
// `--skip-worktree` is set on an examined path during the turn. The full route
// seeds from the repository's index as it is at the end of the turn, flag
// included, and `add` then skips the file, so it misses the turn's edit; this
// route starts from the index as it was when the baseline was taken, before the
// flag, and sees it, which is what a cold read of the tree gives. That is the
// more correct answer. A flag set before the baseline was taken is in both
// indexes, and both miss the edit; that miss predates this route and is not
// closed here.
//
// The pathspecs are passed exactly as the diff will pass them, not as literals:
// the two must select the same files, and a diff that reads a name as a glob
// against an add that reads it literally would compare a stale file to a fresh
// one. Anything the incremental route cannot do — no kept index, another
// repository, no paths, git refusing the add (an ignored path named outright is
// refused, as is one that exists nowhere) — falls back to the full route, which
// has no state to be wrong, and never to a guess.
func (r *Registry) treeAfter(ctx context.Context, b Baseline, paths []string) (string, error) {
	if tree, ok := r.treeFromSeed(ctx, b.seed, paths); ok {
		return tree, nil
	}
	return r.writeWorktreeTree(ctx)
}

// treeFromSeed is the incremental route of treeAfter. It reports false for
// anything that should be done the full way.
func (r *Registry) treeFromSeed(ctx context.Context, seed *baselineSeed, paths []string) (string, bool) {
	if seed == nil || seed.root != r.deps.Root || len(paths) == 0 {
		return "", false
	}
	dir, err := os.MkdirTemp("", "manvi-after-")
	if err != nil {
		return "", false
	}
	defer func() { _ = os.RemoveAll(dir) }()
	index := filepath.Join(dir, "index")
	if err := os.WriteFile(index, seed.index, 0o600); err != nil {
		return "", false
	}
	if err := os.Chtimes(index, seed.mtime, seed.mtime); err != nil {
		return "", false
	}
	tree, err := r.addAndWriteTree(ctx, index, paths...)
	if err != nil {
		return "", false
	}
	return tree, true
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
	tree, _, _, err := r.recordWorktree(ctx, seed, false)
	return tree, err
}

// recordWorktree is worktreeTree that can also hand back the scratch index the
// tree was written from, with its modified time, for a caller that will diff
// against it later. The bytes are nil when keep is false or the index is too
// large to hold; the tree is the same either way.
func (r *Registry) recordWorktree(ctx context.Context, seed, keep bool) (string, []byte, time.Time, error) {
	dir, err := os.MkdirTemp("", "manvi-baseline-")
	if err != nil {
		return "", nil, time.Time{}, err
	}
	// The scratch index and its directory are this function's alone; a failure
	// to remove them leaks a temp directory and nothing else, and there is no
	// caller who could act on it.
	defer func() { _ = os.RemoveAll(dir) }()
	index := filepath.Join(dir, "index")

	written := func(tree string) (string, []byte, time.Time, error) {
		if !keep {
			return tree, nil, time.Time{}, nil
		}
		raw, mtime := readSeedIndex(index)
		return tree, raw, mtime, nil
	}

	if seed && seedScratchIndex(ctx, r.deps.Root, index) {
		if tree, err := r.addAndWriteTree(ctx, index); err == nil {
			return written(tree)
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
			return "", nil, time.Time{}, err
		}
	}
	tree, err := r.addAndWriteTree(ctx, index)
	if err != nil {
		return "", nil, time.Time{}, err
	}
	return written(tree)
}

// readSeedIndex reads a finished scratch index to keep, with the modified time
// git left on it, or returns nil bytes when it cannot be read or is past
// maxSeedIndexBytes. Nil is not an error: a baseline without a kept index is a
// complete baseline, and only the later diff loses its shortcut.
func readSeedIndex(path string) ([]byte, time.Time) {
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxSeedIndexBytes {
		return nil, time.Time{}
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- the path is this package's own scratch index
	if err != nil {
		return nil, time.Time{}
	}
	return raw, info.ModTime()
}

// addAndWriteTree turns the scratch index into a tree object, after bringing it
// up to date with the working tree: all of it when no paths are named, and only
// the named paths otherwise.
func (r *Registry) addAndWriteTree(ctx context.Context, index string, paths ...string) (string, error) {
	// -A so an untracked file the operator left in the tree is part of the
	// baseline. Without it the agent's first write to that path would read as
	// the agent having created it.
	//
	// `--` is load-bearing when paths are named, for the reason scopedDiff gives:
	// without it a path that matches a revision name is read as one.
	args := []string{"add", "-A"}
	if len(paths) > 0 {
		args = append(append(args, "--"), paths...)
	}
	if _, stderr, err := runGitEnv(ctx, r.deps.Root, index, args...); err != nil {
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
