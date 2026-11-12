package devcouncil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Adversarial tests for cards ft-1ba663b7 (the snapshot covers every ref
// namespace git can resolve from the root) and ft-1eb121d4 (an admin dir git can
// resolve refs through is snapshotted or the command is refused). The implementer's
// tests are zrefguard_w21_*_test.go; these do not repeat them.

// leakRoot makes a second repository at root (initArgs go to `git init`, before
// the directory), with a .env on disk, one clean commit and notes.txt staged, and
// points the Registry at it. The fixture itself supplies the Registry.
func leakRoot(t *testing.T, root string, initArgs ...string) *fixture {
	t.Helper()
	f := shellCommitFixture(t, false)
	if err := os.MkdirAll(filepath.Dir(root), 0o750); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"init", "-q", "-b", "main"}, initArgs...)
	run(t, filepath.Dir(root), append(args, root)...)
	run(t, root, "config", "user.email", "h@test")
	run(t, root, "config", "user.name", "h")
	write(t, root, "seed.txt", "seed\n")
	run(t, root, "add", "seed.txt")
	run(t, root, "commit", "-qm", "seed")
	write(t, root, ".env", secretBody)
	write(t, root, "notes.txt", secretBody)
	run(t, root, "add", "notes.txt")
	f.reg.deps.Root = root
	return f
}

// requireStopped asserts that a later push of the name delivers nothing to the
// bare remote. A parking call that was not blocked passes only when git does not
// resolve the name at all (logged), so these tests stay valid when git is stricter
// than the guard.
func requireStopped(t *testing.T, f *fixture, res shellResult, by string) {
	t.Helper()
	remote := bareRemote(t)
	blocked := res.IsError && res.Blocked
	push := shell(f, "git push -q "+remote+" "+by+":refs/heads/leaked")
	if out := gitOut(t, remote, "for-each-ref"); out != "" {
		t.Fatalf("FAIL-OPEN: the leak reached the remote through %q (park call blocked=%v: %s; push: %+v) -> %s", by, blocked, res.Text, push, out)
	}
	if !blocked {
		t.Logf("the parking call was not blocked, and nothing arrived at the remote through %q", by)
	}
}

// reftable: git keeps pseudo-refs (ORIG_HEAD, any root name) in the reftable
// stack, where neither for-each-ref nor a file walk sees them.
func TestAdvW21ReftableRootPseudoRefLeak(t *testing.T) {
	for _, tc := range []pseudoCase{
		{"root_name", "git update-ref MYLEAK_HEAD $c", "MYLEAK_HEAD"},
		{"ORIG_HEAD", "git update-ref ORIG_HEAD $c", "ORIG_HEAD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := leakRoot(t, filepath.Join(t.TempDir(), "rt"), "--ref-format=reftable")
			write(t, f.reg.deps.Root, "park.sh", parkLeak+tc.park+"\n")
			res := shell(f, "sh park.sh")
			requireStopped(t, f, res, tc.by)
		})
	}
}

// A root whose .git is a file pointing at a separate git directory.
func TestAdvW21SeparateGitDirRootPseudoRefs(t *testing.T) {
	base := t.TempDir()
	f := leakRoot(t, filepath.Join(base, "work"), "--separate-git-dir", filepath.Join(base, "sep.git"))
	if fi, err := os.Stat(filepath.Join(base, "work", ".git")); err != nil || fi.IsDir() {
		t.Fatalf("bad test: .git is not a file: %v", err)
	}
	for _, tc := range []pseudoCase{
		{"ORIG_HEAD", "git update-ref ORIG_HEAD $c", "ORIG_HEAD"},
		{"lowercase_file", "echo $c > $d/leakfile", "leakfile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runPseudoCase(t, f, f.reg.deps.Root, tc)
		})
	}
}

// A name that is a directory symlink inside the git dir: git resolves
// `lnk/x` through it, the walk does not follow symlinks.
func TestAdvW21DirectorySymlinkInGitDirIsAPseudoRefSource(t *testing.T) {
	f := shellCommitFixture(t, true)
	write(t, f.root, "park.sh", parkLeak+"mkdir sidecar\necho $c > sidecar/x\nln -s \"$PWD/sidecar\" $d/lnk\n")
	res := shell(f, "sh park.sh")
	if got := gitOut(t, f.root, "rev-parse", "lnk/x"); len(got) < 40 {
		t.Fatalf("bad test: git does not resolve lnk/x: %q", got)
	}
	requireStopped(t, f, res, "lnk/x")
}

