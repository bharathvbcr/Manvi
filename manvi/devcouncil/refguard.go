package devcouncil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// The post-exec ref guard.
//
// devcouncil_exec_command may run `git commit` (the owner's policy: hooks and
// --no-verify included), and a command string is not a security boundary: sh
// builds the words it runs at run time (`git \<nl>commit`, `git${IFS}commit`,
// `{git,commit}`, positional parameters, `xargs git`, `| sh`), and anything
// else can commit for it (a renamed git, an interpreter, make, a script, a
// pre-commit hook that stages after a pre-exec check, commit-tree + update-ref).
// So the check is not made on the command. It is made on the outcome:
//
//  1. Before the shell runs, every ref in the repository at Registry root is
//     recorded (refs, refs/stash, and a detached HEAD).
//  2. After it returns, whatever the way it ended, every ref that changed or
//     appeared is read, and the commits newly reachable from it, and not from
//     any recorded oid, are listed (at most maxGuardCommits; more is an
//     unscanned set, reported as such and never scanned in part).
//  3. Each such commit is judged by treeContentLeaks, the commit tool's own
//     content scanner, against its parent(s), and by the secret-path names.
//  4. Only a ref whose new commits leak is rolled back, to its recorded oid
//     with `git update-ref <ref> <old> <current>` (a ref that did not exist is
//     deleted with `update-ref -d <ref> <current>`), so a ref an operator moved
//     meanwhile is not clobbered. The result is a hard violation naming the
//     ref, the commit and the files, never the value.
//
// A snapshot, scan or restore that fails is a violation too, with the exact
// ref and oid state and "manual intervention needed". It is never a pass. A
// snapshot that cannot be taken in a repository means the command is not run.
// A directory that is not a git repository is not guarded and does not fail.
//
// Two different results block. A commit that leaks is path.secret. A set the
// guard could not scan (over the cap, a timeout, an unreadable ref or worktree)
// is ruleRefUnscanned: the same hard block, because an unexamined commit is not
// a clean one, but never labelled or worded as a credential found.
//
// The index and working tree are not touched by a rollback: what a leaking
// commit recorded stays staged, like a commit the tool refused.
//
// Residuals, stated because the guard does not cover them:
//   - commit and push in one command line: the objects have left before the
//     check. The guard detects it and names the remote when a leaking ref is a
//     remote-tracking ref, and says so when the command mentions push or
//     send-pack. A plain `git push` is allowed by the command policy (only
//     force pushes are denied); the guard cannot un-send it.
//   - dangling objects and the reflog entry of the rolled-back update keep the
//     secret until git expires them;
//   - a repository other than the one at Registry root (a fresh `git init`, a
//     clone elsewhere); linked worktrees share refs and are covered;
//   - an object written but never referenced (hash-object -w, commit-tree
//     without update-ref), which no ref reaches;
//   - a leak committed between calls (by a process that outlived a call, or by
//     the operator) is reported on the next call, not rolled back: refGuard
//     keeps the last call's refs as a baseline and the harness never rewrites
//     what may be the operator's refs;
//   - the secret set is read after the command, from the files as they are then;
//   - a two-step disable: `mv .git .gitx`, a command run in what is then a
//     non-repository (not guarded), `mv .gitx .git`, then the commit. Same class
//     as commit-and-push: the guard looks at refs around one command, not at what
//     happens between two;
//   - a linked worktree that was never readable is skipped (with a note), so a
//     per-worktree ref it holds is unseen until it is readable again (see
//     snapshotRefs for why that loses nothing reachable through a shared ref);
//   - rollback is per Registry, not per author. Overlap with another writer is
//     detected only between exec_command calls of one Registry, so a leaking
//     commit that an operator or a sub-agent of another Registry made during
//     this command's window is rolled back with the command's own. The guard
//     cannot tell whose commit it is, because attribution needs an identity
//     (committer, session) that a ref snapshot does not carry. Scoping rollback
//     to attributable commits is an owner decision, not made here.
const maxGuardCommits = 200

