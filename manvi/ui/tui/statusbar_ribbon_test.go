package tui

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// ribbonRunes are the two characters the one-line mark is drawn with.
const ribbonRunes = "▞▚"

// busyStatus is a running turn with enough on the bar to have something to
// lose when the row is short.
func busyStatus(stall int) Status {
	return Status{
		Posture: "strict", Busy: true, BusyLabel: "run_command", StallSecs: stall,
		Model: "fable", Provider: "anthropic", TaskID: "TASK-12", GitBranch: "brand/fold-m",
		Steps: 3,
	}
}

func darkBlocks() Theme {
	th := Dark()
	th.Unicode = true
	return th
}

// statusRow draws the bar and returns its cells.
func statusRow(th Theme, st Status, w, tick int, fxOn bool) []render.Cell {
	b := render.NewBuffer(w, 1)
	DrawStatusBar(b, render.Rect{W: w, H: 1}, th, st, tick, nil, fxOn)
	row := make([]render.Cell, w)
	for x := range w {
		row[x] = b.Cell(x, 0)
	}
	return row
}

func rowText(row []render.Cell) string {
	var s strings.Builder
	for _, c := range row {
		s.WriteRune(c.R)
	}
	return s.String()
}

// ribbonAt returns the column the mark starts at, or -1 when the row has none.
func ribbonAt(row []render.Cell) int {
	for x, c := range row {
		if strings.ContainsRune(ribbonRunes, c.R) {
			return x
		}
	}
	return -1
}

func brightness(c render.Color) int {
	r, g, b, _ := c.RGBA()
	return int(r) + int(g) + int(b)
}

func hasSpinner(th Theme, row []render.Cell, tick int) bool {
	return strings.Contains(rowText(row), render.Spinner(th.Glyphs().Spinner, tick)+" run_command")
}

// While a turn runs the bar shows the mark where the spinner was, with light
// running round it. It is a busy indicator, so at no tick may it be the still
// mark; and its slanted blocks are half empty, so every one of its cells has to
// sit on the bar's own background, which is not the pane's.
func TestBusyIndicatorIsTheRibbonInMotion(t *testing.T) {
	th := darkBlocks()
	faces := th.Logo()
	positions := map[int]bool{}
	for tick := range 32 {
		row := statusRow(th, busyStatus(0), 120, tick, true)
		text := rowText(row)
		if !strings.Contains(text, "▞▚▞▚ run_command") {
			t.Fatalf("tick %d: the busy indicator is not the mark: %q", tick, text)
		}
		if hasSpinner(th, row, tick) {
			t.Fatalf("tick %d: the spinner is drawn as well as the mark: %q", tick, text)
		}
		at := ribbonAt(row)
		lit, brightest, most := 0, -1, 0
		for i := range 5 {
			c := row[at+i]
			if c.Style.Bg != th.BgRaised {
				t.Fatalf("tick %d: indicator cell %d is on %v, not the bar's %v", tick, i, c.Style.Bg, th.BgRaised)
			}
			if i == 4 {
				break
			}
			if c.Style.Fg != faces.Lit.Fg && c.Style.Fg != faces.Shade.Fg {
				lit++
			}
			// The head is the cell lifted furthest from its own face's
			// colour, which is not the brightest cell: a lit face at rest is
			// about as bright as a shaded one with the light full on it.
			face := faces.Lit.Fg
			if i%2 == 1 {
				face = faces.Shade.Fg
			}
			if lift := brightness(c.Style.Fg) - brightness(face); lift > most {
				most, brightest = lift, i
			}
		}
		if lit == 0 {
			t.Fatalf("tick %d: the mark is still while the turn is running: %q", tick, text)
		}
		positions[brightest] = true
	}
	if len(positions) < 4 {
		t.Fatalf("the light only ever led on %d of the mark's four cells", len(positions))
	}
}

// The checks above compare colours as the theme holds them. A terminal gets
// them after its depth has been applied, and at 256 colours a face lifted a
// little can land on the slot it rests on. A mark whose light reduces away is
// a busy indicator that looks hung, so this reads the escapes actually
// written, on both themes, healthy and stalled, at every tick of two laps: on
// each tick some cell must be written differently from how it rests, and that
// set of cells must change from one step of the light to the next.
func TestBusyRibbonStillMovesAfterColourReduction(t *testing.T) {
	const lap, pace = 8, 2
	for _, name := range []string{"dark", "light"} {
		for _, profile := range []render.Profile{render.ANSI256, render.TrueColor} {
			for _, stall := range []int{0, 45} {
				th := PickThemeByNameUnchecked(name, profile, true)
				var frames [][4]string
				for tick := range lap * 2 {
					row := statusRow(th, busyStatus(stall), 140, tick, true)
					at := ribbonAt(row)
					if at < 0 {
						t.Fatalf("%s %v stall %d tick %d: no mark on the row: %q", name, profile, stall, tick, rowText(row))
					}
					var f [4]string
					for i := range 4 {
						f[i] = sequenceFor(t, row[at+i].Style.Fg, profile)
					}
					frames = append(frames, f)
				}
				// A cell rests for half of every lap, so the escape it is
				// written with most often is its resting one.
				var rest [4]string
				for i := range 4 {
					seen := map[string]int{}
					for _, f := range frames {
						seen[f[i]]++
						if seen[f[i]] > seen[rest[i]] {
							rest[i] = f[i]
						}
					}
				}
				lit := func(f [4]string) (set [4]bool, n int) {
					for i := range 4 {
						if f[i] != rest[i] {
							set[i] = true
							n++
						}
					}
					return set, n
				}
				for tick, f := range frames {
					set, n := lit(f)
					if n == 0 {
						t.Fatalf("%s %v stall %d tick %d: every cell is written as it rests: %q", name, profile, stall, tick, f)
					}
					if tick+pace < len(frames) {
						if next, _ := lit(frames[tick+pace]); next == set {
							t.Fatalf("%s %v stall %d tick %d: the light is on the same cells a step later", name, profile, stall, tick)
						}
					}
				}
			}
		}
	}
}

