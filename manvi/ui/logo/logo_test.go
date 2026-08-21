package logo

import (
	"regexp"
	"strconv"
	"testing"

	"manvi/ui/render"
)

func testColors() Colors {
	return Colors{
		Tile:  render.Style{Fg: render.Hex("#7a1626")},
		Glyph: render.Style{Fg: render.Hex("#fdf2f3")},
		Word:  render.Style{Fg: render.Hex("#c9384a")},
		Tag:   render.Style{Fg: render.Hex("#6e7681")},
	}
}

// The grid is the source both renderers read, so its shape is asserted before
// anything that draws it.
func TestMarkGridIsWellFormed(t *testing.T) {
	rows := append([]string(nil), mark...)
	if len(rows) != markRows {
		t.Fatalf("mark has %d rows, want %d", len(rows), markRows)
	}
	for y, row := range rows {
		if len(row) != markCols {
			t.Fatalf("row %d is %d modules wide, want %d", y, len(row), markCols)
		}
		for x, m := range row {
			if m != '.' && m != '#' {
				t.Fatalf("row %d module %d is %q; the grid is field or knockout, nothing else", y, x, m)
			}
		}
		// The letter is symmetric, and an asymmetric M is the kind of edit that
		// looks fine in a diff and wrong on screen.
		for x := range markCols {
			if row[x] != row[markCols-1-x] {
				t.Fatalf("row %d is not left-right symmetric: %q", y, row)
			}
		}
	}
	// A knockout that touched the edge would stop reading as a stamped tile.
	for x := range markCols {
		if rows[0][x] != '.' || rows[markRows-1][x] != '.' {
			t.Fatalf("the knockout reaches the tile's top or bottom edge")
		}
	}
}

// Mark hands out a copy. A caller that mutated the package's own grid would
// change every subsequent render, including the SVG.
func TestMarkIsACopy(t *testing.T) {
	rows := append([]string(nil), mark...)
	rows[0] = "#######"
	if append([]string(nil), mark...)[0] != "......." {
		t.Fatal("Mark returned the package's own slice")
	}
}

func TestFitLadder(t *testing.T) {
	cases := []struct {
		name    string
		w, h    int
		unicode bool
		want    Size
	}{
		{"wide terminal", 80, 24, true, SizeFull},
		{"exactly full", SizeFull.Width(), markRows, true, SizeFull},
		{"one column short of full", SizeFull.Width() - 1, markRows, true, SizeMedium},
		{"one row short of full", SizeFull.Width(), markRows - 1, true, SizeCompact},
		{"exactly medium", SizeMedium.Width(), markRows, true, SizeMedium},
		{"one column short of medium", SizeMedium.Width() - 1, markRows, true, SizeCompact},
		{"exactly compact", SizeCompact.Width(), 1, true, SizeCompact},
		{"one column short of compact", SizeCompact.Width() - 1, 1, true, SizeText},
		{"name only", len(Name), 1, true, SizeText},
		{"nothing fits", len(Name) - 1, 1, true, SizeNone},
		{"no blocks, wide", 200, 50, false, SizeText},
		{"no blocks, narrow", 2, 1, false, SizeNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Fit(c.w, c.h, c.unicode); got != c.want {
				t.Fatalf("Fit(%d, %d, %v) = %d, want %d", c.w, c.h, c.unicode, got, c.want)
			}
		})
	}
}

