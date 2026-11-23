package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/mcp"
)

// untrustedToolRoots returns the working directory and any enclosing or
// configured workspace roots that an agent or checked-out repository may write.
func untrustedToolRoots(getenv func(string) string) ([]string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolve working directory: %w", err)
	}
	var candidates []string
	addRoot := func(dir string) {
		if dir == "" {
			return
		}
		clean := filepath.Clean(dir)
		// A filesystem root (e.g. "/") would distrust every installed system tool.
		if clean == filepath.Dir(clean) {
			return
		}
		for _, existing := range candidates {
			if existing == clean {
				return
			}
		}
		candidates = append(candidates, clean)
	}

	addRoot(cwd)
	if absCwd, absErr := filepath.Abs(cwd); absErr == nil {
		addRoot(absCwd)
		cur := absCwd
		for {
			if hasWorkspaceMarker(cur) {
				addRoot(cur)
				break
			}
			parent := filepath.Dir(cur)
			if parent == cur {
				break
			}
			cur = parent
		}
	}

	for _, envVal := range []string{
		getenv("MANVI_PROJECT_ROOT"),
		getenv("MANVI_ROOT"),
		getenv("DEVCOUNCIL_ROOT"),
	} {
		trimmed := strings.TrimSpace(envVal)
		if trimmed == "" {
			continue
		}
		if abs, absErr := filepath.Abs(trimmed); absErr == nil {
			addRoot(abs)
		} else {
			addRoot(trimmed)
		}
	}
	return candidates, nil
}

func hasWorkspaceMarker(dir string) bool {
	for _, marker := range []string{".devcouncil", ".git"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return true
		}
	}
	return false
}

// trustedResolvedBinary resolves symlinks on candidate and confirms it is an
// executable regular file outside every untrusted root.
func trustedResolvedBinary(candidate string, untrustedRoots []string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil || !filepath.IsAbs(resolved) {
		return "", false
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() {
		return "", false
	}
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		return "", false
	}
	for _, root := range untrustedRoots {
		inside, err := mcp.PathIsInside(root, resolved)
		if err != nil || inside {
			return "", false
		}
	}
	return resolved, true
}

// resolveTrustedPathBinary locates a bare executable name across absolute PATH
// directories while rejecting empty, relative, working-directory, and
// workspace-owned binaries (including symlinks pointing into them).
func resolveTrustedPathBinary(name string, getenv func(string) string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	roots, err := untrustedToolRoots(getenv)
	if err != nil {
		return "", err
	}
	for _, dir := range filepath.SplitList(getenv("PATH")) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		p, lookErr := exec.LookPath(filepath.Join(dir, name))
		if lookErr != nil {
			continue
		}
		if resolved, ok := trustedResolvedBinary(p, roots); ok {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("binary %q not found in trusted PATH", name)
}
