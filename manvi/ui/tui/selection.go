package tui

import (
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// TextSelection tracks a cell-level mouse drag selection inside a Scrollback
// viewport. Coordinates are stored in layout-row space (index into Scrollback's
// flattened rows slice) and viewport-relative columns, so the selection stays
// anchored to the exact text even if the viewport scrolls.
type TextSelection struct {
	Active    bool
	Dragging  bool
	AnchorRow int
	AnchorCol int
	EndRow    int
	EndCol    int
}

// Clear drops any active selection.
func (s *TextSelection) Clear() {
	*s = TextSelection{}
}

// Begin starts a potential drag at the given layout row and column.
func (s *TextSelection) Begin(row, col int) {
	s.Dragging = true
	s.Active = false
	s.AnchorRow, s.AnchorCol = row, col
	s.EndRow, s.EndCol = row, col
}

// Update extends an in-progress drag and reports whether the selection became
// non-empty (i.e. moved off the anchor cell).
func (s *TextSelection) Update(row, col int) bool {
	if !s.Dragging {
		return false
	}
	s.EndRow, s.EndCol = row, col
	if s.EndRow != s.AnchorRow || s.EndCol != s.AnchorCol {
		s.Active = true
	}
	return s.Active
}

// End finishes dragging and reports whether a non-empty selection exists.
func (s *TextSelection) End() bool {
	s.Dragging = false
	return s.Active
}

// Bounds returns ordered (startRow, startCol, endRow, endCol) inclusive.
func (s TextSelection) Bounds() (r0, c0, r1, c1 int) {
	if s.AnchorRow < s.EndRow || (s.AnchorRow == s.EndRow && s.AnchorCol <= s.EndCol) {
		return s.AnchorRow, s.AnchorCol, s.EndRow, s.EndCol
	}
	return s.EndRow, s.EndCol, s.AnchorRow, s.AnchorCol
}

// RowSpan returns the inclusive [startCol, endCol] highlighted on layout row r
// within a line of width lineW, and whether row r intersects the selection.
func (s TextSelection) RowSpan(r, lineW int) (c0, c1 int, ok bool) {
	if !s.Active || lineW <= 0 {
		return 0, 0, false
	}
	r0, sc0, r1, sc1 := s.Bounds()
	if r < r0 || r > r1 {
		return 0, 0, false
	}
	c0 = 0
	c1 = lineW - 1
	if r == r0 {
		c0 = sc0
	}
	if r == r1 {
		c1 = sc1
	}
	if c0 < 0 {
		c0 = 0
	}
	if c1 >= lineW {
		c1 = lineW - 1
	}
	if c0 > c1 {
		return 0, 0, false
	}
	return c0, c1, true
}

// Extract gathers the plain text covered by the selection from layout rows.
// It strips the transcript's leading gutter columns when a full line is
// selected, removes leading code-block vertical rules ("│ "), and trims
// right-hand padding spaces so copied text pastes cleanly.
func (s TextSelection) Extract(rows []rowRef) string {
	if !s.Active || len(rows) == 0 {
		return ""
	}
	r0, c0, r1, c1 := s.Bounds()
	if r0 < 0 {
		r0 = 0
	}
	if r1 >= len(rows) {
		r1 = len(rows) - 1
	}
	if r0 > r1 {
		return ""
	}
	var out []string
	for r := r0; r <= r1; r++ {
		if rows[r].gap {
			out = append(out, "")
			continue
		}
		text := rows[r].line.Text()
		sc, ec := 0, render.StringWidth(text)-1
		if r == r0 {
			sc = c0
		}
		if r == r1 {
			ec = c1
		}
		// Skip the 3-column gutter automatically when dragging from the left margin.
		if sc < gutter && (r > r0 || c0 < gutter) {
			sc = gutter
		}
		seg := sliceColumns(text, sc, ec)
		seg = strings.TrimRight(seg, " ")
		seg = strings.TrimPrefix(seg, "│ ")
		out = append(out, seg)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// sliceColumns returns the substring of s occupying display columns [c0, c1]
// inclusive, respecting wide runes.
func sliceColumns(s string, c0, c1 int) string {
	if c0 > c1 || s == "" {
		return ""
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		w := render.RuneWidth(r)
		if w <= 0 {
			if col > c0 && col <= c1+1 {
				b.WriteRune(r)
			}
			continue
		}
		end := col + w - 1
		if end >= c0 && col <= c1 {
			b.WriteRune(r)
		}
		col += w
		if col > c1 {
			break
		}
	}
	return b.String()
}
