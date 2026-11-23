package tui

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// clipboardTimeout bounds native clipboard subprocesses so a hung Wayland or
// X11 clipboard daemon can never stall the TUI.
const clipboardTimeout = 2 * time.Second

// osc52Sequence builds an OSC 52 clipboard write escape sequence for text.
//
// Inside tmux ($TMUX) or GNU screen ($STY), raw OSC 52 is intercepted by the
// multiplexer unless wrapped in a Device Control String (DCS) passthrough
// envelope. Wrapping automatically lets copy work cleanly inside nested tmux
// and SSH sessions.
func osc52Sequence(text string, getenv func(string) string) (seq string, truncated bool) {
	if len(text) > osc52Limit {
		text = text[:osc52Limit]
		truncated = true
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(text))
	raw := "\x1b]52;c;" + encoded + "\x07"
	if getenv == nil {
		getenv = os.Getenv
	}
	switch {
	case getenv("TMUX") != "":
		// tmux DCS passthrough: ESC P tmux ; <payload with ESC doubled> ESC \
		return "\x1bPtmux;\x1b" + raw + "\x1b\\", truncated
	case getenv("STY") != "":
		// GNU screen DCS passthrough: ESC P <payload> ESC \
		return "\x1bP" + raw + "\x1b\\", truncated
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

// writeNativeClipboard copies text via the local OS clipboard tool when running
// locally (not over SSH). It returns the backend name on success.
func writeNativeClipboard(ctx context.Context, text string, getenv func(string) string) (string, error) {
	if isRemoteSession(getenv) {
		return "", errors.New("remote session")
	}
	tctx, cancel := context.WithTimeout(ctx, clipboardTimeout)
	defer cancel()

	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("pbcopy"); err != nil {
			return "", err
		}
		cmd := exec.CommandContext(tctx, "pbcopy")
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err != nil {
			return "", err
		}
		return "pbcopy", nil
	case "linux":
		if getenv == nil {
			getenv = os.Getenv
		}
		if getenv("WAYLAND_DISPLAY") != "" {
			if _, err := exec.LookPath("wl-copy"); err == nil {
				cmd := exec.CommandContext(tctx, "wl-copy")
				cmd.Stdin = strings.NewReader(text)
				if err := cmd.Run(); err == nil {
					return "wl-copy", nil
				}
			}
		}
		if _, err := exec.LookPath("xclip"); err == nil {
			cmd := exec.CommandContext(tctx, "xclip", "-selection", "clipboard")
			cmd.Stdin = strings.NewReader(text)
			if err := cmd.Run(); err == nil {
				return "xclip", nil
			}
		}
		if _, err := exec.LookPath("xsel"); err == nil {
			cmd := exec.CommandContext(tctx, "xsel", "--clipboard", "--input")
			cmd.Stdin = strings.NewReader(text)
			if err := cmd.Run(); err == nil {
				return "xsel", nil
			}
		}
	}
	return "", errors.New("no native clipboard backend available")
}

// readNativeClipboard reads plain text from the local OS clipboard for explicit
// Ctrl+V paste when running locally.
func readNativeClipboard(ctx context.Context, getenv func(string) string) (string, error) {
	if isRemoteSession(getenv) {
		return "", errors.New("remote session")
	}
	tctx, cancel := context.WithTimeout(ctx, clipboardTimeout)
	defer cancel()

	var out bytes.Buffer
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("pbpaste"); err != nil {
			return "", err
		}
		cmd := exec.CommandContext(tctx, "pbpaste")
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			return "", err
		}
		return out.String(), nil
	case "linux":
		if getenv == nil {
			getenv = os.Getenv
		}
		if getenv("WAYLAND_DISPLAY") != "" {
			if _, err := exec.LookPath("wl-paste"); err == nil {
				cmd := exec.CommandContext(tctx, "wl-paste", "--no-newline")
				cmd.Stdout = &out
				if err := cmd.Run(); err == nil {
					return out.String(), nil
				}
				out.Reset()
			}
		}
		if _, err := exec.LookPath("xclip"); err == nil {
			cmd := exec.CommandContext(tctx, "xclip", "-o", "-selection", "clipboard")
			cmd.Stdout = &out
			if err := cmd.Run(); err == nil {
				return out.String(), nil
			}
			out.Reset()
		}
		if _, err := exec.LookPath("xsel"); err == nil {
			cmd := exec.CommandContext(tctx, "xsel", "--clipboard", "--output")
			cmd.Stdout = &out
			if err := cmd.Run(); err == nil {
				return out.String(), nil
			}
		}
	}
	return "", errors.New("no native clipboard backend available")
}
