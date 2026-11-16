package tui

import (
	"bytes"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/ui/logo"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// styleSequence returns exactly what a terminal at this profile would be sent
// for a style: the real reduction and the real attributes, not a
// re-implementation of either.
func styleSequence(t *testing.T, s render.Style, p render.Profile) string {
	t.Helper()
	var b bytes.Buffer
	if err := render.WriteLines(&b, p, []render.Line{render.Styled("x", s)}); err != nil {
		t.Fatalf("writing a styled line: %v", err)
	}
	return b.String()
}

// sequenceFor is styleSequence for a bare foreground colour.
func sequenceFor(t *testing.T, c render.Color, p render.Profile) string {
	t.Helper()
	return styleSequence(t, render.Style{Fg: c}, p)
}

// The five outcomes the harness exists to keep apart — passed, demoted,
// blocked, granted, degraded — must reach the terminal distinguishable at every
// depth the harness resolves.
//
// This asserts the style, not the colour, and the distinction is the finding
// rather than a convenience. Sixteen colours are six hues and four greys. The
// palette needs more than six hues: Granted is an orange and Degraded a purple,
// and the sixteen contain neither, so those two land on a neighbour's slot no
// matter how good the reduction is. A test that compared only the colours would
// therefore be asserting something unsatisfiable — and, worse, the property that
// actually matters is what the terminal is sent, which is colour and attributes
// together.
//
// Both faults this covers were invisible to a colour-by-colour reading of the
// theme. The reduction collapsed load-bearing colours onto bright black, and the
// attribute fallback that was supposed to catch that was keyed to the plain
// *theme* rather than the resolved *profile*, so it never ran on a real
// 16-colour terminal.
func TestOutcomeStatesStayDistinguishable(t *testing.T) {
	outcomes := []struct {
		name string
		kind StatusKind
	}{
		{"passed", StatusPass},
		{"demoted", StatusWarn},
		{"blocked", StatusBlock},
		{"granted", StatusGranted},
		{"degraded", StatusDegraded},
	}
	profiles := []struct {
		name string
		p    render.Profile
	}{
		{"truecolor", render.TrueColor},
		{"256", render.ANSI256},
		{"16", render.ANSI16},
		{"none", render.NoColor},
	}
	for _, theme := range []string{"dark", "light", "plain"} {
		for _, prof := range profiles {
			t.Run(theme+"/"+prof.name, func(t *testing.T) {
				t.Setenv("MANVI_TUI_THEME", theme)
				th := PickTheme(prof.p, true)
				painted := make(map[string]string, len(outcomes))
				for _, o := range outcomes {
					seq := styleSequence(t, th.Status(o.kind), prof.p)
					if prior, clash := painted[seq]; clash {
						t.Fatalf("%s and %s are both painted as %q; a %s terminal cannot tell them apart",
							prior, o.name, seq, prof.name)
					}
					painted[seq] = o.name
				}
			})
		}
	}
}

// Info is not one of the five outcomes, but it shares Degraded's slot at
// sixteen colours — both are the only blues in the palette — and a note about a
// check that could not run must not read as an ordinary informational line.
func TestDegradedIsNotJustInfo(t *testing.T) {
	for _, theme := range []string{"dark", "light"} {
		t.Run(theme, func(t *testing.T) {
			t.Setenv("MANVI_TUI_THEME", theme)
			th := PickTheme(render.ANSI16, true)
			info := styleSequence(t, th.Status(StatusInfo), render.ANSI16)
			degraded := styleSequence(t, th.Status(StatusDegraded), render.ANSI16)
			if info == degraded {
				t.Fatalf("a degraded check and an informational line are both painted as %q", info)
			}
		})
	}
}

// The brand is a red and Danger is a red, and the whole harness rests on a
// blocked write being unmistakable. Truecolor is the easy case; the one that
// matters is a 16-colour terminal, where two reds that reduce to the same slot
// would make a focus ring and a refusal identical.
//
// This paints both through the real reduction at every profile rather than
// comparing the hexes, because the hexes differing proves nothing about what
// arrives at the terminal.
func TestAccentStaysSeparableFromDanger(t *testing.T) {
	profiles := []struct {
		name string
		p    render.Profile
	}{
		{"truecolor", render.TrueColor},
		{"256", render.ANSI256},
		{"16", render.ANSI16},
	}
	for _, th := range []Theme{Dark(), Light()} {
		for _, prof := range profiles {
			t.Run(th.Name+"/"+prof.name, func(t *testing.T) {
				accent := sequenceFor(t, th.Accent, prof.p)
				for name, other := range map[string]render.Color{
					"danger":  th.Danger,
					"info":    th.Info,
					"success": th.Success,
				} {
					if accent == sequenceFor(t, other, prof.p) {
						t.Fatalf("the accent and %s reduce to the same sequence %q at %s",
							name, accent, prof.name)
					}
				}
			})
		}
	}
}

// relativeLuminance is the WCAG definition, on the components a Color can
// report.
func relativeLuminance(t *testing.T, c render.Color) float64 {
	t.Helper()
	r, g, b, ok := c.RGBA()
	if !ok {
		t.Fatal("a themed colour has no components; it defaulted to the terminal's own")
	}
	channel := func(v uint8) float64 {
		f := float64(v) / 255
		if f <= 0.04045 {
			return f / 12.92
		}
		return math.Pow((f+0.055)/1.055, 2.4)
	}
	return 0.2126*channel(r) + 0.7152*channel(g) + 0.0722*channel(b)
}

func contrast(t *testing.T, a, b render.Color) float64 {
	t.Helper()
	la, lb := relativeLuminance(t, a), relativeLuminance(t, b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// A dark red is easy to pick and easy to pick too dark. The accent carries the
// mark, the focus ring, and the session title, so it is held to the 3:1 floor
// for large and bold text on its own surface — the floor it was chosen against,
// asserted rather than remembered.
func TestAccentIsLegibleOnItsSurface(t *testing.T) {
	for _, th := range []Theme{Dark(), Light()} {
		t.Run(th.Name, func(t *testing.T) {
			if got := contrast(t, th.Accent, th.Bg); got < 3 {
				t.Fatalf("accent on the background is %.2f:1, below the 3:1 floor", got)
			}
			// Body text over a selection is small text, which is the stricter
			// 4.5:1 case.
			if got := contrast(t, th.Fg, th.Selection); got < 4.5 {
				t.Fatalf("foreground on the selection fill is %.2f:1, below the 4.5:1 floor", got)
			}
		})
	}
}

// The mark's fold is two colours, so the two must still be two after a
// terminal's depth has been applied to them. At sixteen colours both brand reds
// used to land on the same slot, which drew every fold as red on red. The
// comparison is on the escapes actually written, because that is the only
// place the reduction happens.
func TestRibbonFacesStayApartAtEveryDepth(t *testing.T) {
	for _, name := range []string{"dark", "light"} {
		for _, profile := range []render.Profile{render.ANSI16, render.ANSI256, render.TrueColor} {
			c := PickThemeByNameUnchecked(name, profile, true).Logo()
			lit, shade := sequenceFor(t, c.Lit.Fg, profile), sequenceFor(t, c.Shade.Fg, profile)
			if lit == sequenceFor(t, render.Default, profile) {
				t.Fatalf("%s at %v: the lit face was written with no colour: %q", name, profile, lit)
			}
			if lit == shade {
				t.Fatalf("%s at %v: both faces are written as %q", name, profile, lit)
			}

			// The same has to hold while the mark moves. A glint blends each
			// face toward a third colour, and two blends that were apart as
			// colours can reduce to one slot. Every cell holding both faces is
			// checked at every tick of an unfold, a scheduled glint and a
			// nudged one.
			folds := 0
			for tick := range 150 {
				m := logo.Motion{Live: true, Tick: tick, Nudge: 60}
				for _, l := range logo.LinesAt(logo.SizeFull, c, true, "", 200, m) {
					for _, span := range l {
						if span.Style.Bg == c.Lit.Bg || strings.TrimSpace(span.Text) == "" {
							continue
						}
						folds++
						fg, bg := sequenceFor(t, span.Style.Fg, profile), sequenceFor(t, span.Style.Bg, profile)
						if fg == bg {
							t.Fatalf("%s at %v tick %d: a fold is written %q on the same colour", name, profile, tick, fg)
						}
					}
				}
			}
			if folds == 0 {
				t.Fatalf("%s at %v: no fold was drawn in 150 ticks, so nothing was checked", name, profile)
			}
		}
	}
}

// At sixteen colours the theme lends the mark no glint, so once it has
// unfolded it is the still mark at every tick.
func TestSixteenColourMarkHasNoGlint(t *testing.T) {
	c := PickThemeByNameUnchecked("dark", render.ANSI16, true).Logo()
	still := logo.Lines(logo.SizeFull, c, true, "", 200)
	for tick := 12; tick < 150; tick++ {
		got := logo.LinesAt(logo.SizeFull, c, true, "", 200, logo.Motion{Live: true, Tick: tick, Nudge: 60})
		if !reflect.DeepEqual(got, still) {
			t.Fatalf("tick %d: the sixteen-colour mark was recoloured", tick)
		}
	}
	if full := PickThemeByNameUnchecked("dark", render.TrueColor, true).Logo(); full.Glint.IsDefault() {
		t.Fatal("the truecolor theme lends the mark no glint either")
	}
}

// The plain theme is the fallback that makes the status distinctions survive
// NO_COLOR, and the mark must not draw a solid tile of the terminal's own
// foreground into it.
func TestPlainThemeDrawsNoTile(t *testing.T) {
	if NoColorTheme().LogoBlocks() {
		t.Fatal("the plain theme reports that it can draw the block mark")
	}
	unicode := Dark()
	unicode.Unicode = true
	if !unicode.LogoBlocks() {
		t.Fatal("a unicode dark theme cannot draw the block mark")
	}
}
