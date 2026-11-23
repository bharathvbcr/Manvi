package render

import (
	"bytes"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// advScreen is a minimal terminal: it places what the painter writes by the
// widths a terminal uses, so a column the painter miscounts shows up as text in
// the wrong cell.
type advScreen struct {
	cells []string
	col   int
	last  int
	moves int
	esc   int
	stray int
}

func newAdvScreen(w int) *advScreen {
	s := &advScreen{cells: make([]string, w), last: -1}
	for i := range s.cells {
		s.cells[i] = " "
	}
	return s
}

func (s *advScreen) feed(out string) {
	rs := []rune(out)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if r == 0x1b {
			s.esc++
			if i+1 >= len(rs) || rs[i+1] != '[' {
				s.stray++
				continue
			}
			j := i + 2
			for j < len(rs) && (rs[j] < 0x40 || rs[j] > 0x7e) {
				j++
			}
			if j >= len(rs) {
				s.stray++
				return
			}
			params := string(rs[i+2 : j])
			switch rs[j] {
			case 'H':
				s.moves++
				if k := strings.IndexByte(params, ';'); k >= 0 {
					c, _ := strconv.Atoi(params[k+1:])
					s.col = c - 1
				}
			case 'K':
				for c := s.col; c < len(s.cells); c++ {
					s.cells[c] = " "
				}
			case 'J':
				for c := range s.cells {
					s.cells[c] = " "
				}
			}
			i = j
			continue
		}
		w := RuneWidth(r)
		if w == 0 {
			if s.last >= 0 {
				s.cells[s.last] += string(r)
			}
			continue
		}
		if s.col < 0 || s.col+w > len(s.cells) {
			s.stray++
			s.col += w
			continue
		}
		s.cells[s.col] = string(r)
		if w == 2 {
			s.cells[s.col+1] = ""
		}
		s.last = s.col
		s.col += w
	}
}

func advFlush(t *testing.T, p *Painter) string {
	t.Helper()
	var out bytes.Buffer
	if err := p.Flush(&out, Cursor{}); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return out.String()
}

func advCfSeq(s string) []rune {
	var out []rune
	for _, r := range s {
		if unicode.Is(unicode.Cf, r) {
			out = append(out, r)
		}
	}
	return out
}

// Probe 1: the Cf runes StripFormatRunes keeps are the Cf runes SetString puts
// on the tty, for text mixing every context the rule distinguishes.
func TestAdvB30StripFormatRunesMatchesPaintedFormatRunes(t *testing.T) {
	alphabet := []rune{
		0x202A, 0x202B, 0x202C, 0x202D, 0x202E, 0x2066, 0x2067, 0x2068, 0x2069, 0x200E, 0x200F, 0x061C,
		0x200B, 0x200C, 0x200D, 0x2060, 0xFEFF, 0x00AD, 0x0600, 0x0601, 0x0605, 0x070F,
		0x1F600, 0x2764, 0x1F468, 0x1F469, 0x1F3F4, 0x2190, 0x2603,
		'1', '7', ' ', 'a', 'Z', '.', 0x06CC, 0x062E, 0x0915, 0x094D,
		0x1F1FA, 0x1F1F8,
		0xE0020, 0xE0041, 0xE0067, 0xE0062, 0xE0065, 0xE006E, 0xE007E, 0xE007F,
		0x0301, 0x0300, 0xFE0F, 0xFE0E, 0xE0100,
		0x6F22, 0x5B57, 0xFF21, '\x07', '\n',
	}
	marks := []rune{0x0301, 0xFE0F, 0x200D, 0x200C, 0x202E, 0xE0067, 0xE007F, 0x0600}
	bases := []rune{'a', 0x1F468, 0x1F3F4, 0x6F22, '1', ' ', 0x06CC}
	rng := rand.New(rand.NewSource(30))
	const cases = 6000
	for n := 0; n < cases; n++ {
		var sb strings.Builder
		if n%5 == 0 {
			// a long run on one base, to cross MaxCombPerCell
			sb.WriteRune(bases[rng.Intn(len(bases))])
			for k, m := 0, 25+rng.Intn(20); k < m; k++ {
				sb.WriteRune(marks[rng.Intn(len(marks))])
			}
		}
		for k, m := 0, 1+rng.Intn(24); k < m; k++ {
			sb.WriteRune(alphabet[rng.Intn(len(alphabet))])
		}
		text := sb.String()
		b := NewBuffer(400, 1)
		b.Clear(Plain)
		p := NewPainter(400, 1, NoColor)
		p.Buffer().SetString(0, 0, text, Plain)
		got := advCfSeq(advFlush(t, p))
		want := advCfSeq(StripFormatRunes(text))
		if string(got) != string(want) {
			t.Fatalf("case %d: input %+q\n StripFormatRunes keeps Cf %U\n SetString paints Cf    %U", n, text, want, got)
		}
	}
}

