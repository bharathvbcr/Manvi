// Package tui is the harness's full-screen face.
//
// The structure is taken from Grok Build's pager crate, which is the clearest
// public statement of how a coding-agent TUI should be organised: an Elm-style
// loop — input becomes an Action, a dispatcher turns Actions into state changes
// and Effects, Effects run off the loop and feed results back as Actions — with
// an AppView owning global concerns and an AgentView per session owning a
// prompt, a scrollback, panes, and modals.
//
// What is deliberately *not* taken from it is the idea that the TUI is the
// application. Here it is one consumer of the same ui.Event stream the
// newline-delimited-JSON face consumes, and it answers approvals through the
// same ui.Approver seam a headless run answers them through. Anything visible
// here is visible to a CI job, and no permission decision exists that only the
// terminal can make.
//
// The invariant the rest of the harness is built on carries into the rendering:
// a decision that was allowed by a grant, demoted by the posture, or reached
// with a check that could not run is never drawn the way a clean pass is drawn.
// A tick mark is reserved for rules that actually passed.
package tui

import (
	"os"
	"strings"

	"manvi/ui/brand"
	"manvi/ui/logo"
	"manvi/ui/render"
)

// Theme is the semantic palette. Views name roles, never colours, so a
// re-theme is one table and cannot leave a pane referring to a colour that no
// longer means what it did.
type Theme struct {
	Name string

	// Profile is the colour depth this theme was resolved for.
	//
	// A palette does not know what a terminal can show, and the difference is
	// load-bearing rather than cosmetic: the status colours below are chosen to
	// be distinct, and at sixteen colours several of them are not. So the theme
	// carries the depth it was resolved at and Status consults it. The zero
	// value is NoColor, which is the safe end — a theme built without a
	// resolved profile falls back to attributes rather than assuming a terminal
	// that can tell six hues apart.
	Profile render.Profile

	// Surfaces, from furthest back to nearest front.
	Bg        render.Color
	BgRaised  render.Color
	BgOverlay render.Color
	BgInset   render.Color

	// Text, in descending emphasis.
	Fg       render.Color
	FgMuted  render.Color
	FgSubtle render.Color
	FgOn     render.Color // text drawn on an accent fill

	// Brand.
	Accent    render.Color
	AccentDim render.Color

	// Status. These are the load-bearing ones: they encode the difference
	// between a rule that passed, one that was cleared by a grant, one that the
	// posture demoted, and one that could not run. Never collapsed into
	// pass/fail.
	//
	// Distinct hexes do not make them distinct on a terminal. Sixteen colours
	// are six hues and four greys, and the palette needs more distinctions than
	// that: there is no orange, so Granted lands on the same slot as Warning on
	// dark and Danger on light, and no purple, so Degraded lands on Info. The
	// colours are therefore only half of the mechanism — Status pairs them with
	// attributes whenever the resolved profile cannot keep them apart, and
	// theme_test.go paints every outcome through the real reduction at every
	// profile to prove the pairs stay separable.
	Success  render.Color
	Warning  render.Color
	Danger   render.Color
	Info     render.Color
	Granted  render.Color
	Degraded render.Color

	Border      render.Color
	BorderFocus render.Color
	Selection   render.Color

	// Unicode reports whether box drawing and block elements are safe.
	Unicode bool
}

// Dark is the default. It is built around a near-black surface rather than pure
// black so that raised panels can sit above it without a border, and around the
// brand's dark red.
//
// The accent is the one colour here that had to be argued for rather than
// picked: Danger is already a red, and a brand red that reduced to the same
// terminal slot would leave a focus ring and a blocked write indistinguishable
// on a 16-colour terminal. The two are kept apart by depth rather than by hue —
// the accent is the deeper crimson, Danger the brighter vermilion — and
// theme_test.go paints both through the real reduction at every profile to
// prove they still differ. Package brand carries the rest of the reasoning.
func Dark() Theme {
	return Theme{
		Name:      "dark",
		Profile:   render.TrueColor,
		Bg:        render.Hex("#0d1117"),
		BgRaised:  render.Hex("#161b22"),
		BgOverlay: render.Hex("#1c2128"),
		BgInset:   render.Hex("#010409"),

		Fg:       render.Hex("#e6edf3"),
		FgMuted:  render.Hex("#9198a1"),
		FgSubtle: render.Hex("#6e7681"),
		FgOn:     render.Hex("#0d1117"),

		Accent:    render.Hex(brand.AccentDark),
		AccentDim: render.Hex(brand.AccentDimDark),

		Success:  render.Hex("#3fb950"),
		Warning:  render.Hex("#d29922"),
		Danger:   render.Hex("#f85149"),
		Info:     render.Hex("#58a6ff"),
		Granted:  render.Hex("#db9d47"),
		Degraded: render.Hex("#bc8cff"),

		Border:      render.Hex("#30363d"),
		BorderFocus: render.Hex(brand.AccentDark),
		Selection:   render.Hex(brand.SelectionDark),
	}
}

