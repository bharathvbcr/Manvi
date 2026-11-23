package tui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui/term"
)

func drainRunnerActions(ctx context.Context, r *Runner) {
	for {
		r.drainLocal(ctx)
		select {
		case act := <-r.actions:
			r.apply(ctx, act)
		default:
			if r.localQueued() == 0 {
				return
			}
		}
	}
}

func TestResolveExternalEditor(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		wantName string
		wantArgs []string
		wantErr  bool
	}{
		{
			name:     "prefers VISUAL over EDITOR",
			env:      map[string]string{"VISUAL": "nvim -u NONE", "EDITOR": "nano"},
			wantName: "nvim",
			wantArgs: []string{"-u", "NONE"},
		},
		{
			name:     "falls back to EDITOR when VISUAL is whitespace",
			env:      map[string]string{"VISUAL": "   \t ", "EDITOR": "code --wait"},
			wantName: "code",
			wantArgs: []string{"--wait"},
		},
		{
			name:     "simple EDITOR",
			env:      map[string]string{"EDITOR": "vi"},
			wantName: "vi",
			wantArgs: nil,
		},
		{
			name:    "missing both VISUAL and EDITOR",
			env:     map[string]string{},
			wantErr: true,
		},
		{
			name:    "whitespace-only both",
			env:     map[string]string{"VISUAL": "  ", "EDITOR": "\t"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(k string) string { return tt.env[k] }
			name, args, err := resolveExternalEditor(getenv)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got name=%q args=%v", name, args)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if name != tt.wantName {
				t.Fatalf("name = %q, want %q", name, tt.wantName)
			}
			if !slices.Equal(args, tt.wantArgs) {
				t.Fatalf("args = %v, want %v", args, tt.wantArgs)
			}
		})
	}
}

func TestEditInExternalEditorRoundTripAndCleanup(t *testing.T) {
	var seenName string
	var seenArgs []string
	var tmpPath string
	var seenInitial string
	var seenPerm os.FileMode

	getenv := func(k string) string {
		if k == "VISUAL" {
			return "nvim -u NONE --clean"
		}
		return ""
	}
	run := func(name string, args ...string) error {
		seenName = name
		seenArgs = append([]string(nil), args...)
		tmpPath = args[len(args)-1]

		info, err := os.Stat(tmpPath)
		if err != nil {
			return err
		}
		seenPerm = info.Mode().Perm()

		raw, err := os.ReadFile(tmpPath)
		if err != nil {
			return err
		}
		seenInitial = string(raw)

		// Include CRLF and an intentional blank line followed by one POSIX trailing newline.
		return os.WriteFile(tmpPath, []byte("first line\r\nsecond line\n\n"), 0o600)
	}

	got, err := editInExternalEditor("draft text", getenv, run)
	if err != nil {
		t.Fatalf("editInExternalEditor failed: %v", err)
	}
	if seenName != "nvim" {
		t.Fatalf("command name = %q, want %q", seenName, "nvim")
	}
	if len(seenArgs) != 4 || seenArgs[0] != "-u" || seenArgs[1] != "NONE" || seenArgs[2] != "--clean" {
		t.Fatalf("args = %v, want [-u NONE --clean <tmp>]", seenArgs)
	}
	if !strings.HasPrefix(filepath.Base(tmpPath), "manvi-prompt-") || !strings.HasSuffix(tmpPath, ".md") {
		t.Fatalf("unexpected temp path %q", tmpPath)
	}
	if seenPerm&0o077 != 0 {
		t.Fatalf("temp file permissions %o are accessible to group/other", seenPerm)
	}
	if seenInitial != "draft text" {
		t.Fatalf("temp file initial content = %q, want %q", seenInitial, "draft text")
	}
	// Only one trailing newline is trimmed; intentional blank line stays.
	if got != "first line\nsecond line\n" {
		t.Fatalf("returned text = %q, want %q", got, "first line\nsecond line\n")
	}
	if _, statErr := os.Stat(tmpPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("temp file %q was not cleaned up (stat err=%v)", tmpPath, statErr)
	}
}

