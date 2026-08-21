package ui

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Sanitize makes untrusted text safe to write to a terminal.
//
// Everything the harness renders that it did not compose itself is untrusted:
// model output, file contents, tool results, task titles out of the store,
// error bodies from a provider. Any of it can contain terminal control
// sequences, and a terminal executes those rather than displaying them.
//
// The consequences are not cosmetic. A CSI sequence can move the cursor up and
// overwrite lines already printed, so a transcript can be rewritten after the
// fact — including the record of what a tool actually did. OSC 8 embeds a
// hyperlink whose visible text need not match its destination. OSC 52 writes
// the system clipboard on terminals that allow it. And the one that matters
// most here: the harness prints approval prompts to this same terminal, so
// escapes inside model output can redraw the prompt a human is answering. A
// human-in-the-loop control that untrusted content can repaint is not a
// control.
//
// So the rule is allow-list, not deny-list: printable text, plus the specific
// whitespace that ordinary output needs. Everything else becomes a visible
// marker, because silently dropping it would let a caller hide content inside
// characters that never appear.
func Sanitize(text string) string {
	if isPlain(text) {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == '\r':
			// A bare carriage return returns the cursor to the start of the
			// line, so following text overwrites what is already there. It is
			// dropped rather than marked because CRLF line endings are
			// ordinary and marking every one would be noise.
		case r == utf8.RuneError:
			b.WriteRune('�')
		case unicode.IsControl(r):
			b.WriteString(controlMarker(r))
		case isBidiControl(r):
			// Bidirectional overrides reorder displayed text without changing
			// the bytes, so what a reviewer reads can differ from what is
			// there. This is the Trojan Source problem, and a diff viewer is
			// exactly where it does damage.
			b.WriteString(markerFor(r))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isPlain reports whether text needs no work, which is the overwhelmingly
// common case and worth not allocating for.
func isPlain(text string) bool {
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c < 0x20 && c != '\n' && c != '\t' {
			return false
		}
		if c == 0x7f {
			return false
		}
		if c >= utf8.RuneSelf {
			// Any multi-byte content takes the slow path, which is where the
			// bidi and replacement-character checks live.
			return false
		}
	}
	return true
}

func controlMarker(r rune) string {
	switch r {
	case 0x1b:
		return "\\e"
	case 0x07:
		return "\\a"
	case 0x08:
		return "\\b"
	case 0x0c:
		return "\\f"
	case 0x0b:
		return "\\v"
	}
	return markerFor(r)
}

func markerFor(r rune) string {
	const hex = "0123456789abcdef"
	if r <= 0xff {
		return "\\x" + string([]byte{hex[(r>>4)&0xf], hex[r&0xf]})
	}
	return "\\u" + string([]byte{
		hex[(r>>12)&0xf], hex[(r>>8)&0xf], hex[(r>>4)&0xf], hex[r&0xf],
	})
}

// isBidiControl reports whether a rune reorders displayed text.
func isBidiControl(r rune) bool {
	switch r {
	case '؜', // arabic letter mark
		'‎', '‏', // left/right-to-left marks
		'‪', '‫', '‬', '‭', '‮', // embeddings and overrides
		'⁦', '⁧', '⁨', '⁩': // isolates
		return true
	}
	return false
}

// Truncate bounds a string to n runes, marking that it was cut.
//
// The count is in runes rather than bytes so a multi-byte character is never
// split into an invalid sequence, and the marker is included so a truncated
// tool result is never mistaken for a complete one.
func Truncate(text string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	runes := []rune(text)
	return string(runes[:n]) + "… [truncated]"
}