// ruleRefUnscanned is the rule id of a guard result that is not a finding: the
// guard could not read or scan what a command (or the interval between two
// commands) made reachable, so it cannot say whether a credential is in it. It
// blocks like a leak does (hard, no grant), because an unexamined commit is not
// a clean one, but it is a different rule on purpose: path.secret says a
// credential was found, and a fetch of 300 foreign commits found none.
const ruleRefUnscanned = "ref.unscanned"

var pushWord = regexp.MustCompile(`\b(push|send-pack)\b`)

// refSnapshot is every ref's oid at one moment; a detached HEAD is "HEAD".
type refSnapshot struct {
	refs map[string]string
	// scanned is the linked worktrees whose own refs were read; skipped is the
	// ones that were not, with why. See snapshotRefs.
	scanned map[string]bool
	skipped map[string]string
}

func (r *Registry) snapshotRefs(ctx context.Context) (*refSnapshot, error) {
	out, err := runGit(ctx, r.deps.Root, "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		// Whether this is a repository is not read from git's message, which is
		// translated by locale (and by the fake or wrapper a PATH may supply). It
		// is read from the tree: git finds a repository through a .git entry in
		// the directory or an ancestor, or because the directory is itself a bare
		// one, and a directory with neither has nothing to guard. Any other
		// failure, including one inside a repository, is an error: the command
		// is not run.
		if !insideRepository(r.deps.Root) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing refs: %w", err)
	}
	snap := &refSnapshot{refs: map[string]string{}, scanned: map[string]bool{}, skipped: map[string]string{}}
	if err := addRefRecords(snap, "", out); err != nil {
		return nil, err
	}
	// Every worktree has a HEAD of its own, and a detached one is a ref that
	// for-each-ref does not list. A symbolic HEAD is its branch, already listed.
	// The per-worktree namespaces of the other worktrees are not listed from
	// here either, so each is read from its own directory.
	list, err := runGit(ctx, r.deps.Root, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("listing worktrees: %w", err)
	}
	for i, wt := range parseWorktrees(list) {
		if wt.detached && wt.head != "" {
			snap.refs[worktreeKey(wt.path, "HEAD")] = wt.head
		}
		if i == 0 || wt.bare {
			continue
		}
		// A linked worktree that is gone, that git reports as prunable, or whose
		// directory is no longer a working tree (a plain directory, a deleted .git
		// file) cannot be asked for its own refs, and one such directory must not
		// refuse every command until `git worktree prune`. Skipping it loses only
		// what lives in its per-worktree namespaces (HEAD, refs/bisect,
		// refs/worktree, refs/rewritten): every other ref lives in the common
		// directory and is listed through the main worktree above, and a detached
		// HEAD is read from `worktree list` above, none of which needs the
		// directory. A per-worktree ref cannot be moved by git without a working
		// tree. The loss that matters is a worktree readable before a command and
		// not after it, where the command could have moved such a ref and hidden
		// it; judgeMoves reports that as unscanned. A locked worktree is no
		// different from an unlocked one here and is read if it can be.
		if why := unreadableWorktree(wt); why != "" {
			snap.skipped[wt.path] = why
			continue
		}
		own, err := runGit(ctx, wt.path, "for-each-ref", "--format=%(objectname) %(refname)",
			"refs/worktree", "refs/bisect", "refs/rewritten")
		if err != nil {
			return nil, fmt.Errorf("listing the refs of worktree %s: %w", wt.path, err)
		}
		if err := addRefRecords(snap, wt.path, own); err != nil {
			return nil, err
		}
		snap.scanned[wt.path] = true
	}
	return snap, nil
}

// unreadableWorktree says why a linked worktree cannot be asked for its own
// refs, or "" when it can.
func unreadableWorktree(wt worktreeEntry) string {
	if wt.prunable {
		return "git reports it prunable"
	}
	if _, err := os.Stat(wt.path); err != nil {
		return "its directory is missing" // pruned or on an unmounted volume
	}
	if _, err := os.Lstat(filepath.Join(wt.path, ".git")); err != nil {
		return "its directory is not a working tree (no .git)"
	}
	return ""
}

