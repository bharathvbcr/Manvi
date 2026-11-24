package input

import "testing"

// The four control bytes after ESC are Ctrl plus a punctuation key, not Ctrl
// plus a letter: 0x1c is Ctrl+\, 0x1d Ctrl+], 0x1e Ctrl+^ and 0x1f Ctrl+_.
// The byte-to-letter rule gave them names no binding can be written in, so the
// undo binding the help names as ctrl+_ never fired.
func TestControlPunctuationBytesDecodeToTheirBindingNames(t *testing.T) {
	for in, want := range map[string]string{
		"\x1c": "ctrl+\\",
		"\x1d": "ctrl+]",
		"\x1e": "ctrl+^",
		"\x1f": "ctrl+_",
	} {
		if got := singleAs[Key](t, in).String(); got != want {
			t.Errorf("%q decoded as %q, want %q", in, got, want)
		}
	}
}
