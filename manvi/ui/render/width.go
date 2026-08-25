package render

import "unicode"

// RuneWidth reports how many terminal columns a rune occupies: 0, 1, or 2.
//
// Getting this wrong is not a cosmetic bug. The painter positions text by
// counting columns, so a rune the harness thinks is one column wide and the
// terminal draws as two shifts every cell to its right for the rest of the
// line, and the damage persists until a full repaint. A CJK task title or an
// emoji in a commit message is enough to trigger it.
//
// Three classes:
//
//   - Zero. Combining marks, enclosing marks, and format characters — the
//     accent in "é" when it is written as two runes, the zero-width joiner
//     between emoji. These attach to the preceding cell and consume no column
//     of their own.
//   - Two. East Asian Wide and Fullwidth, and the emoji that terminals render
//     double-width.
//   - One. Everything else.
//
// The zero-width classes come from the standard library's Unicode tables, which
// are versioned with the Go release. The wide table is carried here because the
// standard library does not ship East Asian width data.
func RuneWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 0x20 || (r >= 0x7f && r < 0xa0):
		// Control characters, C0 and C1. Sanitize is what removes these before
		// they reach a buffer, and this answer used to say it "should have" —
		// which was the assumption, not the enforcement. It was false: nothing
		// on the full-screen path called Sanitize, and a zero width here is
		// exactly what routed an ESC into the preceding cell's combining marks,
		// from which the painter reassembled it. Buffer.SetRune now refuses to
		// store a control character at all, so this width is only ever consulted
		// for a rune that is about to be dropped.
		return 0
	case r < 0x300:
		// Latin, Greek, Cyrillic and punctuation: the common case, and worth
		// answering before touching a table.
		return 1
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		return 0
	case inRanges(r, wideRanges[:]):
		return 2
	}
	return 1
}

// StringWidth is the column count of a string.
func StringWidth(s string) int {
	w := 0
	for _, r := range s {
		w += RuneWidth(r)
	}
	return w
}

// TruncateWidth cuts s to at most w columns, appending tail if it had to cut.
//
// The cut is by column rather than by rune or byte, because those are the two
// ways this is usually written and both are wrong for the case that matters: a
// status bar holding a path with CJK characters overflows and wraps, which on a
// full-screen TUI scrolls the whole frame by one line.
func TruncateWidth(s string, w int, tail string) string {
	if w <= 0 {
		return ""
	}
	if StringWidth(s) <= w {
		return s
	}
	budget := w - StringWidth(tail)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, r := range s {
		rw := RuneWidth(r)
		if used+rw > budget {
			return s[:i] + tail
		}
		used += rw
	}
	return s + tail
}

// PadWidth right-pads s with spaces to exactly w columns, truncating if longer.
func PadWidth(s string, w int) string {
	cw := StringWidth(s)
	if cw > w {
		return TruncateWidth(s, w, "")
	}
	return s + spaces(w-cw)
}

const spaceRun = "                                                                "

func spaces(n int) string {
	if n <= 0 {
		return ""
	}
	if n <= len(spaceRun) {
		return spaceRun[:n]
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	return string(b)
}

func inRanges(r rune, ranges [][2]rune) bool {
	lo, hi := 0, len(ranges)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		switch {
		case r < ranges[mid][0]:
			hi = mid - 1
		case r > ranges[mid][1]:
			lo = mid + 1
		default:
			return true
		}
	}
	return false
}

