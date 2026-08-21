package input

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// decode reads one event from the head of buf.
//
// It returns the number of bytes consumed. Zero means the buffer holds the
// start of something whose end has not arrived yet, and the caller must read
// more before asking again — with one exception, handled by the caller: a lone
// escape byte is indistinguishable from the start of an escape sequence, and is
// only resolved by waiting.
//
// more reports whether the caller believes further bytes are already in flight.
// When it is false and the buffer holds an incomplete sequence that has waited
// out its grace period, the caller passes flush to force a decision.
func decode(buf []byte, flush bool) (Event, int) {
	if len(buf) == 0 {
		return nil, 0
	}

	if buf[0] == 0x1b {
		return decodeEscape(buf, flush)
	}
	return decodeByte(buf)
}

// decodeByte handles everything that is not an escape sequence.
func decodeByte(buf []byte) (Event, int) {
	b := buf[0]
	switch {
	case b == 0x0d:
		// With ICRNL cleared, Enter is carriage return. That clearing is what
		// makes Enter and Ctrl+J different keys, which several bindings rely on.
		return Key{Type: KeyEnter}, 1
	case b == 0x09:
		return Key{Type: KeyTab}, 1
	case b == 0x7f, b == 0x08:
		// Terminals disagree about which byte Backspace sends, and the wrong
		// answer deletes nothing while the user holds the key down.
		return Key{Type: KeyBackspace}, 1
	case b == 0x00:
		return Key{Type: KeySpace, Mod: ModCtrl}, 1
	case b == ' ':
		return Key{Type: KeySpace, Runes: []rune{' '}}, 1
	case b < 0x20:
		// Control bytes are Ctrl plus the letter they map to: 0x01 is Ctrl+A.
		// 0x0a is Ctrl+J rather than Enter, which only holds because ICRNL is
		// off; with it on, the two would be the same byte.
		return Key{Type: KeyRunes, Runes: []rune{rune(b - 1 + 'a')}, Mod: ModCtrl}, 1
	case b < 0x80:
		return Key{Type: KeyRunes, Runes: []rune{rune(b)}}, 1
	}

	// Multi-byte UTF-8. An incomplete sequence at the end of the buffer asks
	// for more bytes rather than decoding to the replacement character: a
	// pasted or fast-typed multi-byte character can straddle two reads, and
	// answering early corrupts it permanently.
	r, size := utf8.DecodeRune(buf)
	if r == utf8.RuneError && size <= 1 {
		if !utf8.FullRune(buf) {
			return nil, 0
		}
		return Key{Type: KeyRunes, Runes: []rune{utf8.RuneError}}, 1
	}
	return Key{Type: KeyRunes, Runes: []rune{r}}, size
}

// decodeEscape handles anything beginning with ESC.
func decodeEscape(buf []byte, flush bool) (Event, int) {
	if len(buf) == 1 {
		if flush {
			return Key{Type: KeyEscape}, 1
		}
		return nil, 0
	}

	switch buf[1] {
	case '[':
		return decodeCSI(buf, flush)
	case 'O':
		return decodeSS3(buf, flush)
	case 0x1b:
		// ESC ESC. The first is a real Escape; the second starts whatever
		// follows. Emitting one and leaving the other is what makes the
		// double-Escape binding work when the two presses arrive in one read.
		return Key{Type: KeyEscape}, 1
	}

	// ESC followed by anything else is Alt. This is genuinely ambiguous — it is
	// also what a user pressing Escape and then a letter produces — and every
	// terminal application resolves it the same way, by timing, which the
	// caller has already applied before passing flush.
	ev, n := decodeByte(buf[1:])
	if n == 0 {
		return nil, 0
	}
	key, ok := ev.(Key)
	if !ok {
		return ev, n + 1
	}
	key.Mod |= ModAlt
	return key, n + 1
}

