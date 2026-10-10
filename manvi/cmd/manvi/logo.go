package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/bharathvbcr/Manvi/manvi/ui/logo"
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

	profile := outputProfile(out)
	th := outputTheme(profile)
	colors := th.Logo()
	colors.Tile = onTerminal(colors.Tile)
	colors.Glyph = onTerminal(colors.Glyph)
	colors.Word = onTerminal(colors.Word)
	colors.Tag = onTerminal(colors.Tag)

	width := logoWidth()
	blocks := th.LogoBlocks()
	lines := logo.Lines(logo.Fit(width, logo.SizeFull.Height(), blocks), colors, blocks,
		"the DevCouncil execution harness", width)
	if len(lines) == 0 {
		return errors.New("the terminal is too narrow to draw the mark")
	}
	return writeStyled(out, profile, lines)
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
