package logo

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

const motionTag = "the DevCouncil execution harness"

// glintColors are testColors with a glint lent, which is what turns the light
// on.
func glintColors() Colors {
	c := testColors()
	c.Glint = render.Hex("#f0959e")
	return c
}

// painted is one cell of a rendered line.
type painted struct {
	r rune
	s render.Style
}

// cellsOf flattens rendered lines into cells, so two frames can be compared
// cell by cell regardless of how their runs were split.
func cellsOf(lines []render.Line) [][]painted {
	out := make([][]painted, len(lines))
	for y, l := range lines {
		for _, span := range l {
			for _, r := range span.Text {
				out[y] = append(out[y], painted{r, span.Style})
			}
		}
	}
	return out
}

// ribbonRunes are the characters only the ribbon is drawn with.
const ribbonRunes = "▘▝▀▖▌▞▛▗▚▐▜▄▙▟█"

func isRibbon(r rune) bool { return strings.ContainsRune(ribbonRunes, r) }

// settled is a tick at which a live mark has unfolded and no light is on it.
const settled = unfoldTicks + glintTicks

var motionSizes = []Size{SizeFull, SizeMedium, SizeCompact}

// The mark at rest is the mark without motion, to the span. That is what lets
// the CLI, the pinned characters above and a terminal with animation off all
// keep drawing through the functions they already call.
func TestMarkAtRestIsTheMarkWithoutMotion(t *testing.T) {
	for _, size := range motionSizes {
		still := Lines(size, glintColors(), true, motionTag, 80)
		if got := LinesAt(size, glintColors(), true, motionTag, 80, Motion{}); !reflect.DeepEqual(got, still) {
			t.Fatalf("size %d: the zero Motion does not draw the still mark", size)
		}
		// A dead Motion is still whatever its other fields say.
		if got := LinesAt(size, glintColors(), true, motionTag, 80, Motion{Tick: 3, Nudge: 2}); !reflect.DeepEqual(got, still) {
			t.Fatalf("size %d: a Motion that is not live moved the mark", size)
		}
		if got := LinesAt(size, glintColors(), true, motionTag, 80, Motion{Live: true, Tick: settled}); !reflect.DeepEqual(got, still) {
			t.Fatalf("size %d: a live mark that has settled is not the still mark", size)
		}
	}
}

