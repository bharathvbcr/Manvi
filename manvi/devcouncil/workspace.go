package devcouncil

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/dcgrep"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/gate"
	"github.com/bharathvbcr/Manvi/manvi/childenv"
)

const (
	// WorkspaceInherit runs the sub-agent in the parent's working tree (default).
	WorkspaceInherit = "inherit"
	// WorkspaceWorktree provisions an isolated git worktree with a stacked DevMap
	// seed so parallel mutating sub-agents never race on the parent checkout.
	WorkspaceWorktree = "worktree"

	worktreeProvisionTimeout = 60 * time.Second
	devmapSeedTimeout        = 30 * time.Second
)

var worktreeSeq atomic.Uint64

// WorkspaceLease describes an isolated git worktree provisioned for one child.
type WorkspaceLease struct {
	// Path is the canonical filesystem root of the provisioned worktree.
	Path string
	// ContainerDir is the private 0700 parent directory allocated for Path, if
	// created by GitWorktreeCoordinator.
	ContainerDir string
	// HeadSHA is the commit Path was detached at when provisioned.
	HeadSHA string
	// DevmapSeeded reports whether `devmap seed --mode stacked` succeeded.
	DevmapSeeded bool
	// DevmapNote records why DevMap seeding was skipped or failed when
	// DevmapSeeded is false.
	DevmapNote string
}

// WorkspaceCoordinator provisions and reclaims isolated git worktrees for
// sub-agents dispatched with `workspace: "worktree"`.
type WorkspaceCoordinator interface {
	ProvisionWorktree(ctx context.Context, label string) (*WorkspaceLease, error)
	ReleaseWorktree(ctx context.Context, lease *WorkspaceLease, retainDirty bool) (retained bool, err error)
}

// GitWorktreeCoordinator provisions detached git worktrees from Root in
// owner-only temporary directories and seeds each worktree's DevMap overlay via
// `devmap seed --mode stacked`.
type GitWorktreeCoordinator struct {
	Root         string
	BaseDir      string
	DevmapBinary string

	// gitMu serializes `git worktree add` and `git worktree remove`/`prune`
	// against Root's shared `.git/worktrees` admin directory so an 8-way
	// concurrent fan-out never hits `.git/config.lock` contention or prunes a
	// sibling's half-initialized admin entry. Sub-agent turns and stacked DevMap
	// seeding run outside gitMu.
	gitMu sync.Mutex
}

// ProvisionWorktree creates a detached git worktree at Root's HEAD and seeds a
// zero-copy stacked DevMap lease when Root has an index and DevmapBinary is
// reachable.
func (c *GitWorktreeCoordinator) ProvisionWorktree(ctx context.Context, label string) (*WorkspaceLease, error) {
	if c == nil || strings.TrimSpace(c.Root) == "" {
		return nil, fmt.Errorf("workspace coordinator has no repository root configured")
	}
	provCtx, cancel := context.WithTimeout(ctx, worktreeProvisionTimeout)
	defer cancel()

	slug := sanitizeWorktreeSlug(label)
	seq := worktreeSeq.Add(1)
	container, err := os.MkdirTemp(c.BaseDir, fmt.Sprintf("manvi-wt-%s-%d-", slug, seq))
	if err != nil {
		return nil, fmt.Errorf("allocating isolated worktree container: %w", err)
	}
	if err := os.Chmod(container, 0o700); err != nil {
		_ = os.RemoveAll(container)
		return nil, fmt.Errorf("securing isolated worktree container: %w", err)
	}
	wtRaw := filepath.Join(container, "checkout")

	c.gitMu.Lock()
	headOut, headErr := runGit(provCtx, c.Root, "rev-parse", "--verify", "HEAD")
	var headSHA string
	var addErr error
	if headErr == nil {
		headSHA = strings.TrimSpace(headOut)
		_, addErr = runGit(provCtx, c.Root, "worktree", "add", "--detach", wtRaw, headSHA)
	}
	c.gitMu.Unlock()

	if headErr != nil {
		_ = os.RemoveAll(container)
		return nil, fmt.Errorf("cannot provision isolated worktree from %s (repository must have a committed HEAD): %w", c.Root, headErr)
	}
	if addErr != nil {
		_ = os.RemoveAll(container)
		return nil, fmt.Errorf("git worktree add failed for %q: %w", label, addErr)
	}
	wtCanon := wtRaw
	if resolved, evalErr := filepath.EvalSymlinks(wtRaw); evalErr == nil {
		wtCanon = resolved
	}

	seeded, note := c.seedDevmapStacked(provCtx, wtCanon)
	return &WorkspaceLease{
		Path:         wtCanon,
		ContainerDir: container,
		HeadSHA:      headSHA,
		DevmapSeeded: seeded,
		DevmapNote:   note,
	}, nil
}

