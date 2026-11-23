package tui

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bharathvbcr/Manvi/manvi/childenv"
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
	screenDCSChunkSize       = 76
	clipboardTruncatedNotice = "[truncated to 1 MiB]"
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

// resolveClipboardBinary locates an audited clipboard executable on PATH while
// rejecting empty, relative, working-directory, and workspace PATH entries.
func resolveClipboardBinary(name string, getenv func(string) string) (string, error) {
	if !isAllowedClipboardTool(name) {
		return "", fmt.Errorf("disallowed clipboard tool %q", name)
	}
	resolved, err := resolveTrustedPathBinary(name, getenv)
	if err != nil {
		return "", fmt.Errorf("clipboard binary %q not found in trusted PATH: %w", name, err)
	}
	return resolved, nil
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

func runClipboardRead(ctx context.Context, path string, args ...string) (string, bool, error) {
	out := &limitedBuffer{limit: maxPasteBytes}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = childenv.Inherit()
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		return "", false, err
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
	clean, sanTruncated := sanitizePasteText(raw)
	return clean, out.truncated || sanTruncated, nil
}

func appendClipboardTruncatedNotice(text string) string {
	if text == "" {
		return clipboardTruncatedNotice
	}
	if strings.HasSuffix(text, "\n") && len(text)+len(clipboardTruncatedNotice) <= maxPasteBytes {
		return text + clipboardTruncatedNotice
	}
	suffix := "\n" + clipboardTruncatedNotice
	limit := maxPasteBytes - len(suffix)
	if len(text) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	return text + suffix
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
		if text, truncated, err := runClipboardRead(tctx, binPath, b.args...); err == nil {
			if truncated {
				text = appendClipboardTruncatedNotice(text)
			}
			return text, nil
		}
	}
	return "", errors.New("no native clipboard backend available")
}