// The positions along the ribbon, pinned for the three-row mark: 0 at the foot
// of the first leg, up it, down the second, and so on to b (11) at the foot of
// the last. Every motion is a function of these, so a change to how a cell is
// assigned to a leg shows up here as a diff.
func TestStepsRunAlongTheRibbon(t *testing.T) {
	const digits = "0123456789ab"
	want := []string{
		".22238889.",
		".114477aa.",
		"0005556bbb",
	}
	var got []string
	for _, row := range mediumCells() {
		var s strings.Builder
		for _, c := range row {
			if c.fg == toneNone {
				s.WriteByte('.')
				continue
			}
			s.WriteByte(digits[c.at])
		}
		got = append(got, s.String())
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the three-row mark's steps:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// On every tall rung the ribbon starts at the bottom left, ends at the
	// bottom right, and leaves no step without a cell: an unfold that reached
	// a step nothing is drawn at would pause for no visible reason.
	for _, size := range []Size{SizeFull, SizeMedium} {
		rows := size.cells()
		n := steps(rows)
		used := make([]bool, n)
		for y, row := range rows {
			for x, c := range row {
				if c.fg == toneNone {
					continue
				}
				if int(c.at) >= n {
					t.Fatalf("size %d: cell (%d,%d) is at step %d of %d", size, x, y, c.at, n)
				}
				used[c.at] = true
				bottom := y == len(rows)-1
				if c.at == 0 && (!bottom || x*2 >= len(row)) {
					t.Fatalf("size %d: step 0 is at (%d,%d), not the foot of the first leg", size, x, y)
				}
				if int(c.at) == n-1 && (!bottom || x*2 < len(row)) {
					t.Fatalf("size %d: the last step is at (%d,%d), not the foot of the last leg", size, x, y)
				}
			}
		}
		for at, ok := range used {
			if !ok {
				t.Fatalf("size %d: no cell is at step %d", size, at)
			}
		}
	}
}

// The unfold starts with no ribbon, ends with all of it, and in between only
// ever adds cells: what it has drawn stays drawn, in the character it will
// have at rest. Throughout, every line keeps its width and the name and the
// tagline are whole, which is what holds the lockup still while it happens.
func TestUnfoldOnlyAddsCells(t *testing.T) {
	for _, size := range motionSizes {
		still := cellsOf(Lines(size, glintColors(), true, motionTag, 80))
		at := func(tick int) [][]painted {
			return cellsOf(LinesAt(size, glintColors(), true, motionTag, 80, Motion{Live: true, Tick: tick}))
		}
		count := func(frame [][]painted) int {
			n := 0
			for _, row := range frame {
				for _, c := range row {
					if isRibbon(c.r) {
						n++
					}
				}
			}
			return n
		}

		if n := count(at(0)); n != 0 {
			t.Fatalf("size %d: %d ribbon cells are drawn before the unfold starts", size, n)
		}
		prev := at(0)
		for tick := 0; tick <= unfoldTicks; tick++ {
			frame := at(tick)
			for y, row := range frame {
				if len(row) != len(still[y]) {
					t.Fatalf("size %d tick %d: row %d is %d cells wide, %d at rest", size, tick, y, len(row), len(still[y]))
				}
				for x, c := range row {
					rest := still[y][x]
					switch {
					case isRibbon(rest.r):
						if c.r != rest.r && c.r != ' ' {
							t.Fatalf("size %d tick %d: (%d,%d) holds %q, which is neither blank nor the %q it rests as",
								size, tick, x, y, c.r, rest.r)
						}
						if isRibbon(prev[y][x].r) && c.r == ' ' {
							t.Fatalf("size %d tick %d: (%d,%d) was drawn and then withdrawn", size, tick, x, y)
						}
					case c != rest:
						t.Fatalf("size %d tick %d: (%d,%d) is not part of the ribbon and changed", size, tick, x, y)
					}
				}
			}
			if tick > 0 && count(frame) < count(prev) {
				t.Fatalf("size %d tick %d: the ribbon shrank", size, tick)
			}
			prev = frame
		}
		if count(prev) != count(still) {
			t.Fatalf("size %d: %d ribbon cells after the unfold, %d at rest", size, count(prev), count(still))
		}

		// It starts where the ribbon does. The first cells drawn are in the
		// bottom row of the left half.
		for tick := 1; tick <= unfoldTicks; tick++ {
			frame := at(tick)
			if count(frame) == 0 {
				continue
			}
			for y, row := range frame {
				for x, c := range row {
					if !isRibbon(c.r) {
						continue
					}
					if size != SizeCompact && y < len(frame)-2 {
						t.Fatalf("size %d: the unfold's first cells include row %d", size, y)
					}
					if x*2 >= len(still[y]) {
						t.Fatalf("size %d: the unfold's first cells include column %d", size, x)
					}
				}
			}
			break
		}
	}
}

// Start moves the unfold, so a mark that comes on screen late still draws
// itself in rather than appearing whole.
func TestUnfoldStartsWhereItIsTold(t *testing.T) {
	still := Lines(SizeFull, glintColors(), true, motionTag, 80)
	at := func(tick int) []render.Line {
		return LinesAt(SizeFull, glintColors(), true, motionTag, 80, Motion{Live: true, Tick: tick, Start: 500})
	}
	for _, tick := range []int{0, 499, 500} {
		for _, row := range cellsOf(at(tick)) {
			for _, c := range row {
				if isRibbon(c.r) {
					t.Fatalf("tick %d: the ribbon is drawn before a start of 500", tick)
				}
			}
		}
	}
	if !reflect.DeepEqual(at(500+settled), still) {
		t.Fatal("the mark has not settled a full unfold and glint after its start")
	}
}

// lit counts the cells a frame draws in a different style from the still mark,
// and fails if any cell's character differs: light recolours, and that is all
// it does.
func lit(t *testing.T, size Size, m Motion) int {
	t.Helper()
	still := cellsOf(Lines(size, glintColors(), true, motionTag, 80))
	n := 0
	for y, row := range cellsOf(LinesAt(size, glintColors(), true, motionTag, 80, m)) {
		if len(row) != len(still[y]) {
			t.Fatalf("size %d tick %d: row %d changed width", size, m.Tick, y)
		}
		for x, c := range row {
			if c.r != still[y][x].r {
				t.Fatalf("size %d tick %d: (%d,%d) holds %q, %q at rest", size, m.Tick, x, y, c.r, still[y][x].r)
			}
			if c.s != still[y][x].s {
				if !isRibbon(c.r) {
					t.Fatalf("size %d tick %d: (%d,%d) is not ribbon and was recoloured", size, m.Tick, x, y)
				}
				n++
			}
		}
	}
	return n
}

// Once it has unfolded the mark is still except while a glint crosses it, and a
// glint changes colours only. It runs once every glintEvery ticks for
// glintTicks of them, and the frames between are the still mark exactly, which
// is what keeps the animation's cost to the cells the light is on.
func TestGlintRecoloursAndThenLeaves(t *testing.T) {
	for _, size := range motionSizes {
		ribbon := 0
		for _, row := range size.cells() {
			for _, c := range row {
				if c.fg != toneNone {
					ribbon++
				}
			}
		}
		for period := range 2 {
			base := unfoldTicks + period*glintEvery
			peak := 0
			for phase := range glintEvery {
				n := lit(t, size, Motion{Live: true, Tick: base + phase})
				if phase >= glintTicks && n != 0 {
					t.Fatalf("size %d: %d cells are lit %d ticks into a period, after the glint has gone", size, n, phase)
				}
				peak = max(peak, n)
			}
			if peak == 0 {
				t.Fatalf("size %d period %d: no glint ever lit a cell", size, period)
			}
			if size != SizeCompact && peak*2 > ribbon {
				t.Fatalf("size %d: a glint lit %d of the ribbon's %d cells at once", size, peak, ribbon)
			}
		}
	}
}

// A nudge sends a glint at once, in the long stillness between the scheduled
// ones. A nudge from before the mark came on screen is not answered.
func TestNudgeSendsAGlint(t *testing.T) {
	const rest = unfoldTicks + glintTicks + 20
	for _, size := range motionSizes {
		if n := lit(t, size, Motion{Live: true, Tick: rest}); n != 0 {
			t.Fatalf("size %d: %d cells are lit at a tick chosen for being still", size, n)
		}
		answered := 0
		for since := range glintTicks {
			answered += lit(t, size, Motion{Live: true, Tick: rest, Nudge: rest - since})
		}
		if answered == 0 {
			t.Fatalf("size %d: a nudge lit nothing", size)
		}
		if n := lit(t, size, Motion{Live: true, Tick: rest, Nudge: rest - glintTicks}); n != 0 {
			t.Fatalf("size %d: %d cells are still lit when a nudge's glint should have left", size, n)
		}
		if n := lit(t, size, Motion{Live: true, Tick: rest + 100, Start: 100, Nudge: 100}); n != 0 {
			t.Fatalf("size %d: a nudge from before the mark appeared lit %d cells", size, n)
		}
	}
}

// The bare ribbon is the one-line rung without its name, cell for cell, so a
// caller that sets the mark inside a line of its own draws the same mark.
func TestRibbonIsTheOneLineRungWithoutItsName(t *testing.T) {
	c := glintColors()
	for _, m := range []Motion{{}, {Live: true, Tick: 5}, {Live: true, Tick: 7, Working: true}} {
		want := LinesAt(SizeCompact, c, true, "", 80, m)[0]
		got := Ribbon(c, m).Append(" "+Name, c.Word)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("motion %+v: the bare ribbon and the one-line rung differ", m)
		}
	}
	text := ""
	for _, span := range Ribbon(c, Motion{}) {
		text += span.Text
	}
	if text != "▞▚▞▚" {
		t.Fatalf("the bare ribbon is %q", text)
	}
}

