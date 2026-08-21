package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"manvi/flags"
)

var (
	testProjectRootOverride string
	testRootMu              sync.RWMutex
)

// projectRoot returns the canonical repository or project root for this invocation.
//
// An operator who ran manvi from a deep subdirectory (e.g. pkg/foo/bar)
// has their repository root discovered automatically so that the state directory
// (.devcouncil), the git boundary, the code graph, and the policy gates anchor
// to the true repository rather than polluting the subdirectory.
func projectRoot() string {
	testRootMu.RLock()
	override := testProjectRootOverride
	testRootMu.RUnlock()
	if override != "" {
		return override
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return findProjectRoot(cwd)
}

// setProjectRootForTest overrides the cached project root during testing.
func setProjectRootForTest(root string) func() {
	testRootMu.Lock()
	testProjectRootOverride = root
	testRootMu.Unlock()
	return func() {
		testRootMu.Lock()
		testProjectRootOverride = ""
		testRootMu.Unlock()
	}
}

// findProjectRoot discovers the project root starting from startDir and walking upwards.
//
// Search precedence:
//  1. Environment override: MANVI_PROJECT_ROOT, MANVI_ROOT, or DEVCOUNCIL_ROOT.
//  2. Nearest ancestor containing a `.devcouncil` directory or `.git` (directory or file).
//  3. Nearest ancestor containing project markers (go.work, go.mod, Cargo.toml, package.json, pyproject.toml).
//  4. Fallback to startDir itself.
func findProjectRoot(startDir string) string {
	for _, envKey := range []string{"MANVI_PROJECT_ROOT", "MANVI_ROOT", "DEVCOUNCIL_ROOT"} {
		// Trimmed, because the value anchors the state directory, the git
		// boundary and the policy gates. An accidental "   " resolved to
		// "<cwd>/   " — a directory the harness would then create and treat as
		// the repository. Whitespace is never a path anyone meant.
		//
		// A non-existent path is still honoured: naming a root is a deliberate
		// act, and the harness creating it there is what an operator scripting
		// a fresh checkout expects. Only the meaningless case is refused.
		if val := strings.TrimSpace(os.Getenv(envKey)); val != "" {
			if abs, err := filepath.Abs(val); err == nil {
				if resolved, err := filepath.EvalSymlinks(abs); err == nil {
					return filepath.Clean(resolved)
				}
				return filepath.Clean(abs)
			}
		}
	}

	absStart, err := filepath.Abs(startDir)
	if err != nil {
		absStart = filepath.Clean(startDir)
	}
	if resolved, err := filepath.EvalSymlinks(absStart); err == nil {
		absStart = resolved
	}

	// 1. Primary pass: look for .devcouncil or .git (including worktrees and submodules where .git is a file).
	cur := absStart
	for {
		if isSharedRoot(cur) {
			break
		}
		devcouncilPath := filepath.Join(cur, ".devcouncil")
		if info, err := os.Stat(devcouncilPath); err == nil && info.IsDir() {
			return cur
		}
		gitPath := filepath.Join(cur, ".git")
		if _, err := os.Stat(gitPath); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}

	// 2. Secondary pass: look for workspace / build manifests.
	markers := []string{"go.work", "go.mod", "Cargo.toml", "package.json", "pyproject.toml"}
	cur = absStart
	for {
		if isSharedRoot(cur) {
			break
		}
		for _, marker := range markers {
			markerPath := filepath.Join(cur, marker)
			if info, err := os.Stat(markerPath); err == nil && !info.IsDir() {
				return cur
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}

	return absStart
}

// isSharedRoot reports whether dir is a directory that holds unrelated work
// side by side by construction, and so is never the root of the project being
// edited.
//
// Both passes above walk to the filesystem root, and the first marker they meet
// wins. Inside a repository that ordering is what we want: a repository's .git
// sits above a nested package's go.mod, and the repository is the root the
// state directory, the code graph and the policy gates should anchor to.
//
// It stops being what we want the moment the walk leaves the project. A
// .devcouncil left in /private/tmp by an earlier run made a module under
// /private/tmp/…/sandbox resolve its root to /private/tmp — and the root is
// what bounds the outside-root hard rule, so under --yolo, where that rule is
// off and nothing else contains the agent, the reachable surface became every
// sibling directory in /tmp. A dotfiles repository in the home directory does
// the same thing to every project below it that has no marker of its own.
//
// The walk therefore stops at these directories rather than adopting them, and
// nothing above one is considered either: a marker there is debris left by
// unrelated work, and inheriting it widens containment instead of setting it.
//
// This is a bound on inheritance, not a prefix ban. A project that keeps its
// own marker below one of these directories still resolves to itself, and
// running the harness directly in one of them still works — the fallback
// returns the directory the caller was actually standing in. What can no
// longer happen is a deep directory silently claiming a shared ancestor.
func isSharedRoot(dir string) bool {
	clean := filepath.Clean(dir)
	if parent := filepath.Dir(clean); parent == clean {
		return true // the filesystem root, and any volume root
	}
	for _, shared := range sharedRoots() {
		if clean == shared {
			return true
		}
	}
	return false
}

// sharedRoots lists the directories isSharedRoot refuses, resolved the same way
// findProjectRoot resolves its start directory so the comparison is like for
// like — on macOS /tmp is a symlink to /private/tmp and the walk arrives with
// the resolved spelling.
//
// Read from the environment on each call rather than cached: HOME and TMPDIR
// are what a test moves to exercise this, and a cache would answer from the
// process's first caller instead of the current one.
func sharedRoots() []string {
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		out = append(out, filepath.Clean(abs))
	}
	add(os.Getenv("HOME"))
	if home, err := os.UserHomeDir(); err == nil {
		add(home)
	}
	add(os.Getenv("TMPDIR"))
	for _, fixed := range []string{"/tmp", "/private/tmp", "/var/tmp", "/private/var/tmp"} {
		add(fixed)
	}
	return out
}

// stateDir returns the canonical directory where the harness stores persistent state.
//
// If MANVI_STATE_DIR is set to an absolute path, it is used directly.
// Otherwise, it is resolved relative to the discovered project root (defaulting to <projectRoot>/.devcouncil).
func stateDir() string {
	raw := env("MANVI_STATE_DIR", ".devcouncil")
	if filepath.IsAbs(raw) {
		return filepath.Clean(raw)
	}
	return filepath.Join(projectRoot(), raw)
}

// configPath is the committable settings file, inside the state directory.
func configPath() string {
	return filepath.Join(stateDir(), flags.DefaultConfigFile)
}

// storeDBPath is the lease and task store database.
func storeDBPath() string {
	if custom := os.Getenv("MANVI_STORE_DB"); custom != "" {
		if filepath.IsAbs(custom) {
			return filepath.Clean(custom)
		}
		return filepath.Join(projectRoot(), custom)
	}
	return filepath.Join(stateDir(), "state.sqlite")
}

// graphArtifactPath is where the code graph artifact is written.
func graphArtifactPath() string {
	if custom := os.Getenv("MANVI_GRAPH"); custom != "" {
		if filepath.IsAbs(custom) {
			return filepath.Clean(custom)
		}
		return filepath.Join(projectRoot(), custom)
	}
	return filepath.Join(stateDir(), "code_graph.json")
}

// repoMapArtifactPath is where the repo map artifact is written.
func repoMapArtifactPath() string {
	return filepath.Join(stateDir(), "repo_map.json")
}

// grantLedgerPath is where human and agent overrides persist.
func grantLedgerPath() string {
	return filepath.Join(stateDir(), "harness-grants.json")
}

// indexDir is the derived code-intelligence index devmap owns.
func indexDir() string {
	return filepath.Join(stateDir(), "codeintel")
}

// sessionsDir is where headless and TUI sessions persist.
func sessionsDir() string {
	return filepath.Join(stateDir(), "sessions")
}

// artifactsDir is where DevCouncil artifacts persist.
func artifactsDir() string {
	return filepath.Join(stateDir(), "artifacts")
}

// resolveCLIPath resolves a file or path argument passed on the CLI.
//
// When an operator is standing in a subdirectory of the project (e.g. `src/`)
// and types `manvi check helper.go` or `manvi allow helper.go`:
//   - If target starts with `..`, it is resolved relative to cwd.
//   - If `src/helper.go` exists relative to cwd, it resolves to `src/helper.go` (relative to root).
//   - If `root/helper.go` exists (repo-relative notation), it resolves to `root/helper.go`.
//   - If the file does not exist yet (creating a new file in cwd), it resolves relative to cwd.
//   - Absolute paths are preserved for root containment checking.
func resolveCLIPath(root, cwd, target string) string {
	if target == "" || filepath.IsAbs(target) {
		return target
	}

	cleanedTarget := filepath.Clean(target)

	// If target explicitly starts with "../" or is "..", it is explicitly relative to cwd.
	if cleanedTarget == ".." || strings.HasPrefix(cleanedTarget, ".."+string(filepath.Separator)) {
		return filepath.Join(cwd, target)
	}

	fromCwd := filepath.Join(cwd, target)
	fromRoot := filepath.Join(root, target)

	// If it exists in cwd, the operator meant the file in front of them.
	if _, err := os.Stat(fromCwd); err == nil {
		return fromCwd
	}
	// If it exists in root, the operator gave a repo-relative path.
	if _, err := os.Stat(fromRoot); err == nil {
		return fromRoot
	}

	// For a planned new file that does not exist yet:
	// If target starts with a directory segment that already exists at the root,
	// assume the operator typed a repo-relative path (e.g. `pkg/foo/new.go`).
	segments := strings.Split(filepath.ToSlash(cleanedTarget), "/")
	if len(segments) > 1 {
		firstDir := filepath.Join(root, segments[0])
		if info, err := os.Stat(firstDir); err == nil && info.IsDir() {
			return fromRoot
		}
	}

	// Otherwise, if cwd is inside root, anchor to cwd.
	relToRoot, err := filepath.Rel(root, fromCwd)
	if err == nil && !strings.HasPrefix(relToRoot, "..") && relToRoot != "." {
		return fromCwd
	}

	return fromRoot
}
