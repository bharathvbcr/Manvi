//go:build unix

package serve

import (
	"os"
	"path/filepath"
	"testing"
)

// BREAK (fails on 0e2946a): a dangling symlink named .git, and a symlink named .git
// to the outer repository's own .git, both pass Lstat, so either splits a tracked
// subdirectory from its repository.
func TestZadvW17BreakPlantedGitSymlinkSplitsTrackedSubdirectory(t *testing.T) {
	for name, target := range map[string]func(root string) string{
		"dangling symlink":                       func(string) string { return "/nonexistent/nowhere" },
		"symlink to the outer repository's .git": func(root string) string { return filepath.Join(root, ".git") },
	} {
		t.Run(name, func(t *testing.T) {
			root, sub, deeper := trackedRepo(t)
			if err := os.Symlink(target(root), filepath.Join(sub, ".git")); err != nil {
				t.Fatal(err)
			}
			requireIndexStillTracks(t, root)
			if !overlap(t, root, sub) {
				t.Errorf("a %s planted in a tracked subdirectory made it a separate tree", name)
			}
			if !overlap(t, root, deeper) {
				t.Errorf("a %s planted in sub made sub/deeper a separate tree", name)
			}
		})
	}
}

// HOLDS: resolvedCheckout resolves links before the walk, so a symlink into the
// outer checkout is the subdirectory it names, and a symlink to a nested lane is
// the lane.
func TestZadvW17SymlinksAreResolvedBeforeTheBoundaryWalk(t *testing.T) {
	root, lane, _ := nestedRepo(t)
	pkg := filepath.Join(root, "pkg")
	if err := os.Mkdir(pkg, 0o750); err != nil {
		t.Fatal(err)
	}
	elsewhere := resolvedDir(t)

	toPkg := filepath.Join(elsewhere, "to-pkg")
	toLane := filepath.Join(elsewhere, "to-lane")
	toRoot := filepath.Join(elsewhere, "to-root")
	for link, target := range map[string]string{toPkg: pkg, toLane: lane, toRoot: root} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	if !overlap(t, root, toPkg) {
		t.Error("a symlink into the outer checkout does not overlap it")
	}
	if overlap(t, root, toLane) {
		t.Error("a symlink to a nested linked worktree overlaps the container")
	}
	if !overlap(t, toRoot, pkg) {
		t.Error("the outer checkout reached through a symlink does not overlap its subdirectory")
	}
	if overlap(t, toRoot, lane) {
		t.Error("the outer checkout reached through a symlink overlaps its nested linked worktree")
	}
	if !overlap(t, toRoot, toPkg) {
		t.Error("two symlinks, to a checkout and to its subdirectory, do not overlap")
	}

	// A link inside the outer checkout that leaves it names another directory.
	out := filepath.Join(pkg, "out")
	if err := os.Symlink(elsewhere, out); err != nil {
		t.Fatal(err)
	}
	if overlap(t, root, out) {
		t.Error("a link inside the checkout that leaves it overlaps the checkout")
	}
	// A link in the middle of the path: root/pkg/via -> lane, then via/sub is lane's.
	via := filepath.Join(pkg, "via")
	if err := os.Symlink(lane, via); err != nil {
		t.Fatal(err)
	}
	laneSub := filepath.Join(lane, "s")
	if err := os.Mkdir(laneSub, 0o750); err != nil {
		t.Fatal(err)
	}
	if overlap(t, root, filepath.Join(via, "s")) {
		t.Error("a directory reached through a link to a nested lane overlaps the container")
	}
	if !overlap(t, lane, filepath.Join(via, "s")) {
		t.Error("a directory reached through a link to a lane does not overlap that lane")
	}
}

// HOLDS: a directory on the path that cannot be searched makes the boundary
// unreadable, and an unreadable boundary is overlap, in both orders: the refusal's
// test fails closed. It holds for a real linked worktree whose .git cannot be read.
func TestZadvW17UnsearchableDirectoryOnTheWalkIsOverlap(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root searches everything")
	}
	root, lane, _ := nestedRepo(t)
	mid := filepath.Join(root, "mid")
	inner := filepath.Join(mid, "inner")
	if err := os.MkdirAll(inner, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, ".git"), []byte("gitdir: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rootPath, rootInfo := checkoutOf(t, root)
	innerPath, innerInfo := checkoutOf(t, inner)
	if checkoutsOverlap(rootInfo, rootPath, innerInfo, innerPath) {
		t.Fatal("precondition: a readable nested repository overlaps its container")
	}
	if err := os.Chmod(mid, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(mid, 0o750) }) //nolint:gosec // the search bit back so TempDir can remove it
	if !checkoutsOverlap(rootInfo, rootPath, innerInfo, innerPath) {
		t.Error("an unsearchable directory above a nested repository was read as no overlap")
	}
	if !checkoutsOverlap(innerInfo, innerPath, rootInfo, rootPath) {
		t.Error("the overlap is not symmetric when the walk cannot be read")
	}

	// The lane's own directory unsearchable: its .git cannot be statted.
	lanePath, laneInfo := checkoutOf(t, lane)
	if err := os.Chmod(lane, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lane, 0o750) }) //nolint:gosec // the search bit back so TempDir can remove it
	if !checkoutsOverlap(rootInfo, rootPath, laneInfo, lanePath) {
		t.Error("a linked worktree whose .git cannot be read was taken as a separate tree")
	}
}
