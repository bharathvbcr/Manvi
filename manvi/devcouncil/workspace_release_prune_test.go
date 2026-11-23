package devcouncil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ReleaseWorktree gives back the one worktree it provisioned. It used to run
// `git worktree prune` afterwards, which is repository-wide: it forgot every
// registration whose directory was missing, including a worktree the operator
// had registered and whose directory was on an unmounted volume or being moved.
// Only the lease's own registration is the release's to remove.
func TestReleaseWorktreeLeavesTheOperatorsRegistrationsAlone(t *testing.T) {
	root := initCommittedGitRepo(t, map[string]string{"a.txt": "x\n"})
	operator := filepath.Join(t.TempDir(), "operator-wt")
	// #nosec G204 -- the binary is the literal "git" and the arguments are fixed.
	if out, err := exec.CommandContext(t.Context(), "git", "-C", root, "worktree", "add", "--detach", operator).CombinedOutput(); err != nil {
		t.Fatalf("registering the operator's worktree: %v\n%s", err, out)
	}
	if err := os.RemoveAll(operator); err != nil {
		t.Fatal(err)
	}

	coord := &GitWorktreeCoordinator{Root: root, BaseDir: t.TempDir()}
	lease, err := coord.ProvisionWorktree(t.Context(), "child")
	if err != nil {
		t.Fatal(err)
	}
	retained, err := coord.ReleaseWorktree(t.Context(), lease, false)
	if err != nil || retained {
		t.Fatalf("release: retained=%v err=%v", retained, err)
	}

	// #nosec G204 -- the binary is the literal "git" and the arguments are fixed.
	out, err := exec.CommandContext(t.Context(), "git", "-C", root, "worktree", "list", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatalf("listing worktrees: %v\n%s", err, out)
	}
	listing := string(out)
	if !strings.Contains(listing, filepath.Base(operator)) {
		t.Fatalf("the operator's registration was pruned by a child's release:\n%s", listing)
	}
	if strings.Contains(listing, filepath.Base(lease.ContainerDir)) {
		t.Fatalf("the child's own registration survived its release:\n%s", listing)
	}
}

// The release is also correct when the child's directory is already gone: its
// own registration is removed, not left for a later prune.
func TestReleaseWorktreeUnregistersALeaseWhoseDirectoryIsGone(t *testing.T) {
	root := initCommittedGitRepo(t, map[string]string{"a.txt": "x\n"})
	coord := &GitWorktreeCoordinator{Root: root, BaseDir: t.TempDir()}
	lease, err := coord.ProvisionWorktree(t.Context(), "child")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(lease.Path); err != nil {
		t.Fatal(err)
	}
	_, _ = coord.ReleaseWorktree(t.Context(), lease, false)

	// #nosec G204 -- the binary is the literal "git" and the arguments are fixed.
	out, err := exec.CommandContext(t.Context(), "git", "-C", root, "worktree", "list", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatalf("listing worktrees: %v\n%s", err, out)
	}
	if strings.Contains(string(out), filepath.Base(lease.ContainerDir)) {
		t.Fatalf("a released lease whose directory was gone is still registered:\n%s", out)
	}
}