func TestEditInExternalEditorEmptyCancelAndSizeCap(t *testing.T) {
	t.Run("empty file clears draft and removes temp file", func(t *testing.T) {
		var tmpPath string
		got, err := editInExternalEditor(
			"old draft",
			func(k string) string {
				if k == "EDITOR" {
					return "nano"
				}
				return ""
			},
			func(_ string, args ...string) error {
				tmpPath = args[len(args)-1]
				return os.WriteFile(tmpPath, []byte("\n"), 0o600)
			},
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Fatalf("got %q, want empty string", got)
		}
		if _, statErr := os.Stat(tmpPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("temp file %q survived", tmpPath)
		}
	})

	t.Run("editor error cancels without returning partial content and cleans up temp file", func(t *testing.T) {
		var tmpPath string
		sentinel := errors.New("exit status 1")
		got, err := editInExternalEditor(
			"old draft",
			func(k string) string {
				if k == "EDITOR" {
					return "vim"
				}
				return ""
			},
			func(_ string, args ...string) error {
				tmpPath = args[len(args)-1]
				_ = os.WriteFile(tmpPath, []byte("aborted"), 0o600)
				return sentinel
			},
		)
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want wrapped %v", err, sentinel)
		}
		if got != "" {
			t.Fatalf("got %q on cancel, want empty", got)
		}
		if _, statErr := os.Stat(tmpPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("temp file %q survived after error", tmpPath)
		}
	})

	t.Run("oversized output is rejected and temp file removed", func(t *testing.T) {
		var tmpPath string
		_, err := editInExternalEditor(
			"",
			func(string) string { return "vi" },
			func(_ string, args ...string) error {
				tmpPath = args[len(args)-1]
				return os.WriteFile(tmpPath, bytes.Repeat([]byte("a"), maxExternalEditorBytes+1), 0o600)
			},
		)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("expected size cap error, got %v", err)
		}
		if _, statErr := os.Stat(tmpPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("temp file %q survived after size error", tmpPath)
		}
	})

	t.Run("missing runner returns clear error", func(t *testing.T) {
		_, err := editInExternalEditor("hi", func(string) string { return "vi" }, nil)
		if err == nil {
			t.Fatal("expected error for nil runner")
		}
	})
}

func TestExternalEditorKeybinding(t *testing.T) {
	for _, key := range []string{"alt+e", "f4"} {
		if got := resolve(key, CtxPrompt); got != CmdExternalEditor {
			t.Fatalf("resolve(%q, CtxPrompt) = %v, want CmdExternalEditor", key, got)
		}
		if got := resolve(key, CtxScrollback); got != CmdNone {
			t.Fatalf("resolve(%q, CtxScrollback) = %v, want CmdNone", key, got)
		}
	}
	if got := resolve("ctrl+e", CtxPrompt); got != CmdLineEnd {
		t.Fatalf("resolve(ctrl+e, CtxPrompt) = %v, want CmdLineEnd", got)
	}

	app, _ := newTestApp()
	app.AddSession("s1", "session 1")
	v := app.Current()
	longPaste := strings.Repeat("x", 220)
	v.Prompt.SetValue("prefix ")
	v.Prompt.InsertPaste(longPaste)

	effects := app.Dispatch(ActionKey{Binding: "alt+e"})
	if len(effects) != 1 {
		t.Fatalf("got %d effects, want 1 (%#v)", len(effects), effects)
	}
	eff, ok := effects[0].(EffectExternalEditor)
	if !ok {
		t.Fatalf("effect = %T, want EffectExternalEditor", effects[0])
	}
	if eff.SessionID != "s1" {
		t.Fatalf("SessionID = %q, want %q", eff.SessionID, "s1")
	}
	wantInitial := "prefix " + longPaste
	if eff.Initial != wantInitial {
		t.Fatalf("Initial len = %d, want %d (paste chip should expand)", len(eff.Initial), len(wantInitial))
	}

	// Non-prompt context must not launch external editor.
	v.Focus = CtxScrollback
	if effects := app.Dispatch(ActionKey{Binding: "alt+e"}); len(effects) != 0 {
		t.Fatalf("alt+e in scrollback emitted %#v, want none", effects)
	}
}

