package logo

import (
	"encoding/xml"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/ui/brand"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

var (
	testLit   = render.Hex("#d92d3f")
	testShade = render.Hex("#7a1626")
)

func testColors() Colors {
	return Colors{
		Lit:   render.Style{Fg: testLit},
		Shade: render.Style{Fg: testShade},
		Word:  render.Style{Fg: render.Hex("#c9384a")},
		Tag:   render.Style{Fg: render.Hex("#6e7681")},
	}
}

// describe writes cells as two strings per row: the characters, and one letter
// per cell for what it holds (. nothing, L lit, S shade, B lit on shade).
func describe(rows [][]cell) []string {
	var out []string
	for _, row := range rows {
		var glyphs, tones strings.Builder
		for _, c := range row {
			glyphs.WriteRune(c.r)
			switch {
			case c.fg == toneNone:
				tones.WriteByte('.')
			case c.bg != toneNone:
				tones.WriteByte('B')
			case c.fg == toneLit:
				tones.WriteByte('L')
			default:
				tones.WriteByte('S')
			}
		}
		out = append(out, glyphs.String(), tones.String())
	}
	return out
}

func TestFitLadder(t *testing.T) {
	cases := []struct {
		name    string
		w, h    int
		unicode bool
		want    Size
	}{
		{"wide terminal", 80, 24, true, SizeFull},
		{"exactly full", SizeFull.Width(), fullRows, true, SizeFull},
		{"one column short of full", SizeFull.Width() - 1, fullRows, true, SizeMedium},
		{"one row short of full", SizeFull.Width(), fullRows - 1, true, SizeMedium},
		{"exactly medium", SizeMedium.Width(), mediumRows, true, SizeMedium},
		{"one column short of medium", SizeMedium.Width() - 1, mediumRows, true, SizeCompact},
		{"one row short of medium", SizeMedium.Width(), mediumRows - 1, true, SizeCompact},
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

// The characters a terminal draws, pinned. They are computed from the ribbon's
// measurements in svg.go, so this is where a change to those measurements, or
// to the sampling, shows up as a diff someone has to look at: the same job the
// byte comparison in verify.sh does for the SVG. Each row is the characters and
// then what each cell holds (see describe).
func TestCellsAreTheRibbon(t *testing.T) {
	cases := []struct {
		name string
		got  [][]cell
		want []string
	}{
		{"full", fullCells(), []string{
			"   ▐███▙   ▟██▛▌   ", "...LLLLS...LLLBS...",
			"   ███▘█▖ ▗███▘█   ", "...LLLBSS.LLLLBS...",
			"  ▟██▛██▙ ▟██▌██▙  ", "..LLLBSSS.LLLBSSS..",
			" ▗███ ▜██▐██▛ ███▖ ", ".LLLL.SSSBLLL.SSSS.",
			" ███▘ ▝███▜█▘ ▝███ ", ".LLLL.SSSSBLL.SSSS.",
			"▟██▛   ▐██▝▌   ▜██▙", "LLLL...SSSBL...SSSS",
		}},
		{"medium", mediumCells(), []string{
			" ▗█▌▖▗█▛▖ ", ".LLBSLLBS.",
			" █▛█▙▟█▜█ ", ".LLSSLLSS.",
			"▟█▘▝█▐▘▝█▙", "LLLSSBLSSS",
		}},
		{"compact", [][]cell{compactCells}, []string{
			"▞▚▞▚", "LSLS",
		}},
	}
	for _, c := range cases {
		got := describe(c.got)
		if len(got) != len(c.want) {
			t.Fatalf("%s: %d rows, want %d", c.name, len(got)/2, len(c.want)/2)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s row %d:\n got %q\nwant %q", c.name, i/2, got[i], c.want[i])
			}
		}
	}
}

// occupied is the pixels of a cell the ribbon covers, as a quadrant mask.
func occupied(c cell) int {
	if c.bg != toneNone {
		return 15
	}
	for mask, r := range quadrants {
		if r == c.r {
			return mask
		}
	}
	return -1
}

// What a cell may hold, whatever the sampling decides: a block character, at
// most two tones, a background only where both faces meet, and nothing at all
// where the ribbon is not. A cell that broke one of these would paint outside
// the ribbon or ask the terminal for a character outside the Block Elements.
func TestCellsAreDrawableAndStayInsideTheRibbon(t *testing.T) {
	for name, rows := range map[string][][]cell{"full": fullCells(), "medium": mediumCells(), "compact": {compactCells}} {
		sawLit, sawShade := false, false
		for y, row := range rows {
			for x, c := range row {
				mask := occupied(c)
				switch {
				case mask < 0:
					t.Fatalf("%s (%d,%d): %q is not a quadrant block", name, x, y, c.r)
				case (c.fg == toneNone) != (c.r == ' '):
					t.Fatalf("%s (%d,%d): %q with tone %d; a blank cell and a toneless cell must be the same thing", name, x, y, c.r, c.fg)
				case c.bg != toneNone && (c.fg != toneLit || c.bg != toneShade):
					t.Fatalf("%s (%d,%d): a two-tone cell is lit on shade, not %d on %d", name, x, y, c.fg, c.bg)
				}
				sawLit = sawLit || c.fg == toneLit
				sawShade = sawShade || c.fg == toneShade || c.bg == toneShade
			}
		}
		if !sawLit || !sawShade {
			t.Fatalf("%s shows only one face of the ribbon, so it has no fold", name)
		}
	}
}

// The ribbon is its own mirror image, and so is its silhouette in cells: tones
// swap sides, coverage does not. An asymmetric M is the kind of change that
// looks fine in a diff and wrong on screen.
func TestCellSilhouetteIsSymmetric(t *testing.T) {
	// Swapping a quadrant mask left for right swaps bits 0 and 1, and 2 and 3.
	flip := func(mask int) int {
		return mask&1<<1 | mask&2>>1 | mask&4<<1 | mask&8>>1
	}
	for name, rows := range map[string][][]cell{"full": fullCells(), "medium": mediumCells()} {
		for y, row := range rows {
			for x := range row {
				if a, b := occupied(row[x]), occupied(row[len(row)-1-x]); a != flip(b) {
					t.Fatalf("%s row %d: column %d covers %04b and its mirror covers %04b", name, y, x, a, b)
				}
			}
		}
	}
}

// The sizes are what the ladder measures with, so each must be exactly as wide
// and as tall as the lines it produces. A block character counted as two cells
// wide would centre the mark wrongly and let it overrun its pane.
func TestLinesMatchTheSizesTheLadderUses(t *testing.T) {
	for _, size := range []Size{SizeFull, SizeMedium, SizeCompact, SizeText} {
		lines := Lines(size, testColors(), true, "", 200)
		if len(lines) != size.Height() {
			t.Fatalf("size %d produced %d rows, Height says %d", size, len(lines), size.Height())
		}
		widest := 0
		for _, l := range lines {
			widest = max(widest, l.Width())
		}
		if widest != size.Width() {
			t.Fatalf("size %d is %d cells wide, Width says %d", size, widest, size.Width())
		}
	}
}

// Every row of the lockup starts in the same column, and that column centres
// the lockup's widest row. Draw used to centre each row by its own width, so
// the row carrying the name and the row carrying the tagline started further
// left than the rows around them and the ribbon came out sheared. The check
// reads the ribbon back out of the buffer at one origin for all rows, which is
// what a person looking at the pane does.
func TestDrawStartsEveryRowInOneColumn(t *testing.T) {
	const sentinel = '~'
	const tagline = "the DevCouncil execution harness"
	for _, w := range []int{17, 20, 26, 27, 30, 53, 54, 60, 80, 81} {
		const h = 9
		b := render.NewBuffer(w+8, h+4)
		b.Fill(b.Bounds(), sentinel, render.Style{})
		r := render.Rect{X: 3, Y: 2, W: w, H: h}

		size := Fit(w, h, true)
		rows := size.cells()
		if len(rows) < 2 {
			t.Fatalf("w=%d: picked size %d, which has no rows to line up", w, size)
		}
		widest := 0
		for _, l := range Lines(size, testColors(), true, tagline, w) {
			widest = max(widest, l.Width())
		}
		want := r.X + (w-widest)/2

		if used := Draw(b, r, testColors(), true, tagline); used != len(rows) {
			t.Fatalf("w=%d: drew %d rows, want %d", w, used, len(rows))
		}
		for y, row := range rows {
			first := -1
			for x := range b.W {
				if b.Cell(x, r.Y+y).R != sentinel {
					first = x
					break
				}
			}
			if first != want {
				t.Fatalf("w=%d: row %d starts in column %d, want %d (the lockup is %d wide)", w, y, first, want, widest)
			}
			for x, cl := range row {
				if got := b.Cell(want+x, r.Y+y).R; got != cl.r {
					t.Fatalf("w=%d: row %d column %d holds %q, want %q", w, y, x, got, cl.r)
				}
			}
		}
	}
}

// A cell the mark leaves empty is written on the background the caller lent
// the ribbon, and so is the gap before the name. Written with no background it
// takes the terminal's own, which inside a theme that paints its background is
// a hole the shape of the mark's bounding box.
func TestEmptyCellsTakeTheCallersGround(t *testing.T) {
	ground := render.Hex("#0d1117")
	c := testColors()
	c.Lit.Bg, c.Shade.Bg, c.Word.Bg, c.Tag.Bg = ground, ground, ground, ground
	for _, size := range []Size{SizeFull, SizeMedium, SizeCompact} {
		blank := 0
		for y, l := range Lines(size, c, true, "tag", 200) {
			for _, span := range l {
				if span.Style.Bg != ground && span.Style.Bg != testShade {
					t.Fatalf("size %d row %d: %q is drawn on %v, neither the ground nor the shaded face",
						size, y, span.Text, span.Style.Bg)
				}
				if strings.TrimSpace(span.Text) == "" {
					blank++
				}
			}
		}
		if size != SizeCompact && blank == 0 {
			t.Fatalf("size %d has no empty cell, so this test checked nothing", size)
		}
	}
}

// The mark's own colours reach a background in exactly one situation: a cell
// where both faces of the ribbon meet. Lent no background, as here, nothing
// else paints one, which is what lets the CLI print the mark into a terminal
// whose background it does not know.
func TestOnlyTwoToneCellsPaintABackground(t *testing.T) {
	for _, size := range []Size{SizeFull, SizeMedium, SizeCompact} {
		painted := 0
		for _, l := range Lines(size, testColors(), true, "tag", 200) {
			for _, span := range l {
				if span.Style.Bg == (render.Style{}).Bg {
					continue
				}
				if span.Style.Fg != testLit || span.Style.Bg != testShade {
					t.Fatalf("size %d paints a background behind %q that is not lit on shade", size, span.Text)
				}
				painted++
			}
		}
		if size != SizeCompact && painted == 0 {
			t.Fatalf("size %d never draws a fold: no cell holds both faces", size)
		}
	}
}

// The SVG is the same ribbon, filled as polygons. What follows holds that
// rendering to its own description: whole-number geometry, symmetry, and a
// file every viewer can draw.

// The ribbon's measurements are chosen so that every coordinate is a whole
// number. If one of them is changed to a value that breaks that, the inner
// corners of the folds get rounded and a straight edge acquires a step, which
// no reader of the constants would predict.
func TestFoldMeasurementsDivideExactly(t *testing.T) {
	if rem := (foldStep - foldHalf) * (foldBottom - foldTop) % foldStep; rem != 0 {
		t.Fatalf("the fold's inner corner is not on a whole unit (remainder %d): pick measurements that divide", rem)
	}
	if foldHalf*2 >= foldStep {
		t.Fatalf("a ribbon %d wide cannot fold every %d units without its legs merging", foldHalf*2, foldStep)
	}
	if foldLegs%2 != 0 {
		t.Fatalf("%d legs do not end back on the baseline, so they do not make an M", foldLegs)
	}
}

// The ribbon sits in the middle of the plate and is its own mirror image. Both
// are things the eye checks before it reads the letter.
func TestFoldIsCentredAndSymmetric(t *testing.T) {
	if got := foldColumn(0) + foldColumn(foldLegs); got != svgSide {
		t.Fatalf("the ribbon is off centre horizontally: its ends sum to %d, the plate is %d wide", got, svgSide)
	}
	if got := foldTop + foldBottom; got != svgSide {
		t.Fatalf("the ribbon is off centre vertically: top and bottom sum to %d, the plate is %d tall", got, svgSide)
	}
	outline := foldOutline()
	have := map[point]bool{}
	for _, p := range outline {
		have[p] = true
		// Clear of the plate's rounded corners, which bite a quarter of the
		// way in at this radius.
		if p.x < svgSide/8 || p.x > svgSide*7/8 || p.y < svgSide/8 || p.y > svgSide*7/8 {
			t.Fatalf("outline point %v is too close to the plate's edge", p)
		}
	}
	if len(have) != len(outline) {
		t.Fatalf("the outline repeats a point: %v", outline)
	}
	for _, p := range outline {
		if mirror := (point{svgSide - p.x, p.y}); !have[mirror] {
			t.Fatalf("outline point %v has no mirror image at %v", p, mirror)
		}
	}
}

// The folds are drawn as lit legs laid over a silhouette filled in shade. That
// only reads as a fold if the lit legs are every other one and never reach
// outside the silhouette, so each corner of a lit leg must be a corner of it.
func TestLitLegsAreAlternateAndInsideTheSilhouette(t *testing.T) {
	onOutline := map[point]bool{}
	for _, p := range foldOutline() {
		onOutline[p] = true
	}
	lit := 0
	for i := range foldLegs {
		if !foldOnBaseline(i) {
			continue
		}
		lit++
		for _, p := range foldLeg(i) {
			if !onOutline[p] {
				t.Fatalf("leg %d has a corner at %v that is not on the silhouette", i, p)
			}
		}
	}
	if lit*2 != foldLegs {
		t.Fatalf("%d of %d legs are lit; a fold needs every other one", lit, foldLegs)
	}
}

var (
	svgHex   = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	svgPath  = regexp.MustCompile(` d="([^"]*)"`)
	svgPaint = regexp.MustCompile(`url\(#([\w-]+)\)`)
	svgID    = regexp.MustCompile(` id="([\w-]+)"`)
)

// The SVG is generated, so it must be the same every time, parse as XML, and
// use only what every viewer draws: it is pasted into a README, a badge and a
// favicon.
func TestSVGIsWellFormedAndPortable(t *testing.T) {
	svg := SVG()
	if svg != SVG() {
		t.Fatal("two calls produced different SVG")
	}
	dec := xml.NewDecoder(strings.NewReader(svg))
	for {
		if _, err := dec.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("the SVG does not parse: %v", err)
		}
	}
	for _, banned := range []string{"<mask", "<filter", "<text", "font", "<image", "<use", "style="} {
		if strings.Contains(svg, banned) {
			t.Fatalf("the SVG uses %q, which not every viewer draws", banned)
		}
	}
	paths := svgPath.FindAllStringSubmatch(svg, -1)
	if len(paths) != 2 {
		t.Fatalf("expected the silhouette and the lit legs, found %d paths", len(paths))
	}
	for _, m := range paths {
		if strings.Trim(m[1], "MLZ0123456789 ") != "" {
			t.Fatalf("path data is not whole-number straight lines: %q", m[1])
		}
	}
	// Every paint the drawing asks for is one it defines.
	defined := map[string]bool{}
	for _, m := range svgID.FindAllStringSubmatch(svg, -1) {
		defined[m[1]] = true
	}
	for _, m := range svgPaint.FindAllStringSubmatch(svg, -1) {
		if !defined[m[1]] {
			t.Fatalf("the SVG paints with #%s and never defines it", m[1])
		}
	}
}

// Every colour in the published mark is a named brand colour, or the white of
// the hairline. A hex typed into the generator is a colour the brand package
// does not know it has.
func TestSVGUsesOnlyBrandColours(t *testing.T) {
	known := map[string]bool{
		brand.Red200: true, brand.Red300: true, brand.Red400: true,
		brand.Red500: true, brand.Red700: true,
		brand.PlateTop: true, brand.PlateBottom: true,
		"#ffffff": true,
	}
	seen := map[string]bool{}
	for _, hex := range svgHex.FindAllString(SVG(), -1) {
		if !known[hex] {
			t.Fatalf("the SVG uses %s, which is not a brand colour", hex)
		}
		seen[hex] = true
	}
	for hex := range known {
		if !seen[hex] {
			t.Fatalf("%s is listed as a colour of the mark and the SVG never uses it", hex)
		}
	}
}
