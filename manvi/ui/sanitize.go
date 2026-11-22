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
	return sanitizeSlow(text)
}

// sanitizeSlow is the rewriting pass. It is split from Sanitize only so the
// fast path can be tested against it: isPlain is allowed to accept a string
// only if this would hand back the same string.
func sanitizeSlow(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for i, r := range text {
		switch {
		case r == zwj && zwjJoinsEmoji(text, i):
			// A joiner between two pictographs is what makes one emoji out of
			// several; it is ordinary text, not a hidden character.
			b.WriteRune(r)
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
		case isInvisibleFormat(r):
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
//
// It accepts only what sanitizeSlow would copy through untouched. Multi-byte
// runes used to be refused wholesale, which sent the harness's own output —
// every "—" and "✓" it prints — down the allocating path to be copied byte for
// byte. They are decoded here instead, and refused only for the
// reasons the slow path would rewrite them: a byte that does not decode, a C1
// control, a bidi control. A literal U+FFFD is refused too, though the slow
// path would leave it alone: decoding cannot tell it from a bad byte without a
// second look, and erring towards the slow path costs an allocation where
// erring the other way would let something through unexamined.
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
			r, size := utf8.DecodeRuneInString(text[i:])
			if r == zwj && zwjJoinsEmoji(text, i) {
				i += size - 1
				continue
			}
			if r == utf8.RuneError || unicode.IsControl(r) || isInvisibleFormat(r) {
				return false
			}
			i += size - 1
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
	if r > 0xffff {
		return "\\U" + string([]byte{
			hex[(r>>28)&0xf], hex[(r>>24)&0xf], hex[(r>>20)&0xf], hex[(r>>16)&0xf],
			hex[(r>>12)&0xf], hex[(r>>8)&0xf], hex[(r>>4)&0xf], hex[r&0xf],
		})
	}
	return "\\u" + string([]byte{
		hex[(r>>12)&0xf], hex[(r>>8)&0xf], hex[(r>>4)&0xf], hex[r&0xf],
	})
}

const zwj = '\u200d'

// zwjJoinsEmoji reports whether the zero-width joiner at byte offset i of text
// sits between two pictographs, looking through one emoji variation selector
// on the left (as in a heart followed by VS16, a joiner and a flame).
func zwjJoinsEmoji(text string, i int) bool {
	prev, n := utf8.DecodeLastRuneInString(text[:i])
	if prev == '\ufe0f' {
		prev, _ = utf8.DecodeLastRuneInString(text[:i-n])
	}
	next, _ := utf8.DecodeRuneInString(text[i+utf8.RuneLen(zwj):])
	return isPictograph(prev) && isPictograph(next)
}

// isPictograph is an approximation of Unicode's Extended_Pictographic, which
// the standard library carries no table for: the emoji blocks and the symbol
// ranges emoji sequences are built from.
func isPictograph(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF, r >= 0x2600 && r <= 0x27BF, r >= 0x2300 && r <= 0x23FF,
		r >= 0x2190 && r <= 0x21FF, r >= 0x2B00 && r <= 0x2BFF:
		return true
	}
	switch r {
	case 0xA9, 0xAE, 0x203C, 0x2049, 0x2122, 0x2139, 0x24C2, 0x3030, 0x303D, 0x3297, 0x3299:
		return true
	}
	return r >= 0x25AA && r <= 0x25FE
}

// isInvisibleFormat reports a rune that draws nothing (or reorders or breaks
// what is drawn) yet changes what a reader sees: every Unicode format character
// (Cf: bidi controls, zero-width space, joiners, soft hyphen, BOM, the tag
// characters U+E0000 block) and the line and paragraph separators U+2028 and
// U+2029, which a terminal or a log viewer may treat as a line break. Each is
// shown as a visible escape. A zero-width joiner is marked even between emoji:
// a joiner anywhere else is marked.
//
// The one exception is a zero-width joiner with a pictograph on each side
// (zwjJoinsEmoji), which is how a family or a profession emoji is spelled.
func isInvisibleFormat(r rune) bool {
	return r == '\u2028' || r == '\u2029' || unicode.Is(unicode.Cf, r) || isBidiControl(r)
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
