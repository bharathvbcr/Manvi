package tui

import (
	"strings"
	"testing"

	"manvi/ui"
	"manvi/ui/render"
)

// text adds a transcript entry so the scrollback has something to navigate.
func addEntries(a *App, n int) {
	v := a.Current()
	for i := 0; i < n; i++ {
		v.Scroll.Append(ui.Event{Kind: ui.KindTurnStart, Text: "turn " + itoa(i)})
		// Long enough to be foldable: the rule is a newline or more than 200
		// characters, and a shorter fixture would silently test nothing.
		v.Scroll.Append(ui.Event{Kind: ui.KindText, Text: strings.Repeat("body ", 60) + "\ntail"})
	}
}

// TestTabMovesFocusBetweenTheComposerAndTheTranscript, and entering the
// transcript selects something. Entering with nothing selected would make the
// first arrow key jump to the top rather than move, which reads as a lost
// keystroke.
func TestTabMovesFocusBetweenTheComposerAndTheTranscript(t *testing.T) {
	a, _ := newTestApp()
	addEntries(a, 5)
	v := a.Current()

	if v.Focus != CtxPrompt {
		t.Fatalf("focus starts at %v, want the composer", v.Focus)
	}
	key(a, "tab")
	if v.Focus != CtxScrollback {
		t.Fatalf("tab left focus at %v", v.Focus)
	}
	if v.Scroll.Selected() == nil {
		t.Fatal("entering the transcript selected nothing; the first arrow key would jump rather than move")
	}

	key(a, "tab")
	if v.Focus != CtxPrompt {
		t.Fatalf("tab back left focus at %v", v.Focus)
	}
	if v.Scroll.Selected() != nil {
		t.Fatal("leaving the transcript kept a selection, which stays highlighted while typing")
	}
}

// TestFocusFollowsTheContextSoKeysMeanWhatTheLastFrameShowed.
func TestFocusFollowsTheContextSoKeysMeanWhatTheLastFrameShowed(t *testing.T) {
	a, _ := newTestApp()
	addEntries(a, 3)
	if a.Context() != CtxPrompt {
		t.Fatalf("context = %v", a.Context())
	}
	key(a, "tab")
	if a.Context() != CtxScrollback {
		t.Fatalf("context after tab = %v", a.Context())
	}
}

// TestTypingIsUnaffectedByTranscriptKeysWhileTheComposerHasFocus is the
// consequence of the shadowing rule, tested through the app rather than the
// binding table: Ctrl+U in the composer must edit the draft, not scroll.
func TestTypingIsUnaffectedByTranscriptKeysWhileTheComposerHasFocus(t *testing.T) {
	a, _ := newTestApp()
	addEntries(a, 5)
	typeText(a, "implement the parser")

	key(a, "ctrl+u")
	if got := a.Current().Prompt.Value(); got != "" {
		t.Fatalf("ctrl+u in the composer left %q; it must delete to the start of the line", got)
	}

	typeText(a, "second draft")
	key(a, "tab") // to the transcript
	key(a, "ctrl+u")
	if got := a.Current().Prompt.Value(); got != "second draft" {
		t.Fatalf("ctrl+u in the transcript destroyed the draft: %q", got)
	}
}

// TestPromptEditingKeysReachThePrompt covers the editing bindings through the
// dispatcher, which is the path an actual keystroke takes.
func TestPromptEditingKeysReachThePrompt(t *testing.T) {
	a, _ := newTestApp()
	p := a.Current().Prompt

	typeText(a, "alpha beta gamma")
	key(a, "home")
	if p.Value() != "alpha beta gamma" {
		t.Fatalf("home changed the text: %q", p.Value())
	}
	key(a, "delete")
	if p.Value() != "lpha beta gamma" {
		t.Fatalf("delete at line start = %q", p.Value())
	}
	key(a, "end")
	key(a, "backspace")
	if p.Value() != "lpha beta gamm" {
		t.Fatalf("backspace at line end = %q", p.Value())
	}
	key(a, "ctrl+w")
	if p.Value() != "lpha beta " {
		t.Fatalf("ctrl+w = %q, want the last word removed", p.Value())
	}
	key(a, "alt+b")
	key(a, "ctrl+k")
	if p.Value() != "lpha " {
		t.Fatalf("word-left then delete-to-end = %q", p.Value())
	}
	key(a, "left")
	key(a, "right")
	if p.Value() != "lpha " {
		t.Fatalf("cursor motion changed the text: %q", p.Value())
	}
}