// pasteStart and pasteEnd bracket a paste.
const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// decodeCSI handles ESC [ … sequences.
func decodeCSI(buf []byte, flush bool) (Event, int) {
	if strings.HasPrefix(string(buf), pasteStart) {
		return decodePaste(buf, flush)
	}

	// A CSI runs: parameter bytes 0x30-0x3f, intermediate bytes 0x20-0x2f, then
	// one final byte 0x40-0x7e.
	end := -1
	for i := 2; i < len(buf); i++ {
		c := buf[i]
		if c >= 0x40 && c <= 0x7e {
			end = i
			break
		}
		if c < 0x20 || c > 0x3f {
			// Not a valid CSI body. Rather than consume an unbounded run of
			// garbage, treat the ESC as a lone Escape and let the rest decode
			// on its own terms.
			return Key{Type: KeyEscape}, 1
		}
	}
	if end < 0 {
		if flush {
			return Key{Type: KeyEscape}, 1
		}
		return nil, 0
	}

	body := string(buf[2:end])
	final := buf[end]
	consumed := end + 1

	// SGR mouse reporting: ESC [ < b ; x ; y M|m
	if strings.HasPrefix(body, "<") && (final == 'M' || final == 'm') {
		if ev, ok := decodeMouse(body[1:], final == 'M'); ok {
			return ev, consumed
		}
		return nil, consumed
	}

	switch final {
	case 'I':
		return Focus{In: true}, consumed
	case 'O':
		return Focus{In: false}, consumed
	case 'Z':
		return Key{Type: KeyBackTab, Mod: ModShift}, consumed
	}

	params := parseParams(body)
	mod := modFromParam(params, 1)

	switch final {
	case 'A':
		return Key{Type: KeyUp, Mod: mod}, consumed
	case 'B':
		return Key{Type: KeyDown, Mod: mod}, consumed
	case 'C':
		return Key{Type: KeyRight, Mod: mod}, consumed
	case 'D':
		return Key{Type: KeyLeft, Mod: mod}, consumed
	case 'H':
		return Key{Type: KeyHome, Mod: mod}, consumed
	case 'F':
		return Key{Type: KeyEnd, Mod: mod}, consumed
	case 'P':
		return Key{Type: KeyF1, Mod: mod}, consumed
	case 'Q':
		return Key{Type: KeyF2, Mod: mod}, consumed
	case 'R':
		return Key{Type: KeyF3, Mod: mod}, consumed
	case 'S':
		return Key{Type: KeyF4, Mod: mod}, consumed
	case 'u':
		// The kitty keyboard protocol: ESC [ codepoint ; modifiers u. Terminals
		// that negotiate it send Ctrl+letter here instead of as a control byte,
		// and an application that ignores it silently loses those bindings.
		// The codepoint is a number a terminal sent, so it is not necessarily
		// one Unicode has. utf8.ValidRune rejects the three ways it can fail:
		// past U+10FFFF, negative, and the surrogate range. Any of those would
		// become a rune that cannot be encoded, get written into the prompt
		// buffer, and reach the model as a replacement character with nothing
		// recording that a key was misread. The sequence is still consumed, so
		// a terminal sending one does not stall the reader.
		if len(params) > 0 && params[0] > 0 && utf8.ValidRune(rune(params[0])) {
			return kittyKey(rune(params[0]), mod), consumed
		}
		return nil, consumed
	case '~':
		if len(params) == 0 {
			return nil, consumed
		}
		return tildeKey(params[0], mod), consumed
	}
	// A CSI the decoder does not know. Consumed rather than surfaced: a device
	// status report or a capability response is not a keystroke, and passing it
	// through would put its bytes into the user's prompt.
	return nil, consumed
}

// tildeKey maps the numbered ESC [ n ~ family.
func tildeKey(n int, mod Mod) Event {
	switch n {
	case 1, 7:
		return Key{Type: KeyHome, Mod: mod}
	case 2:
		return Key{Type: KeyInsert, Mod: mod}
	case 3:
		return Key{Type: KeyDelete, Mod: mod}
	case 4, 8:
		return Key{Type: KeyEnd, Mod: mod}
	case 5:
		return Key{Type: KeyPageUp, Mod: mod}
	case 6:
		return Key{Type: KeyPageDown, Mod: mod}
	case 11:
		return Key{Type: KeyF1, Mod: mod}
	case 12:
		return Key{Type: KeyF2, Mod: mod}
	case 13:
		return Key{Type: KeyF3, Mod: mod}
	case 14:
		return Key{Type: KeyF4, Mod: mod}
	case 15:
		return Key{Type: KeyF5, Mod: mod}
	case 17:
		return Key{Type: KeyF6, Mod: mod}
	case 18:
		return Key{Type: KeyF7, Mod: mod}
	case 19:
		return Key{Type: KeyF8, Mod: mod}
	case 20:
		return Key{Type: KeyF9, Mod: mod}
	case 21:
		return Key{Type: KeyF10, Mod: mod}
	case 23:
		return Key{Type: KeyF11, Mod: mod}
	case 24:
		return Key{Type: KeyF12, Mod: mod}
	}
	return nil
}

// kittyKey maps a kitty-protocol codepoint back onto the key vocabulary.
func kittyKey(r rune, mod Mod) Event {
	switch r {
	case 13:
		return Key{Type: KeyEnter, Mod: mod}
	case 9:
		if mod&ModShift != 0 {
			return Key{Type: KeyBackTab, Mod: mod}
		}
		return Key{Type: KeyTab, Mod: mod}
	case 27:
		return Key{Type: KeyEscape, Mod: mod}
	case 127:
		return Key{Type: KeyBackspace, Mod: mod}
	case ' ':
		return Key{Type: KeySpace, Runes: []rune{' '}, Mod: mod}
	}
	return Key{Type: KeyRunes, Runes: []rune{r}, Mod: mod}
}