// Encodings git reads as refs: CRLF, upper-case hex, a non-ASCII name.
func TestAdvW21PseudoRefEncodings(t *testing.T) {
	for _, tc := range []pseudoCase{
		{"crlf", "printf '%s\\r\\n' $c > $d/CRLEAK", "CRLEAK"},
		{"uppercase_hex", "echo $c | tr a-f A-F > $d/UPLEAK", "UPLEAK"},
		{"unicode_name", "echo $c > $d/é", "é"},
		{"trailing_words", "printf '%s extra words\\n' $c > $d/WORDSLEAK", "WORDSLEAK"},
		{"multiline_second_is_leak_crlf", "printf '%s\\r\\n%s\\r\\n' $h $c > $d/MERGE_HEAD", "MERGE_HEAD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := shellCommitFixture(t, true)
			runPseudoCase(t, f, f.root, tc)
		})
	}
}

// What git does not resolve as a ref must not stop a command: leading white
// space, a 64-digit id in a sha1 repository, a 40-digit id that names no object.
func TestAdvW21NonRefsThatLookLikeIdsDoNotBlock(t *testing.T) {
	zero40 := strings.Repeat("ab", 20)
	for name, body := range map[string]string{
		"leading_space":     "printf ' %s\\n' $h > $d/WSFILE",
		"sha256_in_sha1":    "printf '%s\\n' " + strings.Repeat("cd", 32) + " > $d/CHECKSUM",
		"id_of_no_object":   "printf '%s\\n' " + zero40 + " > $d/NOOBJ",
		"sha256sum_of_file": "printf '%s  app.js\\n' " + strings.Repeat("ef", 32) + " > $d/SUMS",
	} {
		t.Run(name, func(t *testing.T) {
			f := shellCommitFixture(t, false)
			write(t, f.root, "mk.sh", "h=$(git rev-parse HEAD)\nd=$(git rev-parse --git-common-dir)\n"+body+"\n")
			res := shell(f, "sh mk.sh")
			// What git itself does with the file: if it resolves the name, the guard is
			// right to look; if it does not, a block is the guard being stricter than git.
			var names = map[string]string{"leading_space": "WSFILE", "sha256_in_sha1": "CHECKSUM", "id_of_no_object": "NOOBJ", "sha256sum_of_file": "SUMS"}
			resolved, rerr := exec.CommandContext(t.Context(), "git", "-C", f.root, "rev-parse", "--verify", "-q", names[name]+"^{commit}").Output() // #nosec G204 -- the binary is the literal "git".
			if res.Blocked || strings.Contains(res.Text, "MANUAL INTERVENTION") {
				t.Errorf("git resolves %s to a commit: %v (%q); the command was blocked: rule=%s", names[name], rerr == nil, strings.TrimSpace(string(resolved)), res.Rule)
			}
			if after := shell(f, "echo fine"); after.IsError || !strings.Contains(after.Text, "fine") {
				t.Fatalf("the next command is refused: %+v", after)
			}
		})
	}
}

// A symlink to a file outside the git dir: git resolves it, the guard reads it;
// rollback must not leave the leak resolvable.
func TestAdvW21SymlinkedPseudoRefIsRolledBackAndStaysALink(t *testing.T) {
	f := shellCommitFixture(t, true)
	head := headOf(t, f.root)
	write(t, f.root, "sidecar.txt", head+"\n")
	link := filepath.Join(gitCommon(t, f.root), "SIDELINK")
	if err := os.Symlink(filepath.Join(f.root, "sidecar.txt"), link); err != nil {
		t.Fatal(err)
	}
	write(t, f.root, "park.sh", parkLeak+"echo $c > sidecar.txt\n")
	res := shell(f, "sh park.sh")
	if !res.IsError || !res.Blocked {
		t.Fatalf("FAIL-OPEN: a leak parked through a symlinked pseudo-ref was allowed: %+v", res)
	}
	if got := gitOut(t, f.root, "rev-parse", "SIDELINK"); got != head {
		t.Errorf("SIDELINK resolves to %s after the call, want the clean %s", got, head)
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the user's symlink was replaced by a regular file: %v %v", fi, err)
	}
	if body, _ := os.ReadFile(filepath.Join(f.root, "sidecar.txt")); strings.TrimSpace(string(body)) != head { // #nosec G304 -- a file this test made.
		t.Errorf("the file behind the link still holds the leak: %q", body)
	}
}

