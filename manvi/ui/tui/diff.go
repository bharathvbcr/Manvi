package tui

import (
	"fmt"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// codeBlock represents one fenced code block extracted from Markdown prose.
type codeBlock struct {
	Index int
	Lang  string
	Code  string
}

// extractCodeBlocks returns every fenced code block in text, in order.
func extractCodeBlocks(text string) []codeBlock {
	var blocks []codeBlock
	inFence := false
	lang := ""
	var cur []string
	for _, raw := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, "```") {
			if !inFence {
				inFence = true
				lang = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, "```")))
				cur = cur[:0]
			} else {
				blocks = append(blocks, codeBlock{
					Index: len(blocks) + 1,
					Lang:  lang,
					Code:  strings.Join(cur, "\n"),
				})
				inFence = false
				lang = ""
				cur = cur[:0]
			}
			continue
		}
		if inFence {
			cur = append(cur, raw)
		}
	}
	return blocks
}

// fileEdit summarizes a file modification recorded in a tool call's arguments.
type fileEdit struct {
	Path    string
	Tool    string
	Added   int
	Removed int
	Before  string
	After   string
	Entry   int
}

// extractFileEdit extracts a structured edit from a successful patch_file or
// write_file tool call event, or returns ok=false if the event is not a file edit.
func extractFileEdit(ev ui.Event, entryIdx int) (fileEdit, bool) {
	name := strings.TrimPrefix(ev.Tool, toolPrefix)
	fields := callArgs(ev)
	if len(fields) == 0 {
		return fileEdit{}, false
	}
	path, _ := fields["path"].(string)
	if path == "" {
		path, _ = fields["file_path"].(string)
	}
	if path == "" {
		return fileEdit{}, false
	}
	switch name {
	case "patch_file":
		before, okB := fields["target_content"].(string)
		after, okA := fields["replacement_content"].(string)
		if !okB || !okA {
			return fileEdit{}, false
		}
		added, removed, _ := editSize(fields)
		return fileEdit{
			Path: path, Tool: name, Added: added, Removed: removed,
			Before: before, After: after, Entry: entryIdx,
		}, true
	case "write_file":
		content, ok := fields["content"].(string)
		if !ok {
			return fileEdit{}, false
		}
		lines := 0
		if content != "" {
			lines = strings.Count(strings.TrimRight(content, "\n"), "\n") + 1
		}
		return fileEdit{
			Path: path, Tool: name, Added: lines,
			Before: "", After: content, Entry: entryIdx,
		}, true
	}
	return fileEdit{}, false
}

// diffOp is a single line operation in a synthesized diff.
type diffOp struct {
	kind byte // ' ', '-', '+'
	text string
	oldN int
	newN int
}

// computeLineDiff produces a line-level diff between before and after using
// common-prefix/suffix trimming + bounded LCS so large edits stay fast.
func computeLineDiff(before, after string) []diffOp {
	split := func(s string) []string {
		if s == "" {
			return nil
		}
		return strings.Split(strings.TrimRight(s, "\n"), "\n")
	}
	a, b := split(before), split(after)
	if len(a) == 0 && len(b) == 0 {
		return nil
	}

	// Trim shared prefix and suffix first (common in patch_file context).
	pref := 0
	for pref < len(a) && pref < len(b) && a[pref] == b[pref] {
		pref++
	}
	aEnd, bEnd := len(a), len(b)
	for aEnd > pref && bEnd > pref && a[aEnd-1] == b[bEnd-1] {
		aEnd--
		bEnd--
	}

	var ops []diffOp
	// Keep up to 2 lines of leading context.
	ctxStart := pref - 2
	if ctxStart < 0 {
		ctxStart = 0
	}
	for i := ctxStart; i < pref; i++ {
		ops = append(ops, diffOp{kind: ' ', text: a[i], oldN: i + 1, newN: i + 1})
	}

	midA, midB := a[pref:aEnd], b[pref:bEnd]
	// For moderate middle spans use LCS; for huge blocks emit delete+insert cleanly.
	if len(midA)*len(midB) <= 40000 && len(midA) > 0 && len(midB) > 0 {
		ops = append(ops, lcsDiff(midA, midB, pref+1, pref+1)...)
	} else {
		for i, l := range midA {
			ops = append(ops, diffOp{kind: '-', text: l, oldN: pref + i + 1})
		}
		for i, l := range midB {
			ops = append(ops, diffOp{kind: '+', text: l, newN: pref + i + 1})
		}
	}

	// Keep up to 2 lines of trailing context.
	tailCount := len(a) - aEnd
	if tailCount > 2 {
		tailCount = 2
	}
	for i := 0; i < tailCount; i++ {
		ops = append(ops, diffOp{
			kind: ' ',
			text: a[aEnd+i],
			oldN: aEnd + i + 1,
			newN: bEnd + i + 1,
		})
	}
	return ops
}

func lcsDiff(a, b []string, oldBase, newBase int) []diffOp {
	m, n := len(a), len(b)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < m && j < n {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{kind: ' ', text: a[i], oldN: oldBase + i, newN: newBase + j})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffOp{kind: '-', text: a[i], oldN: oldBase + i})
			i++
		default:
			ops = append(ops, diffOp{kind: '+', text: b[j], newN: newBase + j})
			j++
		}
	}
	for ; i < m; i++ {
		ops = append(ops, diffOp{kind: '-', text: a[i], oldN: oldBase + i})
	}
	for ; j < n; j++ {
		ops = append(ops, diffOp{kind: '+', text: b[j], newN: newBase + j})
	}
	return ops
}