// decodeSS3 handles ESC O …, the application-cursor-mode encoding. Terminals
// switch into it without asking, so an application that only decodes CSI loses
// its arrow keys the moment something enables it.
func decodeSS3(buf []byte, flush bool) (Event, int) {
	if len(buf) < 3 {
		if flush {
			return Key{Type: KeyEscape}, 1
		}
		return nil, 0
	}
	switch buf[2] {
	case 'A':
		return Key{Type: KeyUp}, 3
	case 'B':
		return Key{Type: KeyDown}, 3
	case 'C':
		return Key{Type: KeyRight}, 3
	case 'D':
		return Key{Type: KeyLeft}, 3
	case 'H':
		return Key{Type: KeyHome}, 3
	case 'F':
		return Key{Type: KeyEnd}, 3
	case 'P':
		return Key{Type: KeyF1}, 3
	case 'Q':
		return Key{Type: KeyF2}, 3
	case 'R':
		return Key{Type: KeyF3}, 3
	case 'S':
		return Key{Type: KeyF4}, 3
	}
	return nil, 3
}

// decodePaste collects everything up to the closing bracket.
func decodePaste(buf []byte, flush bool) (Event, int) {
	rest := buf[len(pasteStart):]
	idx := strings.Index(string(rest), pasteEnd)
	if idx < 0 {
		if flush {
			// The terminal opened a paste and never closed it. Delivering what
			// arrived is better than discarding it, and better than waiting
			// forever on a close that is not coming.
			return Paste{Text: sanitizePaste(string(rest))}, len(buf)
		}
		return nil, 0
	}
	text := string(rest[:idx])
	return Paste{Text: sanitizePaste(text)}, len(pasteStart) + idx + len(pasteEnd)
}

// sanitizePaste strips control characters from pasted content, keeping the
// whitespace a multi-line paste needs.
//
// A paste is the most direct route untrusted bytes have into this process: the
// user copies from a web page, a log, or a model's own output. Escape sequences
// inside it must never reach the terminal, and a nested paste-end marker must
// never be able to close the bracket early.
func sanitizePaste(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == '\r':
			// Normalised to a newline: a pasted CRLF document would otherwise
			// carry carriage returns into the prompt buffer, where they move
			// the cursor rather than break the line.
			b.WriteRune('\n')
		case r < 0x20 || r == 0x7f:
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// decodeMouse parses the SGR mouse body: button ; column ; row.
func decodeMouse(body string, press bool) (Event, bool) {
	parts := strings.Split(body, ";")
	if len(parts) != 3 {
		return nil, false
	}
	code, err1 := strconv.Atoi(parts[0])
	x, err2 := strconv.Atoi(parts[1])
	y, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, false
	}

	var mod Mod
	if code&4 != 0 {
		mod |= ModShift
	}
	if code&8 != 0 {
		mod |= ModAlt
	}
	if code&16 != 0 {
		mod |= ModCtrl
	}

	m := Mouse{
		// The wire is 1-based and every rect in this codebase is 0-based.
		// Converting anywhere other than here means two conventions in the same
		// program, which is how a click lands one cell off.
		X: x - 1, Y: y - 1, Mod: mod,
	}

	switch {
	case code&64 != 0:
		// Wheel. It has no press and release; each notch is one event.
		switch code & 3 {
		case 0:
			m.Action = MouseWheelUp
		case 1:
			m.Action = MouseWheelDown
		case 2:
			m.Action = MouseWheelLeft
		case 3:
			m.Action = MouseWheelRight
		}
		return m, true
	case code&32 != 0:
		m.Action = MouseMotion
		m.Button = buttonFor(code & 3)
		return m, true
	}

	m.Button = buttonFor(code & 3)
	if press {
		m.Action = MousePress
	} else {
		m.Action = MouseRelease
	}
	return m, true
}

func buttonFor(low int) MouseButton {
	switch low {
	case 0:
		return MouseLeft
	case 1:
		return MouseMiddle
	case 2:
		return MouseRight
	}
	return MouseNone
}

// parseParams splits a CSI body into its numeric parameters. Empty parameters
// become zero, which is the terminal convention for "default".
func parseParams(body string) []int {
	if body == "" {
		return nil
	}
	// Sub-parameters, separated by colons, are used by the kitty protocol for
	// event types. Only the leading value of each parameter is needed here.
	fields := strings.Split(body, ";")
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		if i := strings.IndexByte(f, ':'); i >= 0 {
			f = f[:i]
		}
		n, err := strconv.Atoi(f)
		if err != nil {
			n = 0
		}
		out = append(out, n)
	}
	return out
}

// modFromParam reads the modifier parameter, which is encoded as a bitmask
// plus one so that "no modifiers" is 1 rather than an empty field.
func modFromParam(params []int, idx int) Mod {
	if len(params) <= idx || params[idx] < 1 {
		return 0
	}
	bits := params[idx] - 1
	var m Mod
	if bits&1 != 0 {
		m |= ModShift
	}
	if bits&2 != 0 {
		m |= ModAlt
	}
	if bits&4 != 0 {
		m |= ModCtrl
	}
	if bits&8 != 0 {
		m |= ModMeta
	}
	return m
}