// FETCH_HEAD's "not-for-merge" marker decides what `git merge FETCH_HEAD` takes;
// restoring one line as a bare id drops it.
func TestAdvW21RestoredFetchHeadKeepsItsNotForMergeMarker(t *testing.T) {
	f := shellCommitFixture(t, true)
	p := filepath.Join(gitCommon(t, f.root), "FETCH_HEAD")
	orig := headOf(t, f.root) + "\tnot-for-merge\tbranch 'x' of /somewhere\n"
	if err := os.WriteFile(p, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	write(t, f.root, "park.sh", parkLeak+"printf '%s\\t\\tbranch y of z\\n' $c > $d/FETCH_HEAD\n")
	res := shell(f, "sh park.sh")
	if !res.IsError || !res.Blocked {
		t.Fatalf("FAIL-OPEN: %+v", res)
	}
	got, err := os.ReadFile(p) // #nosec G304 -- a file this test made.
	if err != nil || string(got) != orig {
		t.Errorf("FETCH_HEAD was not put back as it was:\n got %q\nwant %q (%v)", got, orig, err)
	}
}

// restoreRootFile is compare-and-swap: a file rewritten after it was scanned is
// reported and left, whatever it now holds.
func TestAdvW21RestoreRootFileRefusesAFileRewrittenSinceTheScan(t *testing.T) {
	for name, now := range map[string]string{
		"other_oid":   strings.Repeat("c", 40) + "\n",
		"user_text":   "hello, this is not an id\n",
		"emptied":     "",
		"extra_lines": strings.Repeat("a", 40) + "\n" + strings.Repeat("c", 40) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "LEAK_HEAD")
			leak, clean := strings.Repeat("a", 40), strings.Repeat("b", 40)
			before := &refSnapshot{refs: map[string]string{fileKey(path, 0): clean}, files: map[string][]string{path: {clean}}}
			after := &refSnapshot{refs: map[string]string{fileKey(path, 0): leak}, files: map[string][]string{path: {leak}}}
			if err := os.WriteFile(path, []byte(now), 0o600); err != nil {
				t.Fatal(err)
			}
			g := guardedRef{Ref: fileKey(path, 0)}
			v := &refViolation{}
			restoreRootFile(before, after, g.Ref, &g, v, map[string]bool{})
			got, _ := os.ReadFile(path) // #nosec G304 -- a file this test made.
			if string(got) != now {
				t.Errorf("a file rewritten since the scan was modified: %q -> %q", now, got)
			}
			if g.Restored || !v.manual {
				t.Errorf("a refused restore must be reported: restored=%v manual=%v", g.Restored, v.manual)
			}
		})
	}
}

// A dangling admin dir whose only ref source is its HEAD: git resolves
// `worktrees/evil/HEAD`.
func TestAdvW21DanglingAdminDirWithOnlyAHeadIsARefSource(t *testing.T) {
	f := shellCommitFixture(t, true)
	script := "set -e\nc=$(git commit-tree $(git write-tree) -m leak)\n" +
		"a=" + filepath.Join(gitCommon(t, f.root), "worktrees", "evil") + "\n" +
		"mkdir -p $a\necho ../.. > $a/commondir\necho /nonexistent/evil/.git > $a/gitdir\necho $c > $a/HEAD\n"
	write(t, f.root, "mk.sh", script)
	res := shell(f, "sh mk.sh")
	if got := gitOut(t, f.root, "rev-parse", "worktrees/evil/HEAD"); len(got) < 40 {
		t.Fatalf("bad test: git does not resolve the HEAD: %q", got)
	}
	requireStopped(t, f, res, "worktrees/evil/HEAD")
}

// An admin dir whose commondir names a different common dir.
func TestAdvW21AdminDirWithAForeignCommondir(t *testing.T) {
	f := shellCommitFixture(t, true)
	other := t.TempDir()
	run(t, other, "init", "-q", "-b", "main")
	script := "set -e\nc=$(git commit-tree $(git write-tree) -m leak)\nh=$(git rev-parse HEAD)\n" +
		"a=" + filepath.Join(gitCommon(t, f.root), "worktrees", "evil") + "\n" +
		"mkdir -p $a/refs/worktree\necho " + filepath.Join(other, ".git") + " > $a/commondir\necho /nonexistent/evil/.git > $a/gitdir\n" +
		"echo $c > $a/refs/worktree/leak\necho $h > $a/HEAD\n"
	write(t, f.root, "mk.sh", script)
	res := shell(f, "sh mk.sh")
	requireStopped(t, f, res, "worktrees/evil/refs/worktree/leak")
}

