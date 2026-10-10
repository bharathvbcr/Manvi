package serve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrBadRoot means a repository root the request named is not usable: not
// absolute, missing, a symlink, or not the top of a git work tree. It is never
// retryable, and the server never substitutes its own working directory for
// it: a sidecar's cwd is wherever its host launched it, so an answer about
// that directory is an answer about the wrong tree.
const ErrBadRoot = "E_BAD_ROOT"

func badRoot(format string, args ...any) *Error {
	return &Error{Code: ErrBadRoot, Message: fmt.Sprintf(format, args...)}
}

// canonicalRepoRoot validates an explicit repository root: absolute, existing,
// free of symlinks, and the top of a git work tree. A symlinked root is
// refused rather than followed so the path a host names is the path read.
// field names the request parameter in the refusal.
func canonicalRepoRoot(ctx context.Context, field, raw string) (string, *Error) {
	if raw == "" || strings.ContainsRune(raw, 0) || !filepath.IsAbs(raw) {
		return "", badRoot("%s must be an absolute path", field)
	}
	clean := filepath.Clean(raw)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", badRoot("%s does not exist or cannot be resolved: %v", field, err)
	}
	if resolved != clean {
		return "", badRoot("%s is or passes through a symlink; pass the resolved path %s", field, resolved)
	}
	if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
		return "", badRoot("%s is not a directory", field)
	}
	top, err := scanGit(ctx, resolved, "rev-parse", "--show-toplevel")
	if err != nil {
		if ctx.Err() != nil {
			return "", dependencyError("git", ctx.Err())
		}
		return "", badRoot("%s is not a git work tree", field)
	}
	topResolved, err := filepath.EvalSymlinks(strings.TrimSpace(string(top)))
	if err != nil || topResolved != resolved {
		return "", badRoot("%s is not the top of a git work tree", field)
	}
	return resolved, nil
}