// Light is for terminals with a light background. It is not the dark theme
// inverted: the same hues at the same lightness are illegible on white, so the
// text and status colours are darkened and the surfaces re-ordered.
func Light() Theme {
	return Theme{
		Name:      "light",
		Profile:   render.TrueColor,
		Bg:        render.Hex("#ffffff"),
		BgRaised:  render.Hex("#f6f8fa"),
		BgOverlay: render.Hex("#eaeef2"),
		BgInset:   render.Hex("#f0f3f6"),

		Fg:       render.Hex("#1f2328"),
		FgMuted:  render.Hex("#59636e"),
		FgSubtle: render.Hex("#818b98"),
		FgOn:     render.Hex("#ffffff"),

		Accent:    render.Hex(brand.AccentLight),
		AccentDim: render.Hex(brand.AccentDimLight),

		Success:  render.Hex("#1a7f37"),
		Warning:  render.Hex("#9a6700"),
		Danger:   render.Hex("#cf222e"),
		Info:     render.Hex("#0969da"),
		Granted:  render.Hex("#bc4c00"),
		Degraded: render.Hex("#8250df"),

		Border:      render.Hex("#d1d9e0"),
		BorderFocus: render.Hex(brand.AccentLight),
		Selection:   render.Hex(brand.SelectionLight),
	}
}

// NoColorTheme drops every colour but keeps the attribute vocabulary.
//
// It exists because the status distinctions above are load-bearing, and a theme
// that conveys them only through colour conveys nothing under NO_COLOR or to a
// colour-blind operator. Bold, italic, underline, and reverse carry the same
// five outcomes. Status draws on the same vocabulary for any terminal too
// shallow to keep the colours apart, so this theme is the extreme of a
// fallback rather than a separate mechanism.
func NoColorTheme() Theme {
	t := Theme{Name: "plain", Profile: render.NoColor}
	for _, c := range []*render.Color{
		&t.Bg, &t.BgRaised, &t.BgOverlay, &t.BgInset,
		&t.Fg, &t.FgMuted, &t.FgSubtle, &t.FgOn,
		&t.Accent, &t.AccentDim,
		&t.Success, &t.Warning, &t.Danger, &t.Info, &t.Granted, &t.Degraded,
		&t.Border, &t.BorderFocus, &t.Selection,
	} {
		*c = render.Default
	}
	return t
}

// PickTheme resolves the theme from the environment and the colour profile.
func PickTheme(profile render.Profile, unicode bool) Theme {
	envTheme := os.Getenv("MANVI_TUI_THEME")
	if t, ok := PickThemeByName(envTheme, profile, unicode); ok {
		return t
	}
	if profile == render.NoColor {
		return PickThemeByNameUnchecked("plain", profile, unicode)
	}
	return PickThemeByNameUnchecked("dark", profile, unicode)
}

// PickThemeByName resolves a theme by name ("dark", "light", "plain").
func PickThemeByName(name string, profile render.Profile, unicode bool) (Theme, bool) {
	var t Theme
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "light":
		t = Light()
	case "dark":
		t = Dark()
	case "plain", "none", "mono", "nocolor":
		t = NoColorTheme()
	default:
		return Theme{}, false
	}
	if t.Name != "plain" {
		t.Profile = profile
	}
	t.Unicode = unicode
	return t, true
}

func PickThemeByNameUnchecked(name string, profile render.Profile, unicode bool) Theme {
	t, _ := PickThemeByName(name, profile, unicode)
	return t
}

// Inset is the recessed surface style for code blocks, diffs, and input boxes.
func (t Theme) Inset() render.Style {
	return render.Style{Fg: t.Fg, Bg: t.BgInset}
}

// monochrome reports whether the terminal will be sent no colour at all, so
// that a role which differs from another only by hue would arrive identical.
func (t Theme) monochrome() bool {
	return t.Name == "plain" || t.Profile == render.NoColor
}

// reducedPalette reports whether the terminal has too few colours to be trusted
// with the status distinctions.
//
// It is deliberately wider than monochrome. A 16-colour terminal shows colour,
// which is exactly what makes it dangerous here: two outcomes reduce to one
// slot and the result reads as a deliberate sameness rather than as a terminal
// limitation. Sixteen is the last depth at which that happens — the 256-colour
// palette separates every status colour in both themes — so the line is drawn
// there, not at NoColor.
func (t Theme) reducedPalette() bool {
	return t.monochrome() || t.Profile <= render.ANSI16
}

// Logo lends the mark this theme's colours.
//
// The tile keeps the brand's deep red and its near-white knockout on both
// themes rather than following the surface: it is a solid shape, and a mark
// that changed colour with the terminal's background would no longer be the
// same mark as the published asset. Only the name and the tagline take the
// theme's own text colours.
//
// Under the plain theme every colour is Default, which would paint the tile as
// a solid rectangle of the terminal's own foreground. Callers pass
// Theme.LogoBlocks to the renderer for exactly that reason, and it answers no
// there, which drops the mark to its text rung.
func (t Theme) Logo() logo.Colors {
	return logo.Colors{
		Tile:  render.Style{Fg: render.Hex(brand.TileFill), Bg: t.Bg},
		Glyph: render.Style{Fg: render.Hex(brand.TileGlyph), Bg: t.Bg},
		Word:  t.AccentStyle(),
		Tag:   t.Subtle(),
	}
}