// wideRanges are the East Asian Wide and Fullwidth code points, plus the emoji
// blocks terminals draw double-width. Sorted, and searched by bisection.
//
// The emoji entries are the coarse block ranges rather than the exact
// per-codepoint list from the Unicode data files. The error that leaves is
// confined to unassigned code points inside those blocks, which render as a
// replacement glyph whose width the terminal picks anyway; carrying the exact
// list would be several hundred more entries to re-derive on every Unicode
// release for no case a user can reach.
var wideRanges = [...][2]rune{
	{0x1100, 0x115F}, {0x231A, 0x231B}, {0x2329, 0x232A},
	{0x23E9, 0x23EC}, {0x23F0, 0x23F0}, {0x23F3, 0x23F3},
	{0x25FD, 0x25FE}, {0x2614, 0x2615}, {0x2648, 0x2653},
	{0x267F, 0x267F}, {0x2693, 0x2693}, {0x26A1, 0x26A1},
	{0x26AA, 0x26AB}, {0x26BD, 0x26BE}, {0x26C4, 0x26C5},
	{0x26CE, 0x26CE}, {0x26D4, 0x26D4}, {0x26EA, 0x26EA},
	{0x26F2, 0x26F3}, {0x26F5, 0x26F5}, {0x26FA, 0x26FA},
	{0x26FD, 0x26FD}, {0x2705, 0x2705}, {0x270A, 0x270B},
	{0x2728, 0x2728}, {0x274C, 0x274C}, {0x274E, 0x274E},
	{0x2753, 0x2755}, {0x2757, 0x2757}, {0x2795, 0x2797},
	{0x27B0, 0x27B0}, {0x27BF, 0x27BF}, {0x2B1B, 0x2B1C},
	{0x2B50, 0x2B50}, {0x2B55, 0x2B55},
	{0x2E80, 0x2E99}, {0x2E9B, 0x2EF3}, {0x2F00, 0x2FD5},
	{0x2FF0, 0x2FFB}, {0x3000, 0x303E}, {0x3041, 0x3096},
	{0x3099, 0x30FF}, {0x3105, 0x312F}, {0x3131, 0x318E},
	{0x3190, 0x31E3}, {0x31F0, 0x321E}, {0x3220, 0x3247},
	{0x3250, 0x4DBF}, {0x4E00, 0xA48C}, {0xA490, 0xA4C6},
	{0xA960, 0xA97C}, {0xAC00, 0xD7A3}, {0xF900, 0xFAFF},
	{0xFE10, 0xFE19}, {0xFE30, 0xFE52}, {0xFE54, 0xFE66},
	{0xFE68, 0xFE6B}, {0xFF01, 0xFF60}, {0xFFE0, 0xFFE6},
	{0x16FE0, 0x16FE4}, {0x16FF0, 0x16FF1},
	{0x17000, 0x187F7}, {0x18800, 0x18CD5}, {0x18D00, 0x18D08},
	{0x1B000, 0x1B152}, {0x1B164, 0x1B167}, {0x1B170, 0x1B2FB},
	{0x1F004, 0x1F004}, {0x1F0CF, 0x1F0CF},
	{0x1F18E, 0x1F18E}, {0x1F191, 0x1F19A},
	{0x1F200, 0x1F320}, {0x1F32D, 0x1F335}, {0x1F337, 0x1F37C},
	{0x1F37E, 0x1F393}, {0x1F3A0, 0x1F3CA}, {0x1F3CF, 0x1F3D3},
	{0x1F3E0, 0x1F3F0}, {0x1F3F4, 0x1F3F4}, {0x1F3F8, 0x1F43E},
	{0x1F440, 0x1F440}, {0x1F442, 0x1F4FC}, {0x1F4FF, 0x1F53D},
	{0x1F54B, 0x1F54E}, {0x1F550, 0x1F567}, {0x1F57A, 0x1F57A},
	{0x1F595, 0x1F596}, {0x1F5A4, 0x1F5A4}, {0x1F5FB, 0x1F64F},
	{0x1F680, 0x1F6C5}, {0x1F6CC, 0x1F6CC}, {0x1F6D0, 0x1F6D2},
	{0x1F6D5, 0x1F6D7}, {0x1F6EB, 0x1F6EC}, {0x1F6F4, 0x1F6FC},
	{0x1F7E0, 0x1F7EB}, {0x1F90C, 0x1F93A}, {0x1F93C, 0x1F945},
	{0x1F947, 0x1F978}, {0x1F97A, 0x1F9CB}, {0x1F9CD, 0x1F9FF},
	{0x1FA70, 0x1FA74}, {0x1FA78, 0x1FA7A}, {0x1FA80, 0x1FA86},
	{0x1FA90, 0x1FAA8}, {0x1FAB0, 0x1FAB6}, {0x1FAC0, 0x1FAC2},
	{0x1FAD0, 0x1FAD6},
	{0x20000, 0x2FFFD}, {0x30000, 0x3FFFD},
}