// A packed-refs file in an admin dir.
func TestAdvW21AdminDirPackedRefs(t *testing.T) {
	f := shellCommitFixture(t, true)
	script := "set -e\nc=$(git commit-tree $(git write-tree) -m leak)\nh=$(git rev-parse HEAD)\n" +
		"a=" + filepath.Join(gitCommon(t, f.root), "worktrees", "evil") + "\n" +
		"mkdir -p $a\necho ../.. > $a/commondir\necho /nonexistent/evil/.git > $a/gitdir\necho $h > $a/HEAD\n" +
		"printf '# pack-refs with: peeled fully-peeled sorted\\n%s refs/worktree/x\\n' $c > $a/packed-refs\n"
	write(t, f.root, "mk.sh", script)
	res := shell(f, "sh mk.sh")
	requireStopped(t, f, res, "worktrees/evil/refs/worktree/x")
}

// An admin dir made by `git worktree add` in the command gets a refs/worktree
// entry through its admin dir.
func TestAdvW21WorktreeAddedThenItsRefsWorktreeLeaks(t *testing.T) {
	f := shellCommitFixture(t, true)
	write(t, f.root, "park.sh", parkLeak+"git worktree add -q --detach wts-b\ngit --git-dir=$d/worktrees/wts-b update-ref refs/worktree/x $c\n")
	res := shell(f, "sh park.sh")
	requireStopped(t, f, res, "worktrees/wts-b/refs/worktree/x")
}

// A file exactly at the size limit is read; one byte over refuses.
func TestAdvW21RefSourceFileSizeBoundary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		size    int
		refused bool
	}{
		{"exactly_at_limit", maxRootFileBytes, false},
		{"one_over", maxRootFileBytes + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := shellCommitFixture(t, false)
			head := headOf(t, f.root) + "\n"
			body := head + strings.Repeat("x", tc.size-len(head))
			if len(body) != tc.size {
				t.Fatalf("bad test: %d bytes", len(body))
			}
			if err := os.WriteFile(filepath.Join(gitCommon(t, f.root), "EDGE_HEAD"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			res := shell(f, "echo fine")
			if tc.refused != (res.IsError && strings.Contains(res.Text, "ref snapshot")) {
				t.Fatalf("size %d: refused=%v want %v: %+v", tc.size, !tc.refused, tc.refused, res)
			}
		})
	}
}

// A legitimate `git fetch` of a repository with many branches: FETCH_HEAD gets
// one line per branch, each one more entry to judge beside the refs/remotes/up/*
// the fetch creates. The command stays allowed, and the next one is not refused.
func TestAdvW21FetchOfManyBranchesStaysAllowed(t *testing.T) {
	const branches = 2500
	f := shellCommitFixture(t, false)
	up := upstreamWithCommits(t, 3)
	var in strings.Builder
	tip := gitOut(t, up, "rev-parse", "HEAD")
	for i := 0; i < branches; i++ {
		fmt.Fprintf(&in, "create refs/heads/bulk/b%05d %s\n", i, tip)
	}
	cmd := exec.CommandContext(t.Context(), "git", "-C", up, "update-ref", "--stdin") // #nosec G204 -- the binary is the literal "git".
	cmd.Stdin = strings.NewReader(in.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("creating refs: %v\n%s", err, out)
	}
	run(t, f.root, "remote", "add", "up", up)
	start := time.Now()
	res := shell(f, "git fetch -q up")
	took := time.Since(start)
	t.Logf("git fetch of %d branches through the guard took %v", branches, took)
	if res.Blocked || strings.Contains(res.Text, "MANUAL INTERVENTION") {
		t.Fatalf("a clean fetch of %d branches blocked the command after %v: %.600s", branches, took, res.Text)
	}
	if next := shell(f, "echo fine"); next.IsError || !strings.Contains(next.Text, "fine") {
		t.Fatalf("every later command is refused after the fetch: %+v", next)
	}
}

