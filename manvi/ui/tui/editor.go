package tui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/mcp"
)

// maxExternalEditorBytes bounds how much text is read back from the temporary
// editor file so an accidental binary or multi-megabyte paste cannot exhaust UI memory.
const maxExternalEditorBytes = 4 << 20

// resolveExternalEditor resolves the operator's external editor command from
// $VISUAL then $EDITOR, splitting flags safely via strings.Fields.
//
// An absolute command path is accepted as operator-pinned; a relative path with
// a separator (such as "./vim") is rejected; and a bare command name is
// resolved across absolute PATH directories only, rejecting symlinks or targets
// inside the working directory or repository root.
func resolveExternalEditor(getenv func(string) string) (name string, args []string, err error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	raw := strings.TrimSpace(getenv("VISUAL"))
	if raw == "" {
		raw = strings.TrimSpace(getenv("EDITOR"))
	}
	if raw == "" {
		return "", nil, errors.New("no $VISUAL or $EDITOR configured")
	}
	parts := strings.Fields(raw)
	if len(parts) == 0 {
		return "", nil, errors.New("no $VISUAL or $EDITOR configured")
	}
	cmdName := parts[0]
	if strings.ContainsRune(cmdName, os.PathSeparator) || strings.ContainsRune(cmdName, '/') {
		if !filepath.IsAbs(cmdName) {
			return "", nil, fmt.Errorf("refusing relative external editor path %q", cmdName)
		}
		return filepath.Clean(cmdName), parts[1:], nil
	}
	resolved, ok := trustedLookPathEditor(cmdName, getenv("PATH"))
	if !ok {
		return "", nil, fmt.Errorf("external editor %q not found in a trusted PATH directory", cmdName)
	}
	return resolved, parts[1:], nil
}

func trustedLookPathEditor(name, pathEnv string) (string, bool) {
	for _, dir := range filepath.SplitList(pathEnv) {
		if !filepath.IsAbs(dir) {
			continue
		}
		p, err := exec.LookPath(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if resolved, ok := trustedResolvedEditor(p); ok {
			return resolved, true
		}
	}
	return "", false
}

func trustedResolvedEditor(candidate string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil || isAgentWritableEditorPath(resolved) {
		return "", false
	}
	return resolved, true
}

func isAgentWritableEditorPath(path string) bool {
	cwd, err := os.Getwd()
	if err != nil {
		return true
	}
	roots := []string{cwd}
	if repoRoot := findEditorRepoRoot(cwd); repoRoot != "" && repoRoot != cwd {
		roots = append(roots, repoRoot)
	}
	for _, root := range roots {
		if clean := filepath.Clean(root); clean == filepath.Dir(clean) {
			continue
		}
		inside, err := mcp.PathIsInside(root, path)
		if err != nil || inside {
			return true
		}
	}
	return false
}

func findEditorRepoRoot(start string) string {
	dir := filepath.Clean(start)
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// sanitizeEditorText normalizes line endings and strips C0, DEL, and C1 control
// characters (preserving \n and \t) from text read back from an external editor.
func sanitizeEditorText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\t' {
			b.WriteRune(r)
			continue
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// editInExternalEditor writes initial to a private 0600 temporary markdown
// file, closes it before invoking run with the resolved $VISUAL/$EDITOR
// command and the file path, and reads back the updated text by path (capped at
// maxExternalEditorBytes, sanitized of C0/C1 controls, with line endings
// normalized and one trailing newline trimmed). The temporary file is always
// removed before returning.
func editInExternalEditor(
	initial string,
	getenv func(string) string,
	run func(name string, args ...string) error,
) (string, error) {
	if run == nil {
		return "", errors.New("external editor runner is not configured")
	}
	name, baseArgs, err := resolveExternalEditor(getenv)
	if err != nil {
		return "", err
	}

	tmp, err := os.CreateTemp("", "manvi-prompt-*.md")
	if err != nil {
		return "", fmt.Errorf("create temp prompt file: %w", err)
	}
	path := tmp.Name()
	defer func() { _ = os.Remove(path) }()

	if initial != "" {
		if _, err := io.WriteString(tmp, initial); err != nil {
			_ = tmp.Close()
			return "", fmt.Errorf("write temp prompt file: %w", err)
		}
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close temp prompt file: %w", err)
	}

	args := make([]string, 0, len(baseArgs)+1)
	args = append(args, baseArgs...)
	args = append(args, path)
	if err := run(name, args...); err != nil {
		return "", fmt.Errorf("editor %q failed: %w", name, err)
	}

	f, err := os.Open(path) //nolint:gosec // path was created by os.CreateTemp above
	if err != nil {
		return "", fmt.Errorf("open edited prompt: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(f, maxExternalEditorBytes+1))
	_ = f.Close()
	if readErr != nil {
		return "", fmt.Errorf("read edited prompt: %w", readErr)
	}
	if len(data) > maxExternalEditorBytes {
		return "", fmt.Errorf("edited prompt exceeds %d bytes", maxExternalEditorBytes)
	}

	return strings.TrimSuffix(sanitizeEditorText(string(data)), "\n"), nil
}
