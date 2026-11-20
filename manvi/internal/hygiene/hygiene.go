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
	"encoding/json/jsontext"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
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
		switch r {
		case '\n', '\t', '\r':
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteString(Escape(s[i : i+w]))
		}
		i += w
	}
	return b.String()
}

// EscapeResult is Escape for a third party's reply, which is sometimes plain
// text and sometimes a JSON document: a reply that parses as JSON goes through
// EscapeJSON, so what comes back still parses and no parser can restore a hidden
// rune from it, and anything else through Escape. "Parses as JSON" is as lenient
// as encoding/json (v1) is about member names, which is what a consumer may be
// using: a duplicate member name does not make a document text, because v1 still
// decodes the escapes in it. A document with invalid UTF-8 in it is text.
func EscapeResult(s string) string {
	if jsontext.Value(s).IsValid(jsontext.AllowDuplicateNames(true)) {
		return EscapeJSON(s)
	}
	return Escape(s)
}

// EscapeJSON is Escape for text that is a JSON document, which the caller has
// checked is valid. Escape cannot be used on one: its \xNN is not a JSON escape,
// so a soft hyphen or a C1 control in a string value turned a document that
// parsed into one that did not, and its \uNNNN, which JSON does read, decodes
// back to the very rune it was written to hide, so a parser that read the escaped
// document received the bidi override again.
//
// Here every Invisible rune inside a string value, whether it is written raw or
// as a \uNNNN escape (json.Marshal writes the control characters and U+2028 and
// U+2029 that way), becomes an escaped backslash and the rune's own \uNNNN
// spelling: still one valid JSON string, and one that decodes to the visible text
// \u202e instead of to U+202E. Backslashes that are already escaped are left
// alone, so the result is unchanged by a second pass. A carriage return between
// tokens is legal JSON whitespace but a terminal obeys it by overwriting the line,
// so it is written as the newline it stands for (CRLF as one). Everything else
// outside strings, and every string that holds nothing Invisible, is copied as it
// was.
func EscapeJSON(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inString := false
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case !inString && c == '\r':
			b.WriteByte('\n')
			i++
			if i < len(s) && s[i] == '\n' {
				i++
			}
		case !inString || c == '"':
			if c == '"' {
				inString = !inString
			}
			b.WriteByte(c)
			i++
		case c == '\\':
			r, n := jsonEscape(s[i:])
			if Invisible(r) {
				fmt.Fprintf(&b, "\\\\u%04x", r)
			} else {
				b.WriteString(s[i : i+n])
			}
			i += n
		default:
			r, w := utf8.DecodeRuneInString(s[i:])
			switch {
			case r == utf8.RuneError && w == 1:
				fmt.Fprintf(&b, "\\\\x%02x", c)
			case Invisible(r):
				fmt.Fprintf(&b, "\\\\u%04x", r)
			default:
				b.WriteString(s[i : i+w])
			}
			i += w
		}
	}
	return b.String()
}

// jsonEscape reads the escape sequence that starts s, which begins with a
// backslash inside a JSON string. It returns the rune a \uNNNN escape (or a
// surrogate pair of them) stands for and the length of the sequence, or
// utf8.RuneError, which is not Invisible, and the length of any other escape.
func jsonEscape(s string) (rune, int) {
	if len(s) < 2 {
		return utf8.RuneError, len(s)
	}
	if s[1] != 'u' {
		// The short escapes that name a control character (\b, \f, \r) are
		// Invisible like their \u00NN spellings; the others are not.
		switch s[1] {
		case 'b':
			return '\b', 2
		case 'f':
			return '\f', 2
		case 'r':
			return '\r', 2
		}
		return utf8.RuneError, 2
	}
	unit, ok := hexUnit(s[2:])
	if !ok {
		return utf8.RuneError, 2
	}
	if !utf16.IsSurrogate(unit) {
		return unit, 6
	}
	if len(s) >= 12 && s[6] == '\\' && s[7] == 'u' {
		if low, ok := hexUnit(s[8:]); ok {
			if r := utf16.DecodeRune(unit, low); r != utf8.RuneError {
				return r, 12
			}
		}
	}
	return utf8.RuneError, 6
}

// hexUnit reads the four hex digits that start s as one UTF-16 code unit.
func hexUnit(s string) (rune, bool) {
	if len(s) < 4 {
		return 0, false
	}
	n, err := strconv.ParseUint(s[:4], 16, 16)
	return rune(n), err == nil
}
