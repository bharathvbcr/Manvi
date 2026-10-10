package main

import (
	"fmt"
	"io"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
	"github.com/bharathvbcr/Manvi/manvi/ui/term"
	"github.com/bharathvbcr/Manvi/manvi/ui/tui"
)

// outputProfile is the colour depth a command's answer may use.
//
// Only a terminal gets colour. A pipe, a file, and the full-screen face's own
// command buffer are not one, and an escape written into any of them is noise
// a script has to strip or text the transcript sanitiser has to defang. The
// check goes through any decorator: the composition root hands commands a
// stdout that remembers a failed write, and asking that value whether it is a
// terminal would answer about the wrapper rather than about the screen.
func outputProfile(out io.Writer) render.Profile {
	if f, ok := ui.TerminalFile(out); ok {
		return term.DetectProfile(f)
	}
	return render.NoColor
}

// outputTheme is the theme a command's answer is drawn with: the full-screen
// face's own, so a report printed here matches what that terminal would show
// in a session.
func outputTheme(profile render.Profile) tui.Theme {
	return tui.PickTheme(profile, term.DetectUnicode())
}

// onTerminal drops a style's background. In the full-screen face every cell
// is painted, so a style carrying the theme's background is correct; printed
// into a terminal whose background is something else, the same style draws
// dark boxes around each line.
func onTerminal(s render.Style) render.Style { return s.Background(render.Default) }

// writeStyled writes lines with escapes for a colour profile and as plain
// text otherwise. render.WriteLines still emits attributes under NoColor,
// which is right for a terminal that asked for no colour and wrong for a pipe.
//
// A failed write is the command's answer lost, and is reported as exactly
// that: wrapped in errOutputLost so the exit status is the documented one, and
// carrying the cause so the composition root's backstop knows it was told.
func writeStyled(out io.Writer, profile render.Profile, lines []render.Line) error {
	var err error
	if profile != render.NoColor {
		err = render.WriteLines(out, profile, lines)
	} else {
		for _, l := range lines {
			if _, err = fmt.Fprintln(out, l.Text()); err != nil {
				break
			}
		}
	}
	if err != nil {
		return fmt.Errorf("%w: %w", errOutputLost, err)
	}
	return nil
}
