package logo

import (
	"fmt"
	"strings"

	"manvi/ui/brand"
)

// SVG returns the mark as a self-contained SVG, generated from the same grid
// the terminal draws.
//
// module is the edge of one grid module in user units; the whole icon is seven
// of them square. Nothing here is hand-drawn and nothing references a font, so
// the asset in assets/ is reproducible — verify.sh regenerates it and fails if
// the checked-in file has drifted, which is the only way a generated asset
// stays generated.
//
// The knockout is drawn as modules in the glyph colour rather than as a mask,
// because a mask renders differently across viewers that support only part of
// SVG 1.1, and this mark has to survive being pasted into a README, a badge,
// and a favicon.
func SVG(module int) string {
	if module <= 0 {
		module = 12
	}
	side := markCols * module

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" role="img" aria-labelledby="t">`,
		side, side, side, side)
	b.WriteString("\n  <title id=\"t\">MANVI</title>\n")
	// The tile. Its corner radius is one module, so the rounding is part of the
	// grid rather than a separate decision that would have to be re-made at
	// every size.
	fmt.Fprintf(&b, `  <rect width="%d" height="%d" rx="%d" fill="%s"/>`+"\n",
		side, side, module, brand.TileFill)

	// The knockout, one rect per run of modules so the output is a handful of
	// shapes rather than one per cell.
	for y, row := range mark {
		x := 0
		for x < len(row) {
			if row[x] != '#' {
				x++
				continue
			}
			run := 0
			for x+run < len(row) && row[x+run] == '#' {
				run++
			}
			fmt.Fprintf(&b, `  <rect x="%d" y="%d" width="%d" height="%d" fill="%s"/>`+"\n",
				x*module, y*module, run*module, module, brand.TileGlyph)
			x += run
		}
	}
	b.WriteString("</svg>\n")
	return b.String()
}