// A working mark is a busy indicator, and the one thing a busy indicator must
// never do is look idle. So at every tick it is whole, some of it is lit, and
// the light is somewhere new each step: it laps the ribbon with its tail
// wrapping behind it, and has no frame of rest for a stalled screen to be
// mistaken for.
func TestWorkingMarkIsWholeAndNeverStill(t *testing.T) {
	for _, size := range motionSizes {
		lap := steps(size.cells()) * workPace
		if size == SizeCompact {
			lap = foldLegs * workPace
		}
		still := cellsOf(Lines(size, glintColors(), true, motionTag, 80))
		frame := func(tick int) [][]painted {
			// Start and Nudge are set to values that would hide or light the
			// mark if a working mark read them.
			m := Motion{Live: true, Working: true, Tick: tick, Start: tick + 100, Nudge: tick - 1}
			return cellsOf(LinesAt(size, glintColors(), true, motionTag, 80, m))
		}
		everLit := map[[2]int]bool{}
		for tick := range lap * 3 {
			if n := lit(t, size, Motion{Live: true, Working: true, Tick: tick, Start: tick + 100}); n == 0 {
				t.Fatalf("size %d tick %d: a working mark is drawn exactly as the still one", size, tick)
			}
			cur := frame(tick)
			for y, row := range cur {
				for x, c := range row {
					if c.s != still[y][x].s {
						everLit[[2]int{x, y}] = true
					}
				}
			}
			if !reflect.DeepEqual(cur, frame(tick+lap)) {
				t.Fatalf("size %d tick %d: the working mark does not repeat every %d ticks", size, tick, lap)
			}
			if tick%workPace == 0 && reflect.DeepEqual(cur, frame(tick+workPace)) {
				t.Fatalf("size %d tick %d: the light did not move in %d ticks", size, tick, workPace)
			}
		}
		for y, row := range still {
			for x, c := range row {
				if isRibbon(c.r) && !everLit[[2]int{x, y}] {
					t.Fatalf("size %d: the light never reaches (%d,%d) in three laps", size, x, y)
				}
			}
		}
	}
}