// A turn that has stopped making progress turns the spinner the warning
// colour, and the mark has to say the same thing. It also has to stay where it
// is: swapping it for the spinner at the moment of a stall would shift
// everything after it on the row.
func TestStalledRibbonWarnsAndDoesNotMove(t *testing.T) {
	th := darkBlocks()
	faces := th.Logo()
	const tick = 5
	healthy := statusRow(th, busyStatus(0), 120, tick, true)
	at := ribbonAt(healthy)
	if at < 0 {
		t.Fatalf("no mark on a healthy busy row: %q", rowText(healthy))
	}
	for _, stall := range []int{15, 29, 30, 45} {
		row := statusRow(th, busyStatus(stall), 120, tick, true)
		if got := ribbonAt(row); got != at {
			t.Fatalf("stalled %ds: the mark starts at column %d, %d when healthy: %q", stall, got, at, rowText(row))
		}
		if !strings.Contains(rowText(row), "▞▚▞▚ run_command (no progress") {
			t.Fatalf("stalled %ds: %q", stall, rowText(row))
		}
		recoloured := 0
		for i := range 4 {
			fg := row[at+i].Style.Fg
			if fg != healthy[at+i].Style.Fg {
				recoloured++
			}
			if stall >= 30 && (fg == faces.Lit.Fg || fg == faces.Shade.Fg) {
				t.Fatalf("stalled %ds: cell %d of the mark is still brand red", stall, i)
			}
		}
		if stall >= 30 && recoloured != 4 {
			t.Fatalf("stalled %ds: only %d of the mark's four cells changed colour", stall, recoloured)
		}
		if stall < 30 && recoloured != 0 {
			t.Fatalf("stalled %ds: the mark changed colour before the spinner would have", stall)
		}
	}
}

// Where the mark cannot move it is not used, and the spinner is: a still mark
// beside "working" would be a busy indicator that looks hung. That covers
// animation turned off, a terminal without the blocks, the plain theme, and a
// sixteen-colour terminal, whose theme lends the mark no colour to move toward.
func TestBusyIndicatorFallsBackToTheSpinner(t *testing.T) {
	cases := []struct {
		name string
		th   Theme
		fxOn bool
	}{
		{"animation off", darkBlocks(), false},
		{"no block characters", Dark(), true},
		{"plain theme", NoColorTheme(), true},
		{"sixteen colours", PickThemeByNameUnchecked("dark", render.ANSI16, true), true},
	}
	for _, c := range cases {
		for _, tick := range []int{0, 3, 7} {
			row := statusRow(c.th, busyStatus(0), 120, tick, c.fxOn)
			// Errorf, so one run reports every case that lost its spinner.
			if at := ribbonAt(row); at >= 0 {
				t.Errorf("%s tick %d: the mark is the busy indicator: %q", c.name, tick, rowText(row))
			}
			if !hasSpinner(c.th, row, tick) {
				t.Errorf("%s tick %d: no spinner frame before the label: %q", c.name, tick, rowText(row))
			}
		}
	}
	// And a row that is not busy has neither.
	idle := busyStatus(0)
	idle.Busy = false
	if row := statusRow(darkBlocks(), idle, 120, 0, true); ribbonAt(row) >= 0 {
		t.Fatalf("an idle row shows the mark: %q", rowText(row))
	}
}

// The mark is three cells wider than the spinner, and the bar cuts its left
// side short when the row is too narrow. The mark must not be what causes a
// cut: at the narrowest width the row fits whole with the spinner it keeps the
// spinner, and the mark appears once there is room for it too.
func TestRibbonNeverCostsTheRowItsTail(t *testing.T) {
	th := darkBlocks()
	whole := func(row []render.Cell) bool {
		text := rowText(row)
		return strings.Contains(text, "brand/fold-m") && !strings.Contains(text, "…")
	}
	narrowest := 0
	for w := 40; w <= 120; w++ {
		if whole(statusRow(th, busyStatus(0), w, 0, false)) {
			narrowest = w
			break
		}
	}
	if narrowest == 0 || whole(statusRow(th, busyStatus(0), narrowest-1, 0, false)) {
		t.Fatalf("could not find the width at which the row first fits (got %d)", narrowest)
	}
	for w := narrowest; w <= narrowest+6; w++ {
		row := statusRow(th, busyStatus(0), w, 0, true)
		if !whole(row) {
			t.Fatalf("width %d: the row fits with the spinner and is cut with motion on: %q", w, rowText(row))
		}
		if got, want := ribbonAt(row) >= 0, w >= narrowest+3; got != want {
			t.Fatalf("width %d (row first fits at %d): mark drawn = %v, want %v: %q", w, narrowest, got, want, rowText(row))
		}
	}
}