// Legitimate git work on clean commits stays allowed, from the main worktree and
// from a linked one, with no rollback and no manual intervention.
func TestAdvW21LegitimateGitOperationsStayAllowed(t *testing.T) {
	for _, linked := range []bool{false, true} {
		name := "main_root"
		if linked {
			name = "linked_root"
		}
		t.Run(name, func(t *testing.T) {
			f := shellCommitFixture(t, false)
			root, tip := f.root, "main"
			run(t, root, "commit", "-qm", "base")
			if linked {
				root = filepath.Join(filepath.Dir(f.root), "wt-adv-legit")
				run(t, f.root, "worktree", "add", "-q", "-b", "lane", root)
				f.reg.deps.Root = root
				tip = "lane"
			}
			write(t, root, "f.txt", "base\n")
			run(t, root, "add", "f.txt")
			run(t, root, "commit", "-qm", "f")
			run(t, root, "checkout", "-q", "-b", "feat")
			write(t, root, "f.txt", "feat\n")
			run(t, root, "commit", "-qam", "feat change")
			run(t, root, "checkout", "-q", tip)
			write(t, root, "f.txt", "tip\n")
			run(t, root, "commit", "-qam", "tip change")
			run(t, root, "checkout", "-q", "-b", "clean", tip+"~1")
			write(t, root, "g.txt", "g\n")
			run(t, root, "add", "g.txt")
			run(t, root, "commit", "-qm", "g")
			run(t, root, "checkout", "-q", tip)
			run(t, root, "remote", "add", "up", upstreamWithCommits(t, 5))
			env := "GIT_EDITOR=true EDITOR=true GIT_SEQUENCE_EDITOR=true "
			for _, cmd := range []string{
				"git merge feat || true",
				"git merge --abort",
				"git cherry-pick feat || true",
				"git cherry-pick --abort",
				"git rebase " + tip + " feat || true",
				"git rebase --abort",
				"git checkout -q " + tip,
				"git rebase " + tip + " clean",
				"git checkout -q " + tip,
				"git rebase -i HEAD~2",
				"git revert --no-edit HEAD || true",
				"git revert --abort || true",
				"git bisect start HEAD HEAD~2",
				"git bisect bad",
				"git bisect reset",
				"echo m >> f.txt && git stash && git stash drop",
				"git fetch -q up",
				"git fetch -q up main:refs/remotes/up/m2",
				"git fetch -q --tags up",
				"git worktree add -q --detach wts-l && git worktree remove --force wts-l",
				"git gc -q && git pack-refs --all --prune",
				"git commit --allow-empty -qm e && git reset -q --hard HEAD~1",
				"git checkout -q --detach HEAD && git checkout -q " + tip,
				"git reflog expire --expire=now --all && git gc -q --prune=now",
			} {
				res := shell(f, env+cmd)
				if res.Blocked || strings.Contains(res.Text, "MANUAL INTERVENTION") || strings.Contains(res.Text, "ref snapshot") || strings.HasPrefix(res.Rule, "ref.") {
					t.Errorf("%q was blocked: %+v", cmd, res)
				}
			}
		})
	}
}

// A shallow clone's .git/shallow lists boundary commits one per line, each a
// ref-shaped first line: a deepening fetch rewrites it.
func TestAdvW21ShallowFileDeepeningFetchIsAllowed(t *testing.T) {
	up := upstreamWithCommits(t, 12)
	base := t.TempDir()
	clone := filepath.Join(base, "clone")
	run(t, base, "clone", "-q", "--depth=2", "file://"+up, clone)
	run(t, clone, "config", "user.email", "h@test")
	run(t, clone, "config", "user.name", "h")
	f := shellCommitFixture(t, false)
	f.reg.deps.Root = clone
	for _, cmd := range []string{"git fetch -q --deepen=3", "git fetch -q --unshallow"} {
		res := shell(f, cmd)
		if res.Blocked || strings.Contains(res.Text, "MANUAL INTERVENTION") {
			t.Errorf("%q was blocked: %+v", cmd, res)
		}
	}
}

// The cost of the new walk: it reads the head of every non-skipped file below
// the git directory. Reports (does not assert a speed) the snapshot time with
// the hook samples git writes, and with a large info/ and rebase-merge/.
func TestAdvW21SnapshotCostWithLargeGitDirs(t *testing.T) {
	f := shellCommitFixture(t, false)
	common := gitCommon(t, f.root)
	measure := func() time.Duration {
		best := time.Hour
		for i := 0; i < 5; i++ {
			start := time.Now()
			if snap, err := f.reg.snapshotRefs(t.Context()); err != nil || snap == nil {
				t.Fatalf("snapshot: %v", err)
			}
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}
	plain := measure()
	for _, dir := range []string{"info/many", "rebase-merge"} {
		if err := os.MkdirAll(filepath.Join(common, dir), 0o750); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 20000; i++ {
			if err := os.WriteFile(filepath.Join(common, dir, fmt.Sprintf("f%05d", i)), []byte("some text\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	big := measure()
	t.Logf("snapshotRefs min of 5: plain %v, with 40000 extra files in info/ and rebase-merge/ %v", plain, big)
	if big > 60*time.Second {
		t.Errorf("snapshotRefs took %v", big)
	}
}