// ReleaseWorktree inspects the worktree for uncommitted or untracked changes or
// new commits relative to Lease.HeadSHA. When retainDirty is true and work
// exists in the worktree, the checkout is preserved so the parent or operator
// can inspect and integrate it; otherwise the worktree and its container
// directory are removed and pruned.
func (c *GitWorktreeCoordinator) ReleaseWorktree(ctx context.Context, lease *WorkspaceLease, retainDirty bool) (bool, error) {
	if c == nil || lease == nil || strings.TrimSpace(lease.Path) == "" {
		return false, nil
	}
	relCtx, cancel := context.WithTimeout(ctx, worktreeProvisionTimeout)
	defer cancel()

	if retainDirty {
		dirty, checkErr := c.worktreeHasChanges(relCtx, lease)
		if checkErr != nil {
			// Fail closed toward preservation: if we cannot prove the worktree is
			// clean, never delete bytes a sub-agent may have written.
			return true, fmt.Errorf("preserving worktree %s because cleanliness check failed: %w", lease.Path, checkErr)
		}
		if dirty {
			return true, nil
		}
	}

	c.gitMu.Lock()
	_, removeErr := runGit(relCtx, c.Root, "worktree", "remove", "--force", lease.Path)
	_, _ = runGit(relCtx, c.Root, "worktree", "prune")
	c.gitMu.Unlock()

	if lease.ContainerDir != "" {
		_ = os.RemoveAll(lease.ContainerDir)
	}
	if removeErr != nil {
		return false, fmt.Errorf("removing clean worktree %s: %w", lease.Path, removeErr)
	}
	return false, nil
}

func (c *GitWorktreeCoordinator) worktreeHasChanges(ctx context.Context, lease *WorkspaceLease) (bool, error) {
	porcelain, err := runGit(ctx, lease.Path, "status", "--porcelain=v1", "-z", "-uall")
	if err != nil {
		return true, err
	}
	if porcelainHasUserChanges(porcelain) {
		return true, nil
	}
	if lease.HeadSHA != "" {
		curHead, headErr := runGit(ctx, lease.Path, "rev-parse", "--verify", "HEAD")
		if headErr != nil {
			return true, headErr
		}
		if strings.TrimSpace(curHead) != lease.HeadSHA {
			return true, nil
		}
	}
	return false, nil
}

func porcelainHasUserChanges(porcelain string) bool {
	entries := strings.Split(porcelain, "\x00")
	for i := 0; i < len(entries); i++ {
		rec := entries[i]
		if rec == "" {
			continue
		}
		if len(rec) < 4 || rec[2] != ' ' {
			return true
		}
		xy := rec[:2]
		path := rec[3:]
		if xy[0] == 'R' || xy[0] == 'C' || xy[1] == 'R' || xy[1] == 'C' {
			if i+1 < len(entries) {
				i++
			}
			return true
		}
		if xy == "??" && isHarnessStateRelPath(path) {
			continue
		}
		return true
	}
	return false
}

func isHarnessStateRelPath(rel string) bool {
	clean := strings.TrimPrefix(filepath.ToSlash(rel), "./")
	return clean == ".devcouncil" || strings.HasPrefix(clean, ".devcouncil/") ||
		clean == ".devmap" || strings.HasPrefix(clean, ".devmap/")
}