func TestRunnerApplyActionSetDraftUndoAndMissingEditor(t *testing.T) {
	ctx := context.Background()
	app, _ := newTestApp()
	v1 := app.AddSession("s1", "session 1")
	v2 := app.AddSession("s2", "session 2")
	v1.Prompt.SetValue("keep s1")
	v1.Focus = CtxScrollback
	v2.Prompt.SetValue("keep s2")

	r := &Runner{
		app:     app,
		actions: make(chan Action, 16),
		done:    make(chan struct{}),
	}

	// ActionSetDraft targets its SessionID even when s2 is active, and stays undoable.
	r.apply(ctx, ActionSetDraft{SessionID: "s1", Text: "updated in editor\nsecond line"})
	if v1.Prompt.Value() != "updated in editor\nsecond line" {
		t.Fatalf("v1.Prompt.Value() = %q, want updated draft", v1.Prompt.Value())
	}
	if v1.Focus != CtxPrompt {
		t.Fatalf("v1.Focus = %v, want CtxPrompt", v1.Focus)
	}
	if v2.Prompt.Value() != "keep s2" {
		t.Fatalf("v2.Prompt.Value() = %q, want %q", v2.Prompt.Value(), "keep s2")
	}
	if !v1.Prompt.Undo() {
		t.Fatal("Undo() returned false after ActionSetDraft")
	}
	if v1.Prompt.Value() != "keep s1" {
		t.Fatalf("after Undo, v1.Prompt.Value() = %q, want %q", v1.Prompt.Value(), "keep s1")
	}

	// Missing $VISUAL/$EDITOR fails gracefully, preserves the draft, and warns.
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	r.runEffect(ctx, EffectExternalEditor{SessionID: "s1", Initial: "keep s1"})
	drainRunnerActions(ctx, r)

	if v1.Prompt.Value() != "keep s1" {
		t.Fatalf("draft changed on missing editor: %q", v1.Prompt.Value())
	}
	notice, kind := app.Notice()
	if kind != StatusWarn || !strings.Contains(notice, "external editor") {
		t.Fatalf("notice = (%q, %v), want StatusWarn mentioning external editor", notice, kind)
	}
}

func TestCopyToClipboardAvoidsTruncatingLocalAndFallsBack(t *testing.T) {
	ctx := context.Background()
	largeText := strings.Repeat("a", osc52Limit+100)

	t.Run("remote session emits OSC 52 and warns above 96KB", func(t *testing.T) {
		var out bytes.Buffer
		f, err := os.CreateTemp(t.TempDir(), "notatty")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()

		app, _ := newTestApp()
		r := &Runner{
			app:     app,
			term:    term.New(f, f),
			actions: make(chan Action, 16),
			done:    make(chan struct{}),
		}
		// Unstarted terminal triggers OSC 52 path; remote env skips native.
		r.copyToClipboardWith(ctx, largeText, func(k string) string {
			if k == "SSH_TTY" {
				return "/dev/pts/0"
			}
			return ""
		}, func(context.Context, string, func(string) string) (string, error) {
			t.Fatal("native clipboard must not run over SSH")
			return "", nil
		})
		_ = out
		drainRunnerActions(ctx, r)
		notice, kind := app.Notice()
		if kind != StatusWarn || !strings.Contains(notice, "96KB") {
			t.Fatalf("notice = (%q, %v), want 96KB truncation warning", notice, kind)
		}
	})

	t.Run("ttyPaused suppresses OSC 52 writes", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "out")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()

		app, _ := newTestApp()
		r := &Runner{
			app:       app,
			term:      term.New(f, f),
			actions:   make(chan Action, 16),
			done:      make(chan struct{}),
			ttyPaused: true,
		}
		r.emitOSC52("secret", func(string) string { return "" })
		info, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != 0 {
			t.Fatalf("emitOSC52 wrote %d bytes while ttyPaused=true", info.Size())
		}
	})

	t.Run("fallback action posts warning when >96KB and unpaused", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "out")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()

		app, _ := newTestApp()
		r := &Runner{
			app:     app,
			term:    term.New(f, f),
			actions: make(chan Action, 16),
			done:    make(chan struct{}),
		}
		go func() {
			r.post(actionClipboardFallback{Text: largeText})
		}()
		select {
		case act := <-r.actions:
			r.apply(ctx, act)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for fallback action")
		}
		drainRunnerActions(ctx, r)
		data, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(data), "\x1b]52;c;") {
			t.Fatalf("expected OSC 52 write on fallback, got %q", string(data))
		}
		notice, kind := app.Notice()
		if kind != StatusWarn || !strings.Contains(notice, "96KB") {
			t.Fatalf("notice = (%q, %v), want 96KB truncation warning", notice, kind)
		}
	})
}

func TestTermRunExternalSafetyOnUnstartedAndNil(t *testing.T) {
	var nilTerm *term.Terminal
	called := false
	if err := nilTerm.RunExternal(func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("nil Terminal RunExternal returned %v", err)
	}
	if !called {
		t.Fatal("nil Terminal RunExternal did not run fn")
	}
	if err := nilTerm.RunExternal(nil); err != nil {
		t.Fatalf("nil fn returned %v", err)
	}

	f, err := os.CreateTemp(t.TempDir(), "notatty")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	tr := term.New(f, f)
	sentinel := errors.New("editor aborted")
	if got := tr.RunExternal(func() error { return sentinel }); !errors.Is(got, sentinel) {
		t.Fatalf("unstarted Terminal RunExternal err = %v, want %v", got, sentinel)
	}
}