func advBaseline(t *testing.T) (*advScreen, string) {
	p := NewPainter(12, 1, NoColor)
	p.Buffer().SetString(0, 0, "axyz", Plain)
	out := advFlush(t, p)
	s := newAdvScreen(12)
	s.feed(out)
	return s, out
}

// Probe 3: a hand-built Cell cannot carry what SetRune refuses, and does not
// move the columns after it.
func TestAdvB30SetCellHostileCells(t *testing.T) {
	wideCont := Cell{R: 0}
	cases := []struct {
		name  string
		cells []Cell // placed from column 0
		want0 string // expected content of column 0
		xAt   int    // column where "xyz" is set
	}{
		{"rlo base", []Cell{{R: 0x202E}}, " ", 1},
		{"esc base", []Cell{{R: 0x1b}}, " ", 1},
		{"combining mark as base", []Cell{{R: 0x0301}}, "", 1},
		{"printable wide in comb", []Cell{{R: 'a', Comb: "\u6f22"}}, "a", 1},
		{"comb over cap", []Cell{{R: 'a', Comb: strings.Repeat("\u0301", 100)}}, "a" + strings.Repeat("\u0301", MaxCombPerCell), 1},
		{"invalid utf8 comb", []Cell{{R: 'a', Comb: "\xff\xfe\u0301\x80"}}, "a\u0301", 1},
		{"bidi and esc in comb", []Cell{{R: 'a', Comb: "\u202e\x1b[31m\u0301"}}, "a\u0301", 1},
		{"wide base with comb", []Cell{{R: 0x6F22, Comb: "\u0301"}, wideCont}, "\u6f22\u0301", 2},
		{"wide base with wide comb", []Cell{{R: 0x6F22, Comb: "\u5b57\u0301"}, wideCont}, "\u6f22\u0301", 2},
	}
	baseScreen, baseOut := advBaseline(t)
	_ = baseScreen
	for _, tc := range cases {
		p := NewPainter(12, 1, NoColor)
		for i, c := range tc.cells {
			p.Buffer().SetCell(i, 0, c)
		}
		p.Buffer().SetString(tc.xAt, 0, "xyz", Plain)
		out := advFlush(t, p)
		if !utf8.ValidString(out) {
			t.Errorf("%s: painted bytes are not valid UTF-8: %q", tc.name, out)
		}
		for _, r := range out {
			if r == 0x202E || (unicode.Is(unicode.Cf, r) && r != 0x200D) {
				t.Errorf("%s: format rune U+%04X on the tty: %q", tc.name, r, out)
			}
		}
		if strings.Contains(out, "31m") || strings.Contains(out, "\ufffd") {
			t.Errorf("%s: a hostile Comb fragment reached the tty: %q", tc.name, out)
		}
		if got, want := strings.Count(out, "\x1b"), strings.Count(baseOut, "\x1b"); got != want {
			t.Errorf("%s: %d ESC bytes painted, a plain cell paints %d: %q", tc.name, got, want, out)
		}
		s := newAdvScreen(12)
		s.feed(out)
		if s.stray != 0 {
			t.Errorf("%s: %d stray or out-of-row writes: %q", tc.name, s.stray, out)
		}
		if tc.want0 != "" && s.cells[0] != tc.want0 {
			t.Errorf("%s: column 0 shows %q, want %q (out %q)", tc.name, s.cells[0], tc.want0, out)
		}
		for i, ch := range []string{"x", "y", "z"} {
			if got := s.cells[tc.xAt+i]; got != ch {
				t.Errorf("%s: column %d shows %q, want %q: the painter's column count drifted (out %q)", tc.name, tc.xAt+i, got, ch, out)
				break
			}
		}
	}
}