// LogoBlocks reports whether the mark can be drawn as blocks at all: it needs
// both the block elements and a palette to draw them in.
func (t Theme) LogoBlocks() bool { return t.Unicode && !t.monochrome() }

// Base is the background style everything is drawn onto.
func (t Theme) Base() render.Style {
	return render.Style{Fg: t.Fg, Bg: t.Bg}
}

// Muted is secondary text.
func (t Theme) Muted() render.Style {
	s := render.Style{Fg: t.FgMuted, Bg: t.Bg}
	if t.monochrome() {
		s.Attrs |= render.Dim
	}
	return s
}

// Subtle is tertiary text: hints, separators, inactive shortcuts.
func (t Theme) Subtle() render.Style {
	s := render.Style{Fg: t.FgSubtle, Bg: t.Bg}
	if t.monochrome() {
		s.Attrs |= render.Dim
	}
	return s
}

// Strong is emphasised text.
func (t Theme) Strong() render.Style {
	return render.Style{Fg: t.Fg, Bg: t.Bg, Attrs: render.Bold}
}

// AccentStyle is brand-coloured text.
func (t Theme) AccentStyle() render.Style {
	return render.Style{Fg: t.Accent, Bg: t.Bg, Attrs: render.Bold}
}

// On returns text drawn on a filled accent background.
func (t Theme) On(fill render.Color) render.Style {
	return render.Style{Fg: t.FgOn, Bg: fill, Attrs: render.Bold}
}

// Status maps a severity token to its style, with an attribute fallback so the
// distinction survives a terminal that cannot carry it in colour.
//
// The fallback is keyed to the resolved colour depth, not to the plain theme.
// Keying it to the theme was the natural-looking mistake and it left the states
// told apart by glyph alone on any real 16-colour terminal: the theme was still
// "dark", still full of distinct hexes, and three of its status colours were
// arriving as the same escape sequence. What decides whether colour can carry a
// distinction is the terminal, so that is what is asked.
func (t Theme) Status(kind StatusKind) render.Style {
	plain := t.reducedPalette()
	switch kind {
	case StatusPass:
		return render.Style{Fg: t.Success, Bg: t.Bg}
	case StatusWarn:
		s := render.Style{Fg: t.Warning, Bg: t.Bg}
		if plain {
			s.Attrs |= render.Bold
		}
		return s
	case StatusBlock:
		s := render.Style{Fg: t.Danger, Bg: t.Bg, Attrs: render.Bold}
		if plain {
			s.Attrs |= render.Reverse
		}
		return s
	case StatusGranted:
		s := render.Style{Fg: t.Granted, Bg: t.Bg}
		if plain {
			s.Attrs |= render.Underline
		}
		return s
	case StatusDegraded:
		s := render.Style{Fg: t.Degraded, Bg: t.Bg}
		if plain {
			s.Attrs |= render.Italic | render.Underline
		}
		return s
	case StatusInfo:
		return render.Style{Fg: t.Info, Bg: t.Bg}
	}
	return t.Base()
}

// StatusKind is the vocabulary of outcomes the UI distinguishes.
//
// The set is closed on purpose, and it has five members rather than two. A pass
// that a grant produced is not a pass; a pass reached without a check running is
// not a pass either. Collapsing them is precisely the failure the harness is
// built to prevent, so the type system is not given the option.
type StatusKind int

// The outcomes.
const (
	StatusNeutral StatusKind = iota
	StatusPass
	StatusWarn
	StatusBlock
	StatusGranted
	StatusDegraded
	StatusInfo
)

// Glyphs are the symbols the UI draws, with an ASCII fallback for terminals
// that cannot render the preferred set.
type Glyphs struct {
	Pass, Block, Warn, Granted, Degraded string
	Tool, Lease, Bullet, Arrow           string
	Collapsed, Expanded                  string
	Spinner                              []string
	VBar, Caret                          string
}

// Glyphs returns the symbol set for this theme.
func (t Theme) Glyphs() Glyphs {
	if !t.Unicode {
		return Glyphs{
			Pass: "ok", Block: "XX", Warn: "!!", Granted: "G>", Degraded: "??",
			Tool: ">>", Lease: "#", Bullet: "*", Arrow: "->",
			Collapsed: "+", Expanded: "-",
			Spinner: render.SpinnerASCII, VBar: "|", Caret: "_",
		}
	}
	return Glyphs{
		Pass: "✓", Block: "⛔", Warn: "⚠", Granted: "◈", Degraded: "◐",
		Tool: "⚙", Lease: "⌁", Bullet: "•", Arrow: "→",
		Collapsed: "▸", Expanded: "▾",
		Spinner: render.SpinnerDots, VBar: "│", Caret: "▏",
	}
}
