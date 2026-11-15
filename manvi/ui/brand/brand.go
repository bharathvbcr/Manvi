// Package brand is the MANVI palette: one dark-red ramp, authored once.
//
// It holds hex strings and nothing else, so the three things that have to agree
// on the brand can all read it without depending on each other: the terminal
// theme (ui/tui), the terminal logo (ui/logo), and the SVG the same logo
// generates for use outside a terminal. A palette copied into each of them is a
// palette that drifts, and the first symptom is a mark that no longer matches
// the interface it names.
//
// # Why the accent is not simply "red"
//
// The status colours are load-bearing here — blocked, granted, and degraded are
// distinctions the whole harness exists to preserve — and Danger is already a
// red. A brand red therefore has to stay separable from it at every colour
// depth, not just in the truecolor terminal it was picked in.
//
// The separation is structural rather than a matter of taste:
//
//   - Danger is a bright vermilion (#f85149, unchanged by this palette). Accent
//     is a deeper, more saturated crimson. They reduce to different xterm-256
//     entries and to different ANSI-16 slots, which ui/tui/theme_test.go
//     asserts by painting both through the real reduction rather than by
//     comparing the hexes — two colours differing in hex says nothing about
//     what arrives at a terminal.
//   - The accent never carries a status. It is used for the logo, focus rings,
//     selection, and the session title; a rule's outcome is always drawn with
//     Theme.Status, never with the brand.
//   - Under NO_COLOR the accent disappears entirely and the status distinctions
//     survive as attributes, which is the existing contract.
//
// # Why the accent is not the ramp's core dark red
//
// AccentDark is Red400 rather than the deeper Red700 the mark is filled with,
// and the reason is contrast rather than taste. Against the dark surface
// Red700 measures 1.78:1 and Red500 2.86:1, both under the 3:1 floor that
// large and bold text is held to; Red400 measures 3.96:1, which
// ui/tui/theme_test.go asserts rather than trusts. So the deep red is used
// where it is a filled shape (the mark's tile, dim borders, the selection
// band) and Red400 is used wherever the colour has to survive as text.
//
// A second reason used to stand here and no longer does: every red darker than
// about #c9384a reduced to bright black on a 16-colour terminal, taking the
// focus ring and the session title with it. That was a defect in the reduction
// — it measured the sixteen by distance in RGB, and grey sits in the middle —
// and it is fixed in render.nearest16, which now decides by hue. The deep reds
// reach a red slot. The contrast floor above is what still rules the choice.
package brand

// The ramp, dark to light. Named by weight in the usual 50-950 convention so a
// new consumer picks a step rather than inventing a hex.
const (
	Red950 = "#2b0810"
	Red900 = "#3f0d18"
	Red800 = "#5c1421"
	Red700 = "#7a1626"
	Red600 = "#94202f"
	Red500 = "#b02636"
	Red400 = "#d92d3f" // the accent on dark surfaces
	Red300 = "#e0616f"
	Red200 = "#f0959e"
	Red100 = "#fbe4e7"
	Red050 = "#fdf2f3"
)

// The semantic picks. A consumer names the role, not the step.
const (
	// AccentDark is the accent on a near-black surface. At 3.7:1 against
	// #0d1117 it clears the 3:1 floor for the bold, large text it is used for
	// and stays visibly cooler than the vermilion of Danger.
	AccentDark = Red400
	// AccentDimDark is the same accent receded: inactive borders, rules.
	AccentDimDark = Red700
	// SelectionDark is a selection fill dark enough to keep body text legible
	// on top of it.
	SelectionDark = Red900

	// AccentLight is the accent on white. The light theme can afford a far
	// deeper red than the dark one — contrast runs the other way there — and
	// it needs one: every mid red reduces to ANSI red on a 16-colour terminal,
	// where this theme's own Danger, Warning, and Granted already sit. Going
	// deeper takes the accent to ANSI black instead, which is the only slot
	// left that no status occupies.
	AccentLight    = Red800
	AccentDimLight = Red300
	SelectionLight = Red100
)

// The plate behind the published mark (ui/logo/svg.go). The mark there is a
// ribbon in the ramp's own reds, so the plate has to be dark for both of its
// tones to read, and it is a wine-tinted near-black rather than a neutral one
// so the plate still belongs to the ramp. These two are the only colours in
// this file that are not steps of it. They were picked by eye against the
// ribbon when the mark was drawn; the ramp's own darkest step, Red950, was not
// tried behind it.
const (
	PlateTop    = "#24131b"
	PlateBottom = "#0c090c"
)

// The terminal logo's own colours. That mark is a folded ribbon, so it needs
// two reds rather than a single accent: one for the legs that catch the light
// and one for the legs in shade. They are the same two on both backgrounds —
// the ribbon is a solid shape, so it does not need to change with the surface
// behind it. The lit face is the accent a dark surface already uses, and the
// shaded face is the deep red that surfaces use for filled shapes.
//
// The glint is the pale red the published mark's lit legs fade to at their
// top. In the terminal it is never a fill: it is the colour a face moves
// toward for a moment while light crosses the ribbon.
const (
	RibbonLit     = Red400
	RibbonShade   = Red700
	RibbonGlint   = Red200
	WordmarkDark  = Red400
	WordmarkLight = Red600
)