// SetCell with a wide base in the last column is the half glyph SetRune
// refuses; the cell must not reach the tty as a wide rune that overflows.
func TestAdvB30SetCellWideBaseInLastColumn(t *testing.T) {
	p := NewPainter(4, 1, NoColor)
	p.Buffer().SetCell(3, 0, Cell{R: 0x6F22})
	out := advFlush(t, p)
	if strings.ContainsRune(out, 0x6F22) {
		t.Errorf("a wide rune in the last column reached the tty and overflows the row: %q", out)
	}
}

// Probe 4a: a mark whose left neighbour is a continuation and whose base
// column falls outside the row or was never written.
func TestAdvB30WideBaseAttachBounds(t *testing.T) {
	// wide rune at column 0, mark at column 2, buffer 3 wide: attaches to the base.
	b := NewBuffer(3, 1)
	b.Clear(Plain)
	b.SetString(0, 0, "\u6f22", Plain)
	if n := b.SetRune(2, 0, 0x0301, Plain); n != 0 {
		t.Errorf("mark consumed %d columns", n)
	}
	if got := b.Cell(0, 0).Comb; got != "\u0301" {
		t.Errorf("mark at x=2 over a wide base at 0 (W=3): base Comb %q, want U+0301", got)
	}
	if b.Cell(1, 0).Comb != "" || b.Cell(2, 0).Comb != "" {
		t.Errorf("mark leaked onto the continuation or the next cell: %q %q", b.Cell(1, 0).Comb, b.Cell(2, 0).Comb)
	}
	// buffer 2 wide: column 2 is out of bounds.
	b2 := NewBuffer(2, 1)
	b2.Clear(Plain)
	b2.SetString(0, 0, "\u6f22\u0301", Plain)
	if got := b2.Cell(0, 0).Comb; got != "" {
		t.Errorf("W=2: a mark past the right edge attached: %q", got)
	}
	if n := b2.SetRune(2, 0, 0x0301, Plain); n != 0 || b2.Cell(0, 0).Comb != "" {
		t.Errorf("W=2: SetRune(2) = %d, Comb %q", n, b2.Cell(0, 0).Comb)
	}
	// a buffer whose cells were never written (R == 0 everywhere): nothing to attach to, no panic, no mark kept.
	for _, x := range []int{0, 1, 2, 3} {
		raw := NewBuffer(4, 1)
		for c := range raw.cells {
			raw.cells[c] = Cell{}
		}
		raw.SetRune(x, 0, 0x0301, Plain)
		raw.SetRune(x, 0, 0x200D, Plain)
		for c := 0; c < 4; c++ {
			if raw.Cell(c, 0).Comb != "" {
				t.Errorf("unwritten buffer, mark at x=%d: cell %d kept Comb %q", x, c, raw.Cell(c, 0).Comb)
			}
		}
	}
	// a mark at x=1 whose left cell is a continuation at column 0 (x-2 < 0).
	c := NewBuffer(3, 1)
	c.Clear(Plain)
	c.cells[0] = Cell{R: 0}
	c.SetRune(1, 0, 0x0301, Plain)
	if c.Cell(0, 0).Comb != "" || c.Cell(1, 0).Comb != "" {
		t.Errorf("mark after a column-0 continuation was kept: %q %q", c.Cell(0, 0).Comb, c.Cell(1, 0).Comb)
	}
}