func (c *GitWorktreeCoordinator) seedDevmapStacked(ctx context.Context, wtPath string) (bool, string) {
	if !hasParentDevmapIndex(c.Root) {
		return false, "parent checkout has no DevMap index to seed from"
	}
	bin := strings.TrimSpace(c.DevmapBinary)
	if bin == "" {
		return false, "no devmap binary configured"
	}
	resolvedBin, err := exec.LookPath(bin)
	if err != nil {
		return false, fmt.Sprintf("devmap binary %q not found: %v", bin, err)
	}

	res := runProc(ctx, procSpec{
		argv:        []string{resolvedBin, "seed", "--mode", "stacked", "--from", c.Root, "--json", wtPath},
		dir:         wtPath,
		env:         childenv.Inherit(),
		timeout:     devmapSeedTimeout,
		stdoutLimit: 64 << 10,
		stderrLimit: 64 << 10,
	})
	if res.timedOut {
		return false, fmt.Sprintf("devmap seed timed out after %s", devmapSeedTimeout)
	}
	if res.err != nil {
		detail := strings.TrimSpace(string(res.stderr))
		if detail == "" {
			detail = strings.TrimSpace(string(res.stdout))
		}
		if detail == "" {
			detail = res.err.Error()
		}
		return false, fmt.Sprintf("devmap seed failed: %s", detail)
	}
	return true, ""
}

func hasParentDevmapIndex(root string) bool {
	for _, state := range []string{".devcouncil", ".devmap"} {
		ci := filepath.Join(root, state, "codeintel")
		if info, err := os.Stat(filepath.Join(ci, "devmap.sqlite")); err == nil && !info.IsDir() {
			return true
		}
		if info, err := os.Stat(filepath.Join(ci, "stack_lease.json")); err == nil && !info.IsDir() {
			return true
		}
		if info, err := os.Stat(filepath.Join(ci, "base_store_ref.json")); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

func sanitizeWorktreeSlug(raw string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			if b.Len() >= 24 {
				break
			}
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "subagent"
	}
	return slug
}

func validateWorkspaceMode(coordinator WorkspaceCoordinator, raw string) (string, error) {
	mode := strings.TrimSpace(raw)
	if mode == "" {
		return "", nil
	}
	if coordinator == nil {
		return "", fmt.Errorf("%s", unimplementedWorkspace)
	}
	switch mode {
	case WorkspaceInherit:
		return WorkspaceInherit, nil
	case WorkspaceWorktree:
		return WorkspaceWorktree, nil
	default:
		return "", fmt.Errorf(
			"workspace %q is not supported: valid modes are %q (parent tree) or %q (isolated git worktree). Nothing was dispatched",
			mode, WorkspaceInherit, WorkspaceWorktree)
	}
}

// ForWorktree builds a child Registry whose filesystem, git, policy gate,
// navigator, and repository search are hermetically rooted at worktreeRoot,
// while sharing the parent's task/lease Store, role catalogue, instance
// manager, and secret scrubbers.
func (r *Registry) ForWorktree(worktreeRoot string, wtGate *gate.Gate, wtMap *devmap.Client, wtGrep *dcgrep.Client) (*Registry, error) {
	if r == nil {
		return nil, fmt.Errorf("devcouncil: nil parent registry")
	}
	if strings.TrimSpace(worktreeRoot) == "" {
		return nil, fmt.Errorf("devcouncil: empty worktree root")
	}
	if wtGate == nil {
		return nil, fmt.Errorf("devcouncil: nil worktree gate")
	}
	deps := r.deps
	deps.Root = worktreeRoot
	deps.Gate = wtGate
	deps.Map = wtMap
	deps.Grep = wtGrep
	// Child registries never dispatch nested sub-agents (enforced structurally by
	// floor), so clear Workspaces and SubAgent on the child surface too.
	deps.SubAgent = nil
	deps.Workspaces = nil
	return New(deps)
}
