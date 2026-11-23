package tui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// maxExternalEditorBytes bounds how much text is read back from the temporary
// editor file so an accidental binary or multi-megabyte paste cannot exhaust UI memory.
const maxExternalEditorBytes = 4 << 20

// resolveExternalEditor resolves the operator's external editor command from
// $VISUAL then $EDITOR, splitting flags safely via strings.Fields.
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
	return parts[0], parts[1:], nil
}

// editInExternalEditor writes initial to a private 0600 temporary markdown
// file, closes it before invoking run with the resolved $VISUAL/$EDITOR
// command and the file path, and reads back the updated text by path (capped at
// maxExternalEditorBytes, with line endings normalized and one trailing newline
// trimmed). The temporary file is always removed before returning.
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

	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	return strings.TrimSuffix(normalized, "\n"), nil
}