// With no glint lent the mark unfolds and is then still for good: nothing is
// ever recoloured. This is the mark a sixteen-colour terminal gets.
func TestNoGlintColourMeansNoLight(t *testing.T) {
	for _, size := range motionSizes {
		still := Lines(size, testColors(), true, motionTag, 80)
		for tick := unfoldTicks; tick < unfoldTicks+glintEvery+glintTicks; tick++ {
			got := LinesAt(size, testColors(), true, motionTag, 80, Motion{Live: true, Tick: tick, Nudge: tick - 2})
			if !reflect.DeepEqual(got, still) {
				t.Fatalf("size %d tick %d: the mark moved with no glint colour to move toward", size, tick)
			}
		}
		// And the unfold's leading cells are in the faces' own colours.
		for tick := range unfoldTicks {
			for _, row := range cellsOf(LinesAt(size, testColors(), true, motionTag, 80, Motion{Live: true, Tick: tick})) {
				for _, c := range row {
					if isRibbon(c.r) && c.s.Fg != testLit && c.s.Fg != testShade {
						t.Fatalf("size %d tick %d: a cell is drawn in %v", size, tick, c.s.Fg)
					}
				}
			}
		}
	}
}

// A cell that holds both faces must keep two colours while light is on it, and
// the lit face must stay the lighter of the two, or the fold flattens for the
// length of the glint.
func TestFoldKeepsItsTwoFacesUnderLight(t *testing.T) {
	sum := func(c render.Color) int {
		r, g, b, ok := c.RGBA()
		if !ok {
			t.Fatalf("a face is drawn in a colour with no components: %v", c)
		}
		return int(r) + int(g) + int(b)
	}
	for _, size := range []Size{SizeFull, SizeMedium} {
		folds := 0
		for tick := range unfoldTicks + glintEvery + glintTicks {
			m := Motion{Live: true, Tick: tick, Nudge: unfoldTicks + 40}
			for _, row := range cellsOf(LinesAt(size, glintColors(), true, motionTag, 80, m)) {
				for _, c := range row {
					if !isRibbon(c.r) || c.s.Bg.IsDefault() {
						continue
					}
					folds++
					if sum(c.s.Fg) <= sum(c.s.Bg) {
						t.Fatalf("size %d tick %d: a fold is drawn %v on %v", size, tick, c.s.Fg, c.s.Bg)
					}
				}
			}
		}
		if folds == 0 {
			t.Fatalf("size %d: no fold was ever drawn, so this test checked nothing", size)
		}
	}
}

// DrawAt writes exactly the cells Draw writes, at every tick, and reports the
// same rows. Motion must not be able to move the lockup, widen it, or change
// how much room it tells its caller it took.
func TestDrawAtCoversTheSameCellsAtEveryTick(t *testing.T) {
	const sentinel = '~'
	for _, w := range []int{9, 12, 17, 26, 30, 53, 80} {
		for _, h := range []int{1, 3, 9} {
			r := render.Rect{X: 3, Y: 2, W: w, H: h}
			draw := func(m Motion) (*render.Buffer, int) {
				b := render.NewBuffer(w+8, h+4)
				b.Fill(b.Bounds(), sentinel, render.Style{})
				return b, DrawAt(b, r, glintColors(), true, motionTag, m)
			}
			still, used := draw(Motion{})
			for tick := 0; tick < unfoldTicks+glintEvery+glintTicks; tick += 3 {
				got, n := draw(Motion{Live: true, Tick: tick, Nudge: tick - 4})
				if n != used {
					t.Fatalf("w=%d h=%d tick %d: used %d rows, %d at rest", w, h, tick, n, used)
				}
				for y := range got.H {
					for x := range got.W {
						a, b := got.Cell(x, y).R == sentinel, still.Cell(x, y).R == sentinel
						if a != b {
							t.Fatalf("w=%d h=%d tick %d: (%d,%d) is written in one frame and not the other", w, h, tick, x, y)
						}
					}
				}
			}
		}
	}
}