// insideRepository reports whether git could find a repository from dir: a .git
// entry (directory, or the file of a linked worktree or submodule) in dir or an
// ancestor, or dir being a bare repository. It errs toward "yes": an entry it
// cannot examine counts, because the answer decides whether a failing git is
// ignored.
func insideRepository(dir string) bool {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return true
	}
	if isBareRepoShape(abs) {
		return true
	}
	for d := abs; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); !errors.Is(err, fs.ErrNotExist) {
			return true
		}
		if filepath.Dir(d) == d {
			return false
		}
	}
}

func isBareRepoShape(dir string) bool {
	for _, name := range []string{"HEAD", "objects", "refs"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			return false
		}
	}
	return true
}

func addRefRecords(snap *refSnapshot, worktree, out string) error {
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		oid, name, ok := strings.Cut(line, " ")
		if !ok {
			return fmt.Errorf("git answered an unreadable ref record: %q", line)
		}
		if worktree != "" {
			name = worktreeKey(worktree, name)
		}
		snap.refs[name] = oid
	}
	return nil
}

// worktreeKey names a ref that belongs to one worktree: its HEAD, or a ref in a
// per-worktree namespace. Ref names hold no colon, so the last one splits it.
func worktreeKey(path, ref string) string { return "worktree:" + path + ":" + ref }

// refLocation is where a snapshot key lives: the directory to run git in and the
// ref's name there.
func (r *Registry) refLocation(key string) (dir, name string) {
	rest, ok := strings.CutPrefix(key, "worktree:")
	if !ok {
		return r.deps.Root, key
	}
	i := strings.LastIndex(rest, ":")
	return rest[:i], rest[i+1:]
}

type worktreeEntry struct {
	path, head     string
	detached, bare bool
	prunable       bool
}

// parseWorktrees reads `git worktree list --porcelain`: blank-line separated
// records of "worktree <path>", "HEAD <oid>", "branch <ref>", "detached",
// "bare", "locked [reason]" and "prunable [reason]".
func parseWorktrees(out string) []worktreeEntry {
	var entries []worktreeEntry
	var cur *worktreeEntry
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			entries = append(entries, worktreeEntry{path: strings.TrimPrefix(line, "worktree ")})
			cur = &entries[len(entries)-1]
		case cur == nil:
		case strings.HasPrefix(line, "HEAD "):
			cur.head = strings.TrimPrefix(line, "HEAD ")
		case line == "detached":
			cur.detached = true
		case line == "bare":
			cur.bare = true
		case line == "prunable" || strings.HasPrefix(line, "prunable "):
			cur.prunable = true
		}
	}
	return entries
}

func (s *refSnapshot) oids() []string {
	seen := map[string]bool{}
	var out []string
	for _, oid := range s.refs {
		if !seen[oid] {
			seen[oid] = true
			out = append(out, oid)
		}
	}
	sort.Strings(out)
	return out
}

// guardedRef is one ref that moved, with what happened to it.
type guardedRef struct {
	Ref      string   `json:"ref"`
	Was      string   `json:"was,omitempty"`
	Now      string   `json:"now"`
	Commits  []string `json:"commits,omitempty"`
	Files    []string `json:"files,omitempty"`
	Restored bool     `json:"restored"`
	// RestoreTo is where a worktree's detached HEAD that did not exist before
	// the command is put back: the parent of the oldest new commit.
	RestoreTo string `json:"restore_to,omitempty"`
	Problem   string `json:"problem,omitempty"`
}

// refViolation is what the guard found, ready to become a tool result.
type refViolation struct {
	refs   []guardedRef
	manual bool
	notes  []string
	// between marks commits found on refs that moved between agent calls: they
	// are reported, never rolled back.
	between bool
}

// verifyRefs is the after half of the guard. It returns nil when no ref moved
// or every new commit is clean, and the refs as they stand once any rollback is
// done (nil when they could not be read), which is the next call's baseline.
func (r *Registry) verifyRefs(ctx context.Context, before *refSnapshot, command string) (*refViolation, *refSnapshot) {
	if before == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*gitTimeout)
	defer cancel()

	after, err := r.snapshotRefs(ctx)
	if err != nil || after == nil {
		return &refViolation{manual: true, notes: []string{
			fmt.Sprintf("the refs could not be read after the command (%v); whether it committed a secret is unknown", err),
			"recorded refs before the command: " + describeRefs(before.refs),
		}}, nil
	}
	v := r.judgeMoves(ctx, before, after, true)
	if v != nil {
		v.notes = append(v.notes, r.pushNotes(v.refs, command)...)
	}
	return v, after
}

