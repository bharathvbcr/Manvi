// Package input decodes what a terminal sends into events an application can
// switch on.
//
// A terminal in raw mode delivers bytes, and the mapping from bytes to
// keystrokes is neither injective nor complete. Ctrl+I and Tab are the same
// byte. Escape is a key, and it is also the first byte of every arrow key. F1
// has at least three encodings in common use. A pasted paragraph arrives as
// ordinary keystrokes unless bracketed paste is on, and then it arrives wrapped
// in sequences that must not be interpreted as commands.
//
// The decoder is written as a pure function over a byte slice — decode returns
// the event and how many bytes it consumed, and asks for more bytes by
// consuming none. Every ambiguity in the paragraph above is therefore a table
// entry with a test, rather than state spread through a read loop that can only
// be exercised against a real terminal.
package input

import "fmt"

// Mod is a set of modifier keys.
type Mod uint8

// The modifiers a terminal can report. Shift is only reported for keys where
// the terminal has a distinct encoding — a shifted letter arrives as the
// uppercase rune with no modifier, which is why matching on ModShift for
// printable characters never works.
const (
	ModShift Mod = 1 << iota
	ModAlt
	ModCtrl
	ModMeta
)

// Has reports whether every modifier in m is set.
func (m Mod) Has(other Mod) bool { return m&other == other }

// String renders modifiers in a stable order for display in a shortcut bar.
func (m Mod) String() string {
	out := ""
	if m&ModCtrl != 0 {
		out += "ctrl+"
	}
	if m&ModAlt != 0 {
		out += "alt+"
	}
	if m&ModShift != 0 {
		out += "shift+"
	}
	if m&ModMeta != 0 {
		out += "meta+"
	}
	return out
}

// KeyType names a key that is not a printable character.
type KeyType int

// The named keys.
const (
	KeyRunes KeyType = iota // printable text; see Key.Runes
	KeyEnter
	KeyTab
	KeyBackTab
	KeyBackspace
	KeyDelete
	KeyInsert
	KeyEscape
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyPageUp
	KeyPageDown
	KeySpace
	KeyF1
	KeyF2
	KeyF3
	KeyF4
	KeyF5
	KeyF6
	KeyF7
	KeyF8
	KeyF9
	KeyF10
	KeyF11
	KeyF12
)

var keyNames = map[KeyType]string{
	KeyEnter: "enter", KeyTab: "tab", KeyBackTab: "shift+tab",
	KeyBackspace: "backspace", KeyDelete: "delete", KeyInsert: "insert",
	KeyEscape: "esc", KeyUp: "up", KeyDown: "down", KeyLeft: "left",
	KeyRight: "right", KeyHome: "home", KeyEnd: "end",
	KeyPageUp: "pgup", KeyPageDown: "pgdn", KeySpace: "space",
	KeyF1: "f1", KeyF2: "f2", KeyF3: "f3", KeyF4: "f4", KeyF5: "f5", KeyF6: "f6",
	KeyF7: "f7", KeyF8: "f8", KeyF9: "f9", KeyF10: "f10", KeyF11: "f11", KeyF12: "f12",
}

// Event is something the terminal reported.
type Event interface{ event() }

// Key is a keystroke.
type Key struct {
	Type  KeyType
	Runes []rune
	Mod   Mod
}

func (Key) event() {}

// Rune is the single character of a printable keystroke, or zero.
func (k Key) Rune() rune {
	if k.Type == KeyRunes && len(k.Runes) == 1 {
		return k.Runes[0]
	}
	return 0
}

// String renders the key as a binding name — "ctrl+c", "shift+tab", "a".
//
// This is the form key bindings are declared in, so it has to round-trip
// exactly. A shortcut bar that displays "ctrl+c" while the binding table holds
// something else is a UI that documents keys it does not implement.
func (k Key) String() string {
	if k.Type == KeyRunes {
		if len(k.Runes) == 0 {
			return k.Mod.String() + "?"
		}
		return k.Mod.String() + string(k.Runes)
	}
	name, ok := keyNames[k.Type]
	if !ok {
		name = fmt.Sprintf("key(%d)", int(k.Type))
	}
	// BackTab already means shift+tab; prefixing it again reads as
	// "shift+shift+tab".
	if k.Type == KeyBackTab {
		return name
	}
	return k.Mod.String() + name
}

// MouseAction is what the pointer did.
type MouseAction int

// The pointer actions.
const (
	MousePress MouseAction = iota
	MouseRelease
	MouseMotion
	MouseWheelUp
	MouseWheelDown
	MouseWheelLeft
	MouseWheelRight
)

// MouseButton identifies which button.
type MouseButton int

// The buttons.
const (
	MouseNone MouseButton = iota
	MouseLeft
	MouseMiddle
	MouseRight
)

// Mouse is a pointer event, in zero-based cell coordinates.
type Mouse struct {
	X, Y   int
	Button MouseButton
	Action MouseAction
	Mod    Mod
}

func (Mouse) event() {}

// Paste is a bracketed paste, delivered whole.
//
// Whole is the point. Without bracketed paste, pasting three lines into a
// prompt is indistinguishable from typing three lines and pressing Enter twice,
// so a paste sends the first line to the model and leaves the rest as a
// half-typed follow-up.
type Paste struct{ Text string }

func (Paste) event() {}

// Focus reports the terminal window gaining or losing focus.
type Focus struct{ In bool }

func (Focus) event() {}

// Resize reports a new terminal size. It is not decoded from the byte stream —
// it comes from SIGWINCH — but it travels on the same channel so the
// application has one event loop rather than two.
type Resize struct{ W, H int }

func (Resize) event() {}

// Error reports a failure on the input stream, including its end.
type Error struct{ Err error }

func (Error) event() {}
