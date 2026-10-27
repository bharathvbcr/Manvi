package tui

import (
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// mdOracle is mdState.line as it was before the classification moved into
// mdState.kind: the same branches written out in the same order, in the test
// package only, as the oracle the refactor is held to. It carries the same
// state.
type mdOracle struct {
	inCode bool
	lang   string
}

// line renders one raw line, without its break, appending its rows to out.
func (st *mdOracle) line(out []render.Line, raw string, th Theme, width int) []render.Line {
	trimmed := strings.TrimSpace(raw)

	if strings.HasPrefix(trimmed, "```") {
		if !st.inCode {
			st.inCode = true
			st.lang = strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
			if st.lang == "" {
				st.lang = "code"
			}
			hdr := "┌─ " + st.lang + " "
			topLine := render.Styled(hdr, render.Style{Fg: th.BorderFocus, Bg: th.BgInset, Attrs: render.Bold})
			if topLine.Width() < width {
				topLine = topLine.Append(strings.Repeat("─", width-topLine.Width()), render.Style{Fg: th.Border, Bg: th.BgInset})
			}
			return append(out, topLine.Truncate(width))
		}
		st.inCode = false
		botLine := render.Styled("└"+strings.Repeat("─", width-1), render.Style{Fg: th.Border, Bg: th.BgInset})
		return append(out, botLine.Truncate(width))
	}

	if st.inCode {
		codeLine := highlightCodeLine(raw, th, st.lang)
		if codeLine.Width() < width {
			codeLine = codeLine.Pad(width, render.Style{Bg: th.BgInset})
		}
		return append(out, codeLine.Truncate(width))
	}

	if strings.HasPrefix(raw, "diff --git ") || strings.HasPrefix(raw, "index ") || strings.HasPrefix(raw, "--- ") || strings.HasPrefix(raw, "+++ ") {
		return append(out, render.Styled(raw, render.Style{Fg: th.FgMuted, Bg: th.BgInset, Attrs: render.Bold}).Truncate(width))
	}
	if strings.HasPrefix(raw, "@@") {
		return append(out, render.Styled(raw, render.Style{Fg: th.Info, Bg: th.BgInset, Attrs: render.Bold}).Truncate(width))
	}
	if strings.HasPrefix(raw, "+") && len(raw) > 1 && raw[1] != '+' {
		diffLine := render.Styled("+ ", render.Style{Fg: th.Success, Bg: th.BgInset, Attrs: render.Bold}).
			Append(raw[1:], render.Style{Fg: th.Success, Bg: th.BgInset})
		return append(out, diffLine.Truncate(width))
	}
	if strings.HasPrefix(raw, "-") && len(raw) > 1 && raw[1] != '-' {
		diffLine := render.Styled("- ", render.Style{Fg: th.Danger, Bg: th.BgInset, Attrs: render.Bold}).
			Append(raw[1:], render.Style{Fg: th.Danger, Bg: th.BgInset})
		return append(out, diffLine.Truncate(width))
	}

	if strings.HasPrefix(raw, "# ") {
		heading := strings.TrimPrefix(raw, "# ")
		return append(out, render.Styled("■ "+heading, render.Style{Fg: th.Accent, Bg: th.Bg, Attrs: render.Bold}).Truncate(width))
	}
	if strings.HasPrefix(raw, "## ") {
		heading := strings.TrimPrefix(raw, "## ")
		return append(out, render.Styled("◆ "+heading, render.Style{Fg: th.Fg, Bg: th.Bg, Attrs: render.Bold}).Truncate(width))
	}
	if strings.HasPrefix(raw, "### ") {
		heading := strings.TrimPrefix(raw, "### ")
		return append(out, render.Styled("▸ "+heading, render.Style{Fg: th.FgMuted, Bg: th.Bg, Attrs: render.Bold}).Truncate(width))
	}

	if strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "- ") {
		itemText := strings.TrimSpace(trimmed[2:])
		bullet := render.Styled("  "+th.Glyphs().Bullet+" ", th.Status(StatusInfo))
		itemLine := bullet.Concat(parseInlineSpans(itemText, th, th.Base()))
		return append(out, itemLine.Wrap(width)...)
	}

	return append(out, parseInlineSpans(raw, th, th.Base()).Wrap(width)...)
}