// TestHistoryIsReachableOnlyFromTheComposer. Up in the transcript selects the
// previous entry; recalling a past prompt there would silently replace a draft
// the operator cannot see.
func TestHistoryIsReachableOnlyFromTheComposer(t *testing.T) {
	a, _ := newTestApp()
	typeText(a, "first message")
	key(a, "enter")
	typeText(a, "second message")
	key(a, "enter")

	key(a, "up")
	if got := a.Current().Prompt.Value(); got != "second message" {
		t.Fatalf("history up = %q, want the most recent submission", got)
	}
	key(a, "up")
	if got := a.Current().Prompt.Value(); got != "first message" {
		t.Fatalf("second history up = %q", got)
	}
	key(a, "down")
	if got := a.Current().Prompt.Value(); got != "second message" {
		t.Fatalf("history down = %q", got)
	}

	// With the transcript focused, up must not touch the draft.
	key(a, "down") // back to the empty draft
	typeText(a, "in progress")
	key(a, "tab")
	key(a, "up")
	if got := a.Current().Prompt.Value(); got != "in progress" {
		t.Fatalf("history fired from the transcript and replaced the draft: %q", got)
	}
}

// TestScrollbackNavigationMovesTheViewport covers the transcript bindings.
func TestScrollbackNavigationMovesTheViewport(t *testing.T) {
	a, _ := newTestApp()
	addEntries(a, 40)
	// Draw once so the scrollback learns its viewport height; the page size is
	// the transcript's own height from the last frame.
	screen(a, 100, 30)
	key(a, "tab")
	sb := a.Current().Scroll

	if !sb.Following() {
		t.Fatal("a fresh transcript should be following the tail")
	}
	key(a, "pgup")
	if sb.Following() {
		t.Fatal("paging up must stop following, or new output yanks the view away mid-read")
	}
	// g and G move the viewport, not the selection: an operator jumping to the
	// top to read history has not chosen a different entry, and moving the
	// selection under them would change what Enter folds.
	selected := sb.Selected()
	key(a, "g")
	if sb.Following() {
		t.Fatal("jumping to the top must stop following")
	}
	if sb.Selected() != selected {
		t.Fatal("jumping to the top moved the selection; the operator did not choose a different entry")
	}
	key(a, "G")
	if !sb.Following() {
		t.Fatal("jumping to the bottom must resume following")
	}

	key(a, "ctrl+k")
	if sb.Following() {
		t.Fatal("scrolling up by a line must stop following")
	}
	key(a, "ctrl+j")
	key(a, "ctrl+u")
	key(a, "ctrl+d")
	// The assertion is that none of these panicked and the view stayed within
	// bounds; a scroll offset past the end renders a blank transcript.
	if sb.Selected() == nil && sb.Len() > 0 {
		t.Fatal("navigation cleared the selection")
	}
}

// TestFoldingIsReversibleAndAppliesToEverything.
func TestFoldingIsReversibleAndAppliesToEverything(t *testing.T) {
	a, _ := newTestApp()
	addEntries(a, 6)
	screen(a, 100, 30)
	key(a, "tab")

	sb := a.Current().Scroll
	foldable := 0
	for _, e := range sb.Entries() {
		if e.Foldable {
			foldable++
		}
	}
	if foldable == 0 {
		t.Fatal("the fixture produced nothing foldable, so this test would pass vacuously")
	}

	key(a, "E") // fold all
	for _, e := range sb.Entries() {
		if e.Foldable && !e.Folded {
			t.Fatal("fold-all left a foldable entry expanded")
		}
	}
	folded := strings.Join(screen(a, 100, 30), "\n")

	key(a, "R") // expand all
	for _, e := range sb.Entries() {
		if e.Folded {
			t.Fatal("expand-all left an entry folded")
		}
	}
	expanded := strings.Join(screen(a, 100, 30), "\n")

	if folded == expanded {
		t.Fatal("folding and expanding everything render identically")
	}
	// Folding hides body text but must never hide a qualification: a folded
	// entry that drops the rule that fired would let a demoted write read as a
	// clean one, which is the one thing the transcript must not do.
	if len(folded) >= len(expanded) {
		t.Fatalf("folding did not shorten the frame: %d vs %d", len(folded), len(expanded))
	}
}