// Probe 4b: a wide rune at the right edge becomes a space, and nothing attaches to it.
func TestAdvB30WideAtRightEdgeIsASpace(t *testing.T) {
	for _, w := range []int{1, 2, 3, 4} {
		b := NewBuffer(w, 1)
		b.Clear(Plain)
		text := strings.Repeat("a", w-1) + "\u6f22\u0301\u200d\ufe0f"
		b.SetString(0, 0, text, Plain)
		if w > 1 {
			// SetString stops at the edge, so also drive SetRune past it.
			b.SetRune(w, 0, 0x0301, Plain)
		}
		if c := b.Cell(w-1, 0); c.R != ' ' || c.Comb != "" {
			t.Errorf("W=%d: edge cell = %+v, want a bare space", w, c)
		}
		p := NewPainter(w, 1, NoColor)
		p.Buffer().SetString(0, 0, text, Plain)
		if out := advFlush(t, p); strings.ContainsRune(out, 0x6F22) || strings.ContainsRune(out, 0x0301) {
			t.Errorf("W=%d: the wide rune or its mark reached the tty: %q", w, out)
		}
	}
}

// Probe 4c: the per-cell bound holds on a wide base, in the buffer and on screen.
func TestAdvB30MaxCombOnAWideBase(t *testing.T) {
	text := "\u6f22" + strings.Repeat("\u0301", 80) + "x"
	b := NewBuffer(20, 1)
	b.Clear(Plain)
	if n := b.SetString(0, 0, text, Plain); n != 3 {
		t.Errorf("SetString consumed %d columns, want 3", n)
	}
	if got := utf8.RuneCountInString(b.Cell(0, 0).Comb); got != MaxCombPerCell {
		t.Errorf("wide base holds %d marks, want %d", got, MaxCombPerCell)
	}
	p := NewPainter(20, 1, NoColor)
	p.Buffer().SetString(0, 0, text, Plain)
	s := newAdvScreen(20)
	s.feed(advFlush(t, p))
	if s.cells[0] != "\u6f22"+strings.Repeat("\u0301", MaxCombPerCell) || s.cells[2] != "x" {
		t.Errorf("screen: col0 %q (%d runes) col2 %q", s.cells[0], utf8.RuneCountInString(s.cells[0]), s.cells[2])
	}
}

// Probe 4d: a mark added to an already painted wide base in a later frame is
// repainted, and the cursor does not drift for what follows it.
func TestAdvB30MarkAddedInALaterFrameRepaintsTheBase(t *testing.T) {
	frames := []struct {
		name         string
		first, later string
	}{
		{"cjk accent", "ab\u6f22cd", "ab\u6f22\u0301cd"},
		{"cjk at start", "\u6f22x", "\u6f22\u0301x"},
		{"family", "\U0001f468\U0001f469x", "\U0001f468\u200d\U0001f469x"},
		{"flag", "\U0001f3f4x", "\U0001f3f4\U000e0067\U000e0062\U000e007fx"},
		{"cap on later frame", "\u6f22x", "\u6f22" + strings.Repeat("\u0301", 40) + "x"},
	}
	for _, f := range frames {
		p := NewPainter(20, 1, NoColor)
		s := newAdvScreen(20)
		p.Buffer().SetString(0, 0, f.first, Plain)
		s.feed(advFlush(t, p))
		p.Buffer().Clear(Plain)
		p.Buffer().SetString(0, 0, f.later, Plain)
		out := advFlush(t, p)
		s.feed(out)
		p.Buffer().SetString(10, 0, "Q", Plain)
		s.feed(advFlush(t, p))

		fresh := NewPainter(20, 1, NoColor)
		fresh.Buffer().SetString(0, 0, f.later, Plain)
		fresh.Buffer().SetString(10, 0, "Q", Plain)
		want := newAdvScreen(20)
		want.feed(advFlush(t, fresh))
		for c := range want.cells {
			if s.cells[c] != want.cells[c] {
				t.Errorf("%s: after the later frame column %d shows %q, a fresh paint shows %q (frame-2 bytes %q)", f.name, c, s.cells[c], want.cells[c], out)
				break
			}
		}
		if s.stray != 0 {
			t.Errorf("%s: %d stray writes", f.name, s.stray)
		}
		if out == "" {
			t.Errorf("%s: the later frame painted nothing", f.name)
		}
	}
}
