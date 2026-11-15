package logo

import (
	"fmt"
	"slices"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/ui/brand"
)

// The published mark: one ribbon folded four times into an M, on a dark plate.
//
// The measurements in the const block below are the one place the shape is
// decided. This file fills the ribbon's legs as polygons; cells.go samples the
// same legs into character cells for the terminal, so the two renderings
// cannot be edited apart. Every coordinate is computed from those
// measurements, on a 320-unit square chosen so that all of them are whole
// numbers, and verify.sh regenerates assets/manvi-mark.svg and fails if the
// checked-in file has drifted, which is the only way a generated asset stays
// generated.
const (
	// svgSide is the icon's edge in user units, and svgCorner its radius.
	svgSide   = 320
	svgCorner = 100

	// The ribbon. It runs through foldLegs+1 evenly spaced columns, alternately
	// on the baseline and at the top, so foldLegs legs make the two peaks of
	// the M. foldHalf is half the ribbon's width measured along the baseline:
	// the ends are cut flat, which is what makes it read as folded card rather
	// than as a pen stroke.
	foldLegs   = 4
	foldFirst  = 76
	foldStep   = 42
	foldTop    = 97
	foldBottom = 223
	foldHalf   = 18
)

type point struct{ x, y int }

// foldColumn is the centre of the i-th fold, and foldOnBaseline whether that
// fold sits on the baseline (the even ones) or at the top.
func foldColumn(i int) int      { return foldFirst + i*foldStep }
func foldOnBaseline(i int) bool { return i%2 == 0 }

// foldNotch is how far a fold's inner corner sits from the edge it points
// away from: the height at which the inner edges of two neighbouring legs
// meet. The measurements are chosen so the division is exact, and the tests
// hold that, because a rounded notch would put a visible step in a straight
// edge.
func foldNotch() int {
	return (foldStep - foldHalf) * (foldBottom - foldTop) / foldStep
}

// foldLeg is the parallelogram between fold i and fold i+1.
func foldLeg(i int) []point {
	y0, y1 := foldBottom, foldTop
	if !foldOnBaseline(i) {
		y0, y1 = foldTop, foldBottom
	}
	a, b := foldColumn(i), foldColumn(i+1)
	return []point{{a - foldHalf, y0}, {a + foldHalf, y0}, {b + foldHalf, y1}, {b - foldHalf, y1}}
}

// foldOutline is the silhouette of every leg together, as one polygon: along
// the top from left to right, then back along the underside.
func foldOutline() []point {
	notch := foldNotch()
	upper := []point{{foldColumn(0) - foldHalf, foldBottom}}
	var under []point
	for i := range foldLegs + 1 {
		x := foldColumn(i)
		switch {
		case !foldOnBaseline(i):
			upper = append(upper, point{x - foldHalf, foldTop}, point{x + foldHalf, foldTop})
			under = append(under, point{x, foldBottom - notch})
		case i == 0:
			under = append(under, point{x + foldHalf, foldBottom})
		case i == foldLegs:
			upper = append(upper, point{x + foldHalf, foldBottom})
			under = append(under, point{x - foldHalf, foldBottom})
		default:
			upper = append(upper, point{x, foldTop + notch})
			under = append(under, point{x - foldHalf, foldBottom}, point{x + foldHalf, foldBottom})
		}
	}
	for _, p := range slices.Backward(under) {
		upper = append(upper, p)
	}
	return upper
}

// pathData writes closed polygons as one SVG path.
func pathData(polygons ...[]point) string {
	var b strings.Builder
	for _, poly := range polygons {
		for i, p := range poly {
			cmd := "L"
			if i == 0 {
				cmd = "M"
			}
			fmt.Fprintf(&b, "%s%d %d", cmd, p.x, p.y)
		}
		b.WriteString("Z")
	}
	return b.String()
}

// SVG returns the mark as a self-contained SVG.
//
// It uses gradients and nothing rarer: no mask, no filter, no font. A mask
// renders differently across viewers that support only part of SVG 1.1, and
// this mark has to survive being pasted into a README, a badge, and a favicon.
//
// The shaded legs are not drawn as shapes of their own. The whole silhouette
// is filled in the shade first and the lit legs are laid over it, so the folds
// are edges between two fills of one path rather than seams between
// neighbouring shapes, where a renderer would let the plate show through.
func SVG() string {
	var lit [][]point
	for i := range foldLegs {
		if foldOnBaseline(i) {
			lit = append(lit, foldLeg(i))
		}
	}
	// The lit gradient runs a little past the ribbon at both ends, so neither
	// extreme of the ramp lands exactly on an edge.
	gradTop, gradBottom := foldTop-7, foldBottom+7
	mid := svgSide / 2

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" role="img" aria-labelledby="manvi-title">`,
		svgSide, svgSide, svgSide, svgSide)
	b.WriteString("\n  <title id=\"manvi-title\">MANVI</title>\n  <defs>\n")
	fmt.Fprintf(&b, `    <linearGradient id="manvi-plate" x1="0" y1="0" x2="%d" y2="%d" gradientUnits="userSpaceOnUse"><stop offset="0" stop-color="%s"/><stop offset="1" stop-color="%s"/></linearGradient>`+"\n",
		svgSide, svgSide, brand.PlateTop, brand.PlateBottom)
	fmt.Fprintf(&b, `    <radialGradient id="manvi-glow" cx="%d" cy="%d" r="%d" gradientUnits="userSpaceOnUse"><stop offset="0" stop-color="%s" stop-opacity="0.3"/><stop offset="1" stop-color="%s" stop-opacity="0"/></radialGradient>`+"\n",
		mid, mid, mid-10, brand.Red400, brand.Red400)
	fmt.Fprintf(&b, `    <linearGradient id="manvi-lit" x1="%d" y1="%d" x2="%d" y2="%d" gradientUnits="userSpaceOnUse"><stop offset="0" stop-color="%s"/><stop offset="0.45" stop-color="%s"/><stop offset="1" stop-color="%s"/></linearGradient>`+"\n",
		mid, gradTop, mid, gradBottom, brand.Red200, brand.Red300, brand.Red400)
	fmt.Fprintf(&b, `    <linearGradient id="manvi-shade" x1="%d" y1="%d" x2="%d" y2="%d" gradientUnits="userSpaceOnUse"><stop offset="0" stop-color="%s"/><stop offset="1" stop-color="%s"/></linearGradient>`+"\n",
		mid, gradTop, mid, gradBottom, brand.Red500, brand.Red700)
	b.WriteString("  </defs>\n")

	// The plate, the light behind the mark, and a hairline that keeps the
	// plate's edge from vanishing into a dark page.
	fmt.Fprintf(&b, `  <rect width="%d" height="%d" rx="%d" fill="url(#manvi-plate)"/>`+"\n", svgSide, svgSide, svgCorner)
	fmt.Fprintf(&b, `  <rect width="%d" height="%d" rx="%d" fill="url(#manvi-glow)"/>`+"\n", svgSide, svgSide, svgCorner)
	fmt.Fprintf(&b, `  <rect x="3" y="3" width="%d" height="%d" rx="%d" fill="none" stroke="#ffffff" stroke-opacity="0.1" stroke-width="6"/>`+"\n",
		svgSide-6, svgSide-6, svgCorner-3)

	fmt.Fprintf(&b, `  <path d="%s" fill="url(#manvi-shade)"/>`+"\n", pathData(foldOutline()))
	fmt.Fprintf(&b, `  <path d="%s" fill="url(#manvi-lit)"/>`+"\n", pathData(lit...))
	b.WriteString("</svg>\n")
	return b.String()
}