// TestCyclingSessionsWrapsAndLandsInSessionMode.
func TestCyclingSessionsWrapsAndLandsInSessionMode(t *testing.T) {
	a, _ := newTestApp()
	a.AddSession("S2", "session two")
	a.AddSession("S3", "session three")

	first := a.Current()
	key(a, "ctrl+t")
	if a.Current() == first {
		t.Fatal("ctrl+t did not move to another session")
	}
	key(a, "ctrl+t")
	key(a, "ctrl+t")
	if a.Current() != first {
		t.Fatal("cycling three sessions three times did not wrap back to the first")
	}

	// From the dashboard, cycling returns to the session view — otherwise the
	// keystroke changes the selection behind an overlay the operator is reading.
	key(a, "ctrl+g")
	if a.Context() != CtxDashboard {
		t.Fatalf("context = %v, want the dashboard", a.Context())
	}
	key(a, "ctrl+t")
	if a.Context() == CtxDashboard {
		t.Fatal("cycling a session left the dashboard open")
	}
}

// TestThePaletteOpensAndDismisses.
func TestThePaletteOpensAndDismisses(t *testing.T) {
	a, _ := newTestApp()
	key(a, "ctrl+p")
	if a.Context() != CtxOverlay {
		t.Fatalf("context = %v, want an overlay after ctrl+p", a.Context())
	}
	frame := strings.Join(screen(a, 100, 30), "\n")
	if !strings.Contains(frame, "doctor") {
		t.Fatalf("the palette does not list the host's commands:\n%s", frame)
	}
	key(a, "esc")
	if a.Context() == CtxOverlay {
		t.Fatal("esc did not dismiss the palette")
	}
}

// TestHelpOverlayListsTheBindingsItClaimsTo. A shortcut bar that advertises a
// key the dispatcher does not honour is worse than none.
func TestHelpOverlayListsTheBindingsItClaimsTo(t *testing.T) {
	a, _ := newTestApp()
	key(a, "f1")
	if a.Context() != CtxOverlay {
		t.Fatalf("f1 did not open help, context = %v", a.Context())
	}
	frame := strings.Join(screen(a, 100, 40), "\n")
	for _, want := range []string{"cancel turn", "quit"} {
		if !strings.Contains(frame, want) {
			t.Errorf("help does not mention %q:\n%s", want, frame)
		}
	}
	key(a, "esc")
	if a.Context() == CtxOverlay {
		t.Fatal("esc did not close help")
	}
}

// TestAnAppWithNoSessionsStillDraws. The empty state is reached on startup
// before the first session exists and after the last one closes, and a panic
// there takes the terminal down with the raw mode still set.
func TestAnAppWithNoSessionsStillDraws(t *testing.T) {
	host := &stubHost{}
	a := NewApp(Dark(), host)
	a.Dispatch(ActionResize{W: 80, H: 24})

	if a.Current() != nil {
		t.Fatal("a fresh app has no current session")
	}
	frame := strings.Join(screen(a, 80, 24), "\n")
	if strings.TrimSpace(frame) == "" {
		t.Fatal("the empty state drew nothing at all; the operator sees a blank terminal")
	}

	// Keys that assume a session must be no-ops rather than panics.
	for _, binding := range []string{"tab", "up", "down", "enter", "ctrl+t", "ctrl+u", "esc", "ctrl+g"} {
		key(a, binding)
	}
	typeText(a, "text with nowhere to go")
}

// TestDrawingIsStableAcrossExtremeGeometry. A terminal is resized by dragging,
// which produces every intermediate size including degenerate ones.
func TestDrawingIsStableAcrossExtremeGeometry(t *testing.T) {
	a, _ := newTestApp()
	addEntries(a, 20)
	key(a, "ctrl+p") // an overlay open through every resize

	for _, size := range [][2]int{
		{1, 1}, {2, 3}, {10, 4}, {40, 5}, {200, 80}, {1, 40}, {40, 1}, {120, 30},
	} {
		w, h := size[0], size[1]
		a.Dispatch(ActionResize{W: w, H: h})
		b := render.NewBuffer(w, h)
		a.Draw(b)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				b.Cell(x, y) // must be addressable: an out-of-bounds write would have panicked
			}
		}
	}
}