// formatUnifiedDiffText produces standard unified diff text suitable for copying.
func formatUnifiedDiffText(ed fileEdit) string {
	ops := computeLineDiff(ed.Before, ed.After)
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", ed.Path, ed.Path)
	for _, op := range ops {
		b.WriteByte(op.kind)
		b.WriteString(op.text)
		b.WriteByte('\n')
	}
	return b.String()
}

// renderEditDiff renders a fileEdit as either a clean side-by-side Before/After
// diff (when width >= 110 and both sides exist) or a crisp unified diff with
// hairline file header and line numbers.
func renderEditDiff(ed fileEdit, th Theme, width int, maxLines int) []render.Line {
	ops := computeLineDiff(ed.Before, ed.After)
	if len(ops) == 0 {
		return nil
	}
	if width < 20 {
		width = 20
	}
	g := th.Glyphs()
	headerBar := "─"
	if !th.Unicode {
		headerBar = "-"
	}
	hdr := render.Styled(headerBar+" diff ", th.Subtle()).
		Append(ed.Path, th.Status(StatusInfo))
	if ed.Added > 0 {
		hdr = hdr.Append("  +"+groupDigits(ed.Added), render.Style{Fg: th.Success, Bg: th.Bg})
	}
	if ed.Removed > 0 {
		hdr = hdr.Append("  -"+groupDigits(ed.Removed), render.Style{Fg: th.Danger, Bg: th.Bg})
	}
	if fill := width - hdr.Width() - 1; fill > 2 {
		hdr = hdr.Append(" "+strings.Repeat(headerBar, fill), render.Style{Fg: th.ruleColor(), Bg: th.Bg})
	}
	out := []render.Line{hdr.Truncate(width)}

	// Side-by-side mode on wide viewports when there are both deletions and additions.
	if width >= 110 && ed.Before != "" && ed.After != "" {
		out = append(out, renderSideBySide(ops, th, width, maxLines)...)
		return out
	}

	// Unified diff with line numbers.
	rule := render.Styled(g.VBar+" ", render.Style{Fg: th.ruleColor(), Bg: th.Bg})
	bodyW := width - rule.Width()
	if bodyW < 10 {
		bodyW = 10
	}
	limit := len(ops)
	clipped := 0
	if maxLines > 0 && limit > maxLines {
		clipped = limit - maxLines
		limit = maxLines
	}
	for _, op := range ops[:limit] {
		st := th.Muted()
		prefix := "  "
		switch op.kind {
		case '+':
			st = render.Style{Fg: th.Success, Bg: th.Bg}
			prefix = "+ "
		case '-':
			st = render.Style{Fg: th.Danger, Bg: th.Bg}
			prefix = "- "
		}
		txt := expandTabs(op.text)
		line := rule.Append(prefix, st).Append(txt, st)
		out = append(out, line.Truncate(width))
	}
	if clipped > 0 {
		out = append(out, rule.Append(fmt.Sprintf("… +%d more diff lines", clipped), th.Subtle()))
	}
	return out
}

// renderSideBySide pairs deleted and added runs side by side for wide terminals.
func renderSideBySide(ops []diffOp, th Theme, width int, maxLines int) []render.Line {
	sep := " │ "
	if !th.Unicode {
		sep = " | "
	}
	colW := (width - len(sep)) / 2
	if colW < 20 {
		colW = 20
	}
	sepStyled := render.Styled(sep, render.Style{Fg: th.ruleColor(), Bg: th.Bg})
	delStyle := render.Style{Fg: th.Danger, Bg: th.Bg}
	addStyle := render.Style{Fg: th.Success, Bg: th.Bg}
	ctxStyle := th.Muted()

	type pair struct {
		left, right render.Line
	}
	var pairs []pair
	i := 0
	for i < len(ops) {
		if ops[i].kind == ' ' {
			txt := expandTabs(ops[i].text)
			l := render.Styled(fmt.Sprintf("%3d   %s", ops[i].oldN, txt), ctxStyle)
			r := render.Styled(fmt.Sprintf("%3d   %s", ops[i].newN, txt), ctxStyle)
			pairs = append(pairs, pair{left: l, right: r})
			i++
			continue
		}
		var dels, adds []diffOp
		for i < len(ops) && ops[i].kind != ' ' {
			if ops[i].kind == '-' {
				dels = append(dels, ops[i])
			} else {
				adds = append(adds, ops[i])
			}
			i++
		}
		n := len(dels)
		if len(adds) > n {
			n = len(adds)
		}
		for k := 0; k < n; k++ {
			var l, r render.Line
			if k < len(dels) {
				l = render.Styled(fmt.Sprintf("%3d - %s", dels[k].oldN, expandTabs(dels[k].text)), delStyle)
			}
			if k < len(adds) {
				r = render.Styled(fmt.Sprintf("%3d + %s", adds[k].newN, expandTabs(adds[k].text)), addStyle)
			}
			pairs = append(pairs, pair{left: l, right: r})
		}
	}

	out := make([]render.Line, 0, len(pairs)+2)
	out = append(out,
		render.Styled("BEFORE", th.Subtle()).Pad(colW, th.Base()).
			Concat(sepStyled).
			Append("AFTER", th.Subtle()).Truncate(width))

	limit := len(pairs)
	clipped := 0
	if maxLines > 0 && limit > maxLines {
		clipped = limit - maxLines
		limit = maxLines
	}
	for _, p := range pairs[:limit] {
		row := p.left.Truncate(colW).Pad(colW, th.Base()).
			Concat(sepStyled).
			Concat(p.right.Truncate(colW))
		out = append(out, row.Truncate(width))
	}
	if clipped > 0 {
		out = append(out, render.Styled(fmt.Sprintf("… +%d more diff rows", clipped), th.Subtle()))
	}
	return out
}
