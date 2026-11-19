// Package hygiene is the one owner of which characters in third-party text are
// refused their effect before that text reaches a model or a terminal.
//
// Text that a repository, an MCP server or a web page chose is not text this
// program wrote. A control character in it is executed by the terminal that
// echoes it, a bidi override reorders the line around it, and a zero-width
// joiner splits a marker so that a filter looking for the marker misses it.
// Invisible names those runes; Escape renders them visible instead of dropping
// them, so that a reader can see that something was there.
package hygiene

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Invisible reports runes that render as nothing, or rewrite what is around
// them, and so can hide or fake text: control characters other than newline and
// tab (an escape sequence is executed by the terminal that echoes it), format
// characters (zero-width joiners, bidi overrides, which split a marker or
// reorder a line), private-use characters, the Unicode line and paragraph
// separators, and the tag block.
func Invisible(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Co, r) ||
		r == '\u2028' || r == '\u2029' || // line and paragraph separators: a terminal or renderer breaks the line
		(r >= 0xE0000 && r <= 0xE007F) // the tag block: renders as nothing, and can carry a hidden message
}

// Escape renders every Invisible rune in s, and every byte that is not valid
// UTF-8, as a visible \xNN or \uNNNN escape, and leaves everything else alone.
// A CRLF line ending is read as the newline it stands for, so text from a
// Windows program is not filled with escaped carriage returns; a lone carriage
// return, which a terminal obeys by overwriting the line, is escaped.
//
// Backslashes are left as they are, so Escape is idempotent: text that has been
// through it once comes back unchanged and nothing is escaped twice.
func Escape(s string) string {
	clean := !strings.Contains(s, "\r")
	if clean {
		for i, r := range s {
			if Invisible(r) || (r == utf8.RuneError && !utf8.ValidString(s[i:i+1])) {
				clean = false
				break
			}
		}
	}
	if clean {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, w := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && w == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case Invisible(r) && r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case Invisible(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+w])
		}
		i += w
	}
	return b.String()
}

// EscapeLine is Escape for text that has to stay on one line: it also renders
// newline and tab visible, and does not fold CRLF, so the result has no line
// break in it and cannot forge a line of whatever it is printed into.
func EscapeLine(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, w := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\n' || r == '\t' || r == '\r':
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteString(Escape(s[i : i+w]))
		}
		i += w
	}
	return b.String()
}