// judgeMoves scans the commits that appeared on every ref that differs between
// two snapshots. With rollback it restores the leaking refs and updates after to
// match; without it the leak is only reported.
func (r *Registry) judgeMoves(ctx context.Context, before, after *refSnapshot, rollback bool) *refViolation {
	var moved []string
	for name, now := range after.refs {
		if before.refs[name] != now {
			moved = append(moved, name)
		}
	}
	lost := before.unreadableNow(after)
	if len(moved) == 0 && len(lost) == 0 {
		return nil
	}
	sort.Strings(moved)

	v := &refViolation{between: !rollback}
	set, err := r.collectSecretFiles(ctx)
	if err != nil {
		return &refViolation{manual: rollback, between: !rollback, notes: []string{
			fmt.Sprintf("the secret files could not be read to judge the new commits (%v); they were not scanned", err),
			"moved refs: " + describeMoved(before, after, moved),
		}}
	}
	base := before.oids()
	verdicts := map[string][]stagedLeak{}
	for _, name := range moved {
		now := after.refs[name]
		g := guardedRef{Ref: name, Was: before.refs[name], Now: now}
		commits, err := r.newCommits(ctx, now, base)
		switch {
		case err != nil:
			g.Problem = fmt.Sprintf("the commits it added could not be listed: %v", err)
			v.manual = v.manual || rollback
		case len(commits) > maxGuardCommits:
			g.Problem = fmt.Sprintf("it added more than %d commits, too many to scan; none were scanned", maxGuardCommits)
			v.manual = v.manual || rollback
		default:
			files := map[string]bool{}
			for _, c := range commits {
				leaks, seen := verdicts[c]
				if !seen {
					leaks, err = r.commitLeaks(ctx, set, c)
					if err != nil {
						g.Problem = fmt.Sprintf("commit %s could not be scanned: %v", c, err)
						v.manual = v.manual || rollback
						break
					}
					verdicts[c] = leaks
				}
				if len(leaks) > 0 {
					g.Commits = append(g.Commits, c)
					for _, l := range leaks {
						files[l.Path] = true
					}
				}
			}
			for f := range files {
				g.Files = append(g.Files, f)
			}
			sort.Strings(g.Files)
			if len(g.Commits) > 0 && g.Problem == "" && rollback {
				if _, refName := r.refLocation(name); g.Was == "" && refName == "HEAD" && len(commits) > 0 {
					// A worktree created by the command: its HEAD cannot be deleted,
					// so it goes back to the commit the new ones were built on.
					if parent, err := runGit(ctx, r.deps.Root, "rev-parse", "--verify", "--quiet", commits[len(commits)-1]+"^"); err == nil {
						g.RestoreTo = strings.TrimSpace(parent)
					}
				}
				r.restoreRef(ctx, &g, v)
				if g.Restored {
					if g.Was == "" && g.RestoreTo == "" {
						delete(after.refs, name)
					} else if g.Was == "" {
						after.refs[name] = g.RestoreTo
					} else {
						after.refs[name] = g.Was
					}
				}
			}
		}
		if len(g.Commits) > 0 || g.Problem != "" {
			v.refs = append(v.refs, g)
		}
	}
	for _, path := range lost {
		v.refs = append(v.refs, guardedRef{Ref: "worktree:" + path, Problem: fmt.Sprintf(
			"the linked worktree could be read before and cannot now (%s): its own refs were not scanned and may have moved",
			after.skipped[path])})
		v.manual = v.manual || rollback
	}
	if len(v.refs) == 0 {
		return nil
	}
	return v
}