// The property that matters more than any of the sizes: whatever rung is
// chosen, nothing is drawn outside the rectangle the caller gave. A splash that
// overruns its pane corrupts the frame around it, and the frame is where the
// weakened-settings band and the status bar live.
func TestDrawStaysInsideItsRect(t *testing.T) {
	const sentinel = '~'
	for w := 1; w <= 90; w++ {
		for _, h := range []int{1, 2, 5, 7, 9, 24} {
			for _, unicode := range []bool{true, false} {
				b := render.NewBuffer(w+8, h+6)
				b.Fill(b.Bounds(), sentinel, render.Style{})
				r := render.Rect{X: 3, Y: 2, W: w, H: h}

				used := Draw(b, r, testColors(), unicode, "the DevCouncil execution harness")
				if used > h {
					t.Fatalf("w=%d h=%d unicode=%v: drew %d rows into %d", w, h, unicode, used, h)
				}
				for y := range b.H {
					for x := range b.W {
						if r.Contains(x, y) {
							continue
						}
						if got := b.Cell(x, y).R; got != sentinel {
							t.Fatalf("w=%d h=%d unicode=%v: wrote %q outside the rect at (%d,%d)",
								w, h, unicode, got, x, y)
						}
					}
				}
			}
		}
	}
}

// An empty or degenerate rect is a no-op rather than a panic: the TUI resizes
// through zero-width panes while a split is being dragged.
func TestDrawHandlesDegenerateRects(t *testing.T) {
	b := render.NewBuffer(20, 10)
	for _, r := range []render.Rect{{}, {X: 1, Y: 1, W: 0, H: 5}, {X: 1, Y: 1, W: 5, H: 0}} {
		if used := Draw(b, r, testColors(), true, "tag"); used != 0 {
			t.Fatalf("rect %+v drew %d rows", r, used)
		}
	}
	if used := Draw(nil, render.Rect{W: 80, H: 24}, testColors(), true, "tag"); used != 0 {
		t.Fatal("a nil buffer drew something")
	}
}

// The tagline is never allowed to widen the logo past its rect, because it is
// the one part of the lockup whose length the package does not control.
func TestTaglineNeverOverflows(t *testing.T) {
	long := "a tagline considerably longer than any terminal is wide, repeated for good measure"
	for _, size := range []Size{SizeFull, SizeMedium, SizeCompact, SizeText} {
		for _, width := range []int{10, 40, 80} {
			for _, l := range Lines(size, testColors(), true, long, width) {
				if l.Width() > width {
					t.Fatalf("size %d at width %d produced a %d-cell line", size, width, l.Width())
				}
			}
		}
	}
}

var svgRect = regexp.MustCompile(`<rect x="(\d+)" y="(\d+)" width="(\d+)" height="(\d+)"`)

// The claim in the package doc is that the SVG and the terminal draw one
// bitmap. That is only true while it is checked: this rebuilds the grid from
// the emitted rects and compares it to the grid itself, so a hand-edit to
// either renderer fails here rather than shipping two marks that differ.
func TestSVGReproducesTheGrid(t *testing.T) {
	const module = 12
	svg := SVG(module)

	grid := make([][]byte, markRows)
	for y := range grid {
		grid[y] = []byte("       ")
	}
	for _, m := range svgRect.FindAllStringSubmatch(svg, -1) {
		x, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[2])
		w, _ := strconv.Atoi(m[3])
		h, _ := strconv.Atoi(m[4])
		if x%module != 0 || y%module != 0 || w%module != 0 || h != module {
			t.Fatalf("rect %v is not on the module grid", m[1:])
		}
		for i := range w / module {
			grid[y/module][x/module+i] = '#'
		}
	}
	for y, row := range append([]string(nil), mark...) {
		want := []byte(row)
		for x := range want {
			if want[x] == '.' {
				want[x] = ' ' // the field is the tile rect, not a knockout rect
			}
		}
		if string(grid[y]) != string(want) {
			t.Fatalf("row %d: SVG draws %q, the grid says %q", y, grid[y], want)
		}
	}
}

// The tile is one rect and the knockouts are runs, not one rect per module: a
// mark that emitted forty-nine rects would still be correct and would still be
// the wrong thing to paste into a README.
func TestSVGCoalescesRuns(t *testing.T) {
	if n := len(svgRect.FindAllString(SVG(12), -1)); n > 16 {
		t.Fatalf("the mark emitted %d knockout rects; runs are not being coalesced", n)
	}
	if got := SVG(0); got != SVG(12) {
		t.Fatal("SVG(0) should fall back to the default module size")
	}
}
