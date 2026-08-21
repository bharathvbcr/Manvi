package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"manvi/ui/logo"
	"manvi/ui/render"
	"manvi/ui/term"
	"manvi/ui/tui"
)

// showLogo prints the mark, or emits it as SVG.
//
// It draws through the same theme, the same profile detection, and the same
// size ladder the full-screen face uses, so what a terminal prints here is what
// that terminal would show at the top of a session. A second rendering path for
// the same mark is a second thing to keep in step.
func showLogo(out io.Writer, args []string) error {
	svg := false
	for _, a := range args {
		switch a {
		case "--svg":
			svg = true
		default:
			return fmt.Errorf("usage: manvi logo [--svg]")
		}
	}
	if svg {
		_, err := io.WriteString(out, logo.SVG(0))
		return err
	}

	profile := render.NoColor
	if f, ok := out.(*os.File); ok {
		profile = term.DetectProfile(f)
	}
	th := tui.PickTheme(profile, term.DetectUnicode())
	colors := th.Logo()
	// Backgrounds are dropped for a stream this command does not own. In the
	// full-screen face every cell is painted, so a style carrying the theme's
	// background is correct; printed into a terminal whose background is
	// something else, the same style draws dark boxes around each line.
	colors.Tile = colors.Tile.Background(render.Default)
	colors.Glyph = colors.Glyph.Background(render.Default)
	colors.Word = colors.Word.Background(render.Default)
	colors.Tag = colors.Tag.Background(render.Default)

	width := logoWidth()
	blocks := th.LogoBlocks()
	lines := logo.Lines(logo.Fit(width, logo.SizeFull.Height(), blocks), colors, blocks,
		"the DevCouncil execution harness", width)
	if len(lines) == 0 {
		return errors.New("the terminal is too narrow to draw the mark")
	}
	if profile == render.NoColor {
		for _, l := range lines {
			if _, err := fmt.Fprintln(out, l.Text()); err != nil {
				return err
			}
		}
		return nil
	}
	return render.WriteLines(out, profile, lines)
}

// logoWidth is how wide the mark may be drawn.
//
// COLUMNS if the shell exports it, and eighty otherwise: this command prints
// and exits rather than taking over the terminal, so it never gets a resize
// event and has no window to query.
func logoWidth() int {
	if v, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && v > 0 {
		return v
	}
	return 80
}