// unreadableNow lists the linked worktrees whose own refs were read in s and
// that after still lists but cannot read, sorted.
func (s *refSnapshot) unreadableNow(after *refSnapshot) []string {
	var lost []string
	for path := range s.scanned {
		if _, skipped := after.skipped[path]; skipped {
			lost = append(lost, path)
		}
	}
	sort.Strings(lost)
	return lost
}

// newCommits lists the commits reachable from oid and from none of the
// recorded oids, at most maxGuardCommits+1 of them. A ref that now points at
// something that is not a commit (an annotated tag of a blob) adds none.
func (r *Registry) newCommits(ctx context.Context, oid string, recorded []string) ([]string, error) {
	if _, err := runGit(ctx, r.deps.Root, "rev-parse", "--verify", "--quiet", oid+"^{commit}"); err != nil {
		kind, kerr := runGit(ctx, r.deps.Root, "cat-file", "-t", oid)
		if kerr != nil {
			return nil, kerr
		}
		if strings.TrimSpace(kind) == "commit" {
			return nil, err
		}
		if peeled, perr := runGit(ctx, r.deps.Root, "rev-parse", "--verify", "--quiet", oid+"^{}"); perr == nil {
			if t, terr := runGit(ctx, r.deps.Root, "cat-file", "-t", strings.TrimSpace(peeled)); terr == nil && strings.TrimSpace(t) == "commit" {
				return nil, err
			}
		}
		return nil, nil
	}
	args := []string{"rev-list", fmt.Sprintf("--max-count=%d", maxGuardCommits+1), oid + "^{commit}"}
	if len(recorded) > 0 {
		args = append(args, "--not")
		args = append(args, recorded...)
	}
	out, err := runGit(ctx, r.deps.Root, args...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// commitLeaks judges one commit by what it adds over each parent, with the
// commit tool's scanner and its secret-path names. A merge is judged against
// every parent and reported only for what leaks against all of them, so a
// secret that one side of a merge already held is not blamed on the merge.
func (r *Registry) commitLeaks(ctx context.Context, set secretFileSet, commit string) ([]stagedLeak, error) {
	line, err := runGit(ctx, r.deps.Root, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return nil, err
	}
	parents := strings.Fields(line)
	if len(parents) > 0 {
		parents = parents[1:]
	}
	if len(parents) == 0 {
		empty, err := r.emptyTree(ctx)
		if err != nil {
			return nil, err
		}
		parents = []string{empty}
	}
	var common map[string]stagedLeak
	for _, parent := range parents {
		leaks, _, err := r.treeContentLeaksWith(ctx, set, parent, commit)
		if err != nil {
			return nil, err
		}
		names, err := runGit(ctx, r.deps.Root, "diff", "--name-only", "-z", "--no-renames", "--diff-filter=AMCRT", parent, commit)
		if err != nil {
			return nil, err
		}
		hit, err := secretPaths(r.matcher(), strings.FieldsFunc(names, func(c rune) bool { return c == 0 }))
		if err != nil {
			return nil, err
		}
		byPath := map[string]stagedLeak{}
		for _, l := range leaks {
			byPath[l.Path] = l
		}
		for _, p := range hit {
			l := byPath[p]
			l.Path = p
			l.Reason = append(l.Reason, "matches the secret-path patterns")
			byPath[p] = l
		}
		if common == nil {
			common = byPath
			continue
		}
		for p := range common {
			if _, ok := byPath[p]; !ok {
				delete(common, p)
			}
		}
	}
	out := make([]stagedLeak, 0, len(common))
	for _, l := range common {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// restoreRef puts a leaking ref back, compare-and-swap on the current value.
func (r *Registry) restoreRef(ctx context.Context, g *guardedRef, v *refViolation) {
	dir, name := r.refLocation(g.Ref)
	args := []string{"update-ref"}
	if name == "HEAD" {
		args = append(args, "--no-deref")
	}
	target := g.Was
	if target == "" {
		target = g.RestoreTo
	}
	switch {
	case target != "":
		args = append(args, name, target, g.Now)
	case name == "HEAD":
		g.Problem = "a worktree's HEAD that did not exist before the command cannot be put back: its first new commit has no parent"
		v.manual = true
		return
	default:
		args = append(args, "-d", name, g.Now)
	}
	if _, err := runGit(ctx, dir, args...); err != nil {
		g.Problem = fmt.Sprintf("it could not be restored (%v); it may have moved since it was read", err)
		v.manual = true
		return
	}
	g.Restored = true
}

func (r *Registry) pushNotes(refs []guardedRef, command string) []string {
	var notes []string
	remotes := map[string]bool{}
	for _, g := range refs {
		if rest, ok := strings.CutPrefix(g.Ref, "refs/remotes/"); ok && len(g.Commits) > 0 {
			remote, _, _ := strings.Cut(rest, "/")
			remotes[remote] = true
		}
	}
	for remote := range remotes {
		notes = append(notes, fmt.Sprintf("a remote-tracking ref of %q moved onto the leaking commit: it was probably pushed or fetched there, and rolling back the local ref does not recall it", remote))
	}
	if pushWord.MatchString(command) {
		notes = append(notes, "the command mentions push: if it pushed, the objects have left this repository and only the remote's owner can remove them")
	}
	sort.Strings(notes)
	return notes
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func describeRefs(refs map[string]string) string {
	names := make([]string, 0, len(refs))
	for n := range refs {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > 20 {
		return fmt.Sprintf("%d refs (first 20 shown) ", len(names)) + describeRefs(subset(refs, names[:20]))
	}
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, n+"="+refs[n])
	}
	return strings.Join(parts, " ")
}

func subset(refs map[string]string, names []string) map[string]string {
	out := make(map[string]string, len(names))
	for _, n := range names {
		out[n] = refs[n]
	}
	return out
}

func describeMoved(before, after *refSnapshot, moved []string) string {
	parts := make([]string, 0, len(moved))
	for _, n := range moved {
		parts = append(parts, fmt.Sprintf("%s: was %q now %q", n, before.refs[n], after.refs[n]))
	}
	return strings.Join(parts, "; ")
}

// leaked reports whether the guard found a credential: some ref's new commits
// carry one. A violation with none is the guard failing to look, not finding.
func (v *refViolation) leaked() bool {
	for _, g := range v.refs {
		if len(g.Commits) > 0 {
			return true
		}
	}
	return false
}

// withViolation makes the command's result the violation it caused. A finding
// is path.secret. A set the guard could not scan is ruleRefUnscanned: the same
// block (hard, is_error, no grant) with its own label and words, because "could
// not scan" is not "carries a credential".
func (v *refViolation) withViolation(res tools.Result) tools.Result {
	rule, operation, reason := string(policy.RuleSecretPath), "shell_commit", v.leakReason()
	if v.between {
		operation = "ref_changed_between_calls"
	}
	if !v.leaked() {
		rule, reason = ruleRefUnscanned, v.unscannedReason()
		operation = "shell_ref_unscanned"
		if v.between {
			operation = "ref_unscanned_between_calls"
		}
	}
	payload := map[string]any{
		"operation":            operation,
		"rule":                 rule,
		"severity":             string(policy.Hard),
		"reason":               reason,
		"refs":                 v.refs,
		"manual_intervention":  v.manual,
		"notes":                v.notes,
		"command_output_below": true,
	}
	data, _ := json.Marshal(payload)
	res.Text = string(data) + "\n" + res.Text
	res.IsError = true
	res.Blocked = true
	res.Rule = rule
	res.Severity = string(policy.Hard)
	return res
}

func (v *refViolation) leakReason() string {
	reason := "the command left commits that carry a credential reachable from a ref. A shell command is not a boundary the harness can read, so the refs are checked after it ran. "
	if v.between {
		reason = "a ref changed between agent calls, and the commits it gained carry a credential (see refs). " +
			"This was not done by the command that just ran, and nothing was rolled back: the writer may be the operator, whose refs the harness never rewrites, or a process that outlived an earlier call. "
	}
	switch {
	case v.between:
	case v.manual:
		reason += "MANUAL INTERVENTION NEEDED: the state below could not be settled automatically. "
	default:
		reason += "The refs named were restored to where they stood before the command; the files stay staged or in the working tree. "
	}
	return reason + "Use the devcouncil git commit tool, which refuses before committing."
}

func (v *refViolation) unscannedReason() string {
	names := make([]string, 0, len(v.refs))
	for _, g := range v.refs {
		names = append(names, g.Ref)
	}
	where := "the refs it could not read"
	if len(names) > 0 {
		where = "ref(s) " + strings.Join(names, ", ")
	}
	reason := "the ref guard could not scan what " + where + " made reachable, so it cannot say whether it holds a credential; none was found and nothing was rolled back. " +
		"Each ref's \"problem\" says why (a cap of " + fmt.Sprint(maxGuardCommits) + " commits, a timeout, or an unreadable repository). "
	if v.between {
		reason = "between agent calls, " + reason +
			"This was not done by the command that just ran: the writer may be the operator, whose refs the harness never rewrites. "
	}
	if v.manual {
		reason += "MANUAL INTERVENTION NEEDED: inspect the refs named before relying on them. "
	}
	return reason + "Until it is scanned this stays blocked (an unexamined commit is not a clean one); scan it with the devcouncil git tools or have the operator review it."
}

// guardTimeout bounds the snapshot taken before the command.
const guardTimeout = time.Minute

// refGuard is what the guard remembers between exec_command calls: the refs as
// the last call left them, so a ref that moved in between is compared with them
// and not simply absorbed into the next call's starting snapshot. Without it a
// process that outlived a call (a descendant that called setsid leaves the
// process group the call kills) could move a ref after the check and have the
// next call record the result as its baseline.
//
// Calls may overlap (sub-agents), and a call cannot tell its own ref changes
// from another's, so overlapping calls drop the baseline and the next call
// alone says so, as a note, rather than comparing against a baseline that two
// writers share.
type refGuard struct {
	mu       sync.Mutex
	active   map[*guardRun]bool
	baseline *refSnapshot
	note     string
}

// guardRun is one exec_command call's guard state.
type guardRun struct {
	before     *refSnapshot
	between    *refViolation
	notes      []string
	overlapped bool
}

// begin takes the starting snapshot and, when the call is alone and a baseline
// is held, scans what moved since that baseline (report only, no rollback).
func (r *Registry) guardBegin(ctx context.Context) (*guardRun, error) {
	snapCtx, cancel := context.WithTimeout(ctx, guardTimeout)
	defer cancel()
	g := &r.guard
	g.mu.Lock()
	defer g.mu.Unlock()
	snap, err := r.snapshotRefs(snapCtx)
	if err != nil {
		return nil, err
	}
	if snap == nil {
		return &guardRun{}, nil
	}
	run := &guardRun{before: snap}
	for _, path := range sortedKeys(snap.skipped) {
		run.notes = append(run.notes, fmt.Sprintf("linked worktree %s was not scanned for its own refs: %s", path, snap.skipped[path]))
	}
	if g.active == nil {
		g.active = map[*guardRun]bool{}
	}
	if len(g.active) > 0 {
		run.overlapped = true
		for other := range g.active {
			other.overlapped = true
		}
	} else {
		if g.note != "" {
			run.notes = append(run.notes, g.note)
			g.note = ""
		}
		if g.baseline != nil {
			run.between = r.judgeMoves(snapCtx, g.baseline, snap, false)
		}
	}
	g.active[run] = true
	return run, nil
}

// guardFinish is the after half: it judges and rolls back this call's own ref
// changes, then keeps the resulting refs as the baseline when the call was alone.
func (r *Registry) guardFinish(ctx context.Context, run *guardRun, command string) (*refViolation, *refViolation) {
	if run.before == nil {
		return nil, nil
	}
	v, final := r.verifyRefs(ctx, run.before, command)
	g := &r.guard
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.active, run)
	switch {
	case run.overlapped:
		g.baseline = nil
		g.note = "exec_command calls overlapped, so refs that changed between calls were not compared with a baseline"
	case len(g.active) > 0:
	case final == nil:
		g.baseline = nil
		g.note = "the refs could not be read after the previous call, so refs that changed between calls were not compared"
	default:
		g.baseline = final
	}
	return v, run.between
}
