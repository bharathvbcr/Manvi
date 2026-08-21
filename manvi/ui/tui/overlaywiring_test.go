package tui

import (
	"testing"

	"manvi/ui/render"
)

// The session picker was written whole — the overlay, its items, and the
// acceptOverlay case that switches a.active — and nothing ever opened it. The
// feature was one line short of existing, and no test noticed because every
// piece had one of its own.
func TestTheSessionPickerOpensAndSwitchesSessions(t *testing.T) {
	a, _ := newTestApp()
	a.AddSession("S2", "session two")
	if a.active != 1 {
		t.Fatalf("a new session did not become active: %d", a.active)
	}

	a.runCommand("sessions", "")
	if a.overlay == nil || a.overlay.Kind != OverlaySessions {
		t.Fatalf("/sessions did not open the picker: %+v", a.overlay)
	}

	// The picker opens on the first row; accept it.
	a.acceptOverlay()
	if a.overlay != nil {
		t.Error("the picker stayed open after a selection")
	}
	if a.active != 0 {
		t.Fatalf("active = %d, want the picked session (0)", a.active)
	}
	if a.mode != ModeSession {
		t.Errorf("picking a session did not enter session mode: %v", a.mode)
	}
}

// Opening the theme picker and choosing an entry must actually change the
// theme. Before this, OverlayTheme had no case in acceptOverlay at all, so the
// overlay — had anything opened it — was a dead end the user could only escape.
func TestTheThemePickerAppliesTheChosenTheme(t *testing.T) {
	a, _ := newTestApp()
	if a.Theme.Name != "dark" {
		t.Fatalf("the test app did not start dark: %q", a.Theme.Name)
	}

	a.runCommand("theme", "")
	if a.overlay == nil || a.overlay.Kind != OverlayTheme {
		t.Fatalf("/theme did not open the theme picker: %+v", a.overlay)
	}

	// The items are dark, light, plain in that order; step to light.
	a.overlay.Move(1)
	a.acceptOverlay()
	if a.overlay != nil {
		t.Error("the picker stayed open after a selection")
	}
	if a.Theme.Name != "light" {
		t.Fatalf("theme = %q, want light", a.Theme.Name)
	}
}

// Switching to plain and back must not strip colour permanently.
//
// This is the trap the wiring had to avoid. NoColorTheme sets Profile to
// NoColor whatever the terminal supports, so resolving the next theme against
// the *outgoing* theme would hand a colour terminal a dark palette with every
// colour flattened — and the terminal would look broken with nothing to point
// at. The capability is kept on the App instead.
func TestSwitchingThroughPlainKeepsTheTerminalsColour(t *testing.T) {
	a, _ := newTestApp()
	a.Profile = render.ANSI256
	a.Unicode = true

	// dark, light, plain — step to the one wanted.
	pick := func(offset int) {
		a.runCommand("theme", "")
		a.overlay.Move(offset)
		a.acceptOverlay()
	}

	pick(2)
	if a.Theme.Name != "plain" {
		t.Fatalf("theme = %q, want plain", a.Theme.Name)
	}
	pick(0)
	if a.Theme.Name != "dark" {
		t.Fatalf("theme = %q, want dark", a.Theme.Name)
	}
	if a.Theme.Profile != render.ANSI256 {
		t.Fatalf("profile = %v, want the terminal's own %v: a trip through plain flattened the palette",
			a.Theme.Profile, render.ANSI256)
	}
}

// Both commands must be recognised, or the palette reports "no command".
func TestTheNewCommandsAreRecognised(t *testing.T) {
	for _, name := range []string{"sessions", "theme"} {
		a, _ := newTestApp()
		a.runCommand(name, "")
		if a.overlay == nil {
			t.Errorf("/%s was not recognised; it produced no overlay", name)
		}
		if a.notice != "" {
			t.Errorf("/%s produced a notice %q, so it was treated as unknown", name, a.notice)
		}
	}
}
