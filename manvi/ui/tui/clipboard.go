package tui

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bharathvbcr/Manvi/manvi/childenv"
	"github.com/bharathvbcr/Manvi/manvi/mcp"
)

type clipboardBackend struct {
	name           string
	args           []string
	requireWayland bool
}

type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

const (
	// clipboardTimeout bounds native clipboard subprocesses so a hung Wayland or
	// X11 clipboard daemon can never stall the TUI.
	clipboardTimeout = 2 * time.Second
	// screenDCSChunkSize keeps each GNU screen DCS passthrough frame well below
	// screen's ~768-byte DCS buffer cap.
	screenDCSChunkSize = 76
)

var (
	darwinWriteBackends = []clipboardBackend{
		{name: "pbcopy"},
	}
	linuxWriteBackends = []clipboardBackend{
		{name: "wl-copy", requireWayland: true},
		{name: "xclip", args: []string{"-selection", "clipboard"}},
		{name: "xsel", args: []string{"--clipboard", "--input"}},
	}
	darwinReadBackends = []clipboardBackend{
		{name: "pbpaste"},
	}
	linuxReadBackends = []clipboardBackend{
		{name: "wl-paste", args: []string{"--no-newline"}, requireWayland: true},
		{name: "xclip", args: []string{"-o", "-selection", "clipboard"}},
		{name: "xsel", args: []string{"--clipboard", "--output"}},
	}
)

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.limit <= 0 {
		l.truncated = true
		return len(p), nil
	}
	rem := l.limit - l.buf.Len()
	if rem <= 0 {
		l.truncated = true
		return len(p), nil
	}
	if len(p) > rem {
		l.buf.Write(p[:rem])
		l.truncated = true
		return len(p), nil
	}
	l.buf.Write(p)
	return len(p), nil
}

// osc52Sequence builds an OSC 52 clipboard write escape sequence for text.
//
// Inside tmux ($TMUX) or GNU screen ($STY), raw OSC 52 is intercepted by the
// multiplexer unless wrapped in a Device Control String (DCS) passthrough
// envelope. Note: tmux 3.3+ defaults `allow-passthrough` to off, so users must
// enable `set -g allow-passthrough on` in tmux.conf for nested OSC 52 to reach
// the outer terminal. Under GNU screen, payloads are split into small DCS
// chunks so large copies never overflow screen's ~768-byte DCS limit.
func osc52Sequence(text string, getenv func(string) string) (seq string, truncated bool) {
	if len(text) > osc52Limit {
		cut := osc52Limit
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
		truncated = true
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(text))
	raw := "\x1b]52;c;" + encoded + "\x07"
	if getenv == nil {
		getenv = os.Getenv
	}
	switch {
	case getenv("TMUX") != "":
		// tmux DCS passthrough: ESC P tmux ; <payload with every ESC doubled> ESC \
		return "\x1bPtmux;" + strings.ReplaceAll(raw, "\x1b", "\x1b\x1b") + "\x1b\\", truncated
	case getenv("STY") != "":
		// GNU screen DCS passthrough: chunked ESC P <chunk> ESC \ frames.
		var b strings.Builder
		b.Grow(len(raw) + (len(raw)/screenDCSChunkSize+1)*4)
		for i := 0; i < len(raw); i += screenDCSChunkSize {
			end := i + screenDCSChunkSize
			if end > len(raw) {
				end = len(raw)
			}
			b.WriteString("\x1bP")
			b.WriteString(raw[i:end])
			b.WriteString("\x1b\\")
		}
		return b.String(), truncated
	default:
		return raw, truncated
	}
}

// isRemoteSession reports whether this terminal appears to be running over SSH.
func isRemoteSession(getenv func(string) string) bool {
	if getenv == nil {
		getenv = os.Getenv
	}
	return getenv("SSH_TTY") != "" || getenv("SSH_CONNECTION") != ""
}

// isAllowedClipboardTool reports whether name is one of the audited local
// clipboard executables.
func isAllowedClipboardTool(name string) bool {
	switch name {
	case "pbcopy", "pbpaste", "wl-copy", "wl-paste", "xclip", "xsel":
		return true
	default:
		return false
	}
}

// clipboardUntrustedRoots returns the working directory and any enclosing or
// configured workspace roots that an agent or checked-out repository may write.
func clipboardUntrustedRoots(getenv func(string) string) ([]string, error) {
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

// clipboardTrustedPath resolves symlinks on candidate and confirms it is an
// executable regular file outside every untrusted root.
func clipboardTrustedPath(candidate string, untrustedRoots []string) (string, bool) {
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

// resolveClipboardBinary locates an audited clipboard executable on PATH while
// rejecting empty, relative, working-directory, and workspace PATH entries.
func resolveClipboardBinary(name string, getenv func(string) string) (string, error) {
	if !isAllowedClipboardTool(name) {
		return "", fmt.Errorf("disallowed clipboard tool %q", name)
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	roots, err := clipboardUntrustedRoots(getenv)
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
		if resolved, ok := clipboardTrustedPath(p, roots); ok {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("clipboard binary %q not found in trusted PATH", name)
}

func clipboardBackends(write bool) []clipboardBackend {
	switch runtime.GOOS {
	case "darwin":
		if write {
			return darwinWriteBackends
		}
		return darwinReadBackends
	case "linux":
		if write {
			return linuxWriteBackends
		}
		return linuxReadBackends
	default:
		return nil
	}
}

func runClipboardWrite(ctx context.Context, path, text string, args ...string) error {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = childenv.Inherit()
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}

func runClipboardRead(ctx context.Context, path string, args ...string) (string, error) {
	out := &limitedBuffer{limit: maxPasteBytes}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = childenv.Inherit()
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	raw := out.buf.String()
	if out.truncated {
		for len(raw) > 0 && !utf8.RuneStart(raw[len(raw)-1]) {
			raw = raw[:len(raw)-1]
		}
		if len(raw) > 0 && !utf8.FullRuneInString(raw[len(raw)-1:]) {
			raw = raw[:len(raw)-1]
		}
	}
	return sanitizePasteText(raw), nil
}

// writeNativeClipboard copies text via the local OS clipboard tool when running
// locally (not over SSH). It returns the backend name on success.
func writeNativeClipboard(ctx context.Context, text string, getenv func(string) string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if isRemoteSession(getenv) {
		return "", errors.New("remote session")
	}
	tctx, cancel := context.WithTimeout(ctx, clipboardTimeout)
	defer cancel()

	for _, b := range clipboardBackends(true) {
		if b.requireWayland && getenv("WAYLAND_DISPLAY") == "" {
			continue
		}
		binPath, err := resolveClipboardBinary(b.name, getenv)
		if err != nil {
			continue
		}
		if err := runClipboardWrite(tctx, binPath, text, b.args...); err == nil {
			return b.name, nil
		}
	}
	return "", errors.New("no native clipboard backend available")
}

// readNativeClipboard reads plain text from the local OS clipboard for explicit
// Ctrl+V paste when running locally.
func readNativeClipboard(ctx context.Context, getenv func(string) string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if isRemoteSession(getenv) {
		return "", errors.New("remote session")
	}
	tctx, cancel := context.WithTimeout(ctx, clipboardTimeout)
	defer cancel()

	for _, b := range clipboardBackends(false) {
		if b.requireWayland && getenv("WAYLAND_DISPLAY") == "" {
			continue
		}
		binPath, err := resolveClipboardBinary(b.name, getenv)
		if err != nil {
			continue
		}
		if text, err := runClipboardRead(tctx, binPath, b.args...); err == nil {
			return text, nil
		}
	}
	return "", errors.New("no native clipboard backend available")
}
