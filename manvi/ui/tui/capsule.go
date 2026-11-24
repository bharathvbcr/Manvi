package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// workCapsule summarizes a contiguous run of intermediate work entries
// (thoughts, tool calls, tool results, policy decisions, sub-agent activity)
// inside a single turn so they collapse into one clean, expandable row.
//
// The harness's policy-honesty invariant is never relaxed here: any entry
// inside the run that failed, fired a policy rule, leaned on a grant, or ran
// with degraded/weakened checks stays pinned visibly beneath the collapsed
// capsule row.
type workCapsule struct {
	start int // first entry index in Scrollback.entries (inclusive)
	end   int // last entry index (exclusive)

	thoughts  int
	reads     int
	edits     int
	runs      int
	searches  int
	other     int
	added     int
	removed   int
	subagents []string

	running      bool
	runningLabel string
	duration     time.Duration

	// pinned holds indices of entries whose status must remain visible even
	// when the capsule is collapsed (blocks, failures, grants, degraded, warns).
	pinned []int
}

func isSafetyPinnedStatus(st StatusKind) bool {
	return st == StatusBlock || st == StatusGranted || st == StatusDegraded || st == StatusWarn
}

func isWorkEntry(k ui.Kind) bool {
	switch k {
	case ui.KindReasoning, ui.KindToolStart, ui.KindToolResult,
		ui.KindPolicy, ui.KindApproval, ui.KindApprovalDone:
		return true
	}
	return false
}

// buildWorkCapsule inspects entries[start:end] and computes its summary stats
// and safety-pinned rows.
func buildWorkCapsule(entries []*Entry, start, end int) workCapsule {
	cap := workCapsule{start: start, end: end}
	var firstAt, lastAt time.Time
	subSeen := map[string]bool{}

	for i := start; i < end; i++ {
		e := entries[i]
		ev := e.Event
		if !ev.At.IsZero() {
			if firstAt.IsZero() || ev.At.Before(firstAt) {
				firstAt = ev.At
			}
			t := e.until
			if t.IsZero() {
				t = ev.At
			}
			if t.After(lastAt) {
				lastAt = t
			}
		}
		if ev.Agent != "" && !subSeen[ev.Agent] {
			subSeen[ev.Agent] = true
			cap.subagents = append(cap.subagents, ev.Agent)
		}

		// Pin any entry with a non-routine safety/error outcome so collapsing
		// can never hide a block, failure, grant, demotion, or unrun check.
		if !e.absorbed && isSafetyPinnedStatus(e.Status()) {
			cap.pinned = append(cap.pinned, i)
		}

		switch ev.Kind {
		case ui.KindReasoning:
			cap.thoughts++
			if e.live {
				cap.running = true
				cap.runningLabel = "Thinking"
			}
		case ui.KindToolStart:
			if e.result == nil {
				cap.running = true
				cap.runningLabel = doingPhrase(ev)
			}
			cap.countTool(ev, e.result != nil && !e.result.Event.IsError)
		case ui.KindToolResult:
			// Unpaired tool result (no ToolStart absorbed into it).
			if e.call == nil && !e.under {
				cap.other++
			}
		}
	}
	if !firstAt.IsZero() && lastAt.After(firstAt) {
		cap.duration = lastAt.Sub(firstAt)
	}
	return cap
}

func (c *workCapsule) countTool(ev ui.Event, succeeded bool) {
	name := strings.TrimPrefix(ev.Tool, toolPrefix)
	switch name {
	case "read_file", "list_dir", "get_diff", "graph_context":
		c.reads++
	case "patch_file", "write_file", "delete_file":
		c.edits++
		if succeeded {
			fields := callArgs(ev)
			switch name {
			case "patch_file":
				if a, r, ok := editSize(fields); ok {
					c.added += a
					c.removed += r
				}
			case "write_file":
				if content, _ := fields["content"].(string); content != "" {
					c.added += strings.Count(strings.TrimRight(content, "\n"), "\n") + 1
				}
			}
		}
	case "exec_command":
		c.runs++
	case "grep", "find_files", "graph_query", "search_tools":
		c.searches++
	case "spawn_subagents", "invoke_subagent":
		// Subagents are counted in c.subagents; keep tool count clean if none forwarded yet.
		if len(c.subagents) == 0 {
			c.other++
		}
	default:
		if strings.HasPrefix(name, "git_") {
			c.runs++
		} else {
			c.other++
		}
	}
}

// visibleCount reports how many non-absorbed entries this capsule represents.
func (c *workCapsule) visibleCount(entries []*Entry) int {
	n := 0
	for i := c.start; i < c.end; i++ {
		if !entries[i].absorbed {
			n++
		}
	}
	return n
}

// headerLine renders the single-row capsule header with disclosure chevron,
// concise activity counts, diff totals, and an expand hint.
func (c *workCapsule) headerLine(th Theme, width int, expanded bool) render.Line {
	g := th.Glyphs()
	chev := g.Collapsed
	if expanded {
		chev = g.Expanded
	}
	chevStyle := th.Subtle()
	if c.running {
		chev = g.Tool
		chevStyle = render.Style{Fg: th.Accent, Bg: th.Bg, Attrs: render.Bold}
	}

	line := render.Styled(chev+"  ", chevStyle)
	verb := "Worked"
	if c.running {
		verb = "Working"
		if c.runningLabel != "" {
			verb = c.runningLabel
		}
	}
	if !c.running && c.duration >= time.Second {
		verb += " " + shortDuration(c.duration)
	}
	line = line.Append(verb, th.Muted().With(render.Bold))

	var parts []string
	plural := func(n int, sing, plur string) string {
		if n == 1 {
			return "1 " + sing
		}
		return fmt.Sprintf("%d %s", n, plur)
	}
	if c.thoughts > 0 {
		parts = append(parts, plural(c.thoughts, "thought", "thoughts"))
	}
	if c.reads > 0 {
		parts = append(parts, plural(c.reads, "read", "reads"))
	}
	if c.searches > 0 {
		parts = append(parts, plural(c.searches, "search", "searches"))
	}
	if c.edits > 0 {
		parts = append(parts, plural(c.edits, "edit", "edits"))
	}
	if c.runs > 0 {
		parts = append(parts, plural(c.runs, "cmd", "cmds"))
	}
	if len(c.subagents) > 0 {
		parts = append(parts, plural(len(c.subagents), "subagent", "subagents"))
	}
	if c.other > 0 {
		parts = append(parts, plural(c.other, "tool", "tools"))
	}

	dot := " " + g.Dot + " "
	for _, p := range parts {
		line = line.Append(dot, th.Subtle()).Append(p, th.Muted())
	}
	if c.added > 0 || c.removed > 0 {
		line = line.Append(" (", th.Subtle())
		if c.added > 0 {
			line = line.Append("+"+groupDigits(c.added), render.Style{Fg: th.Success, Bg: th.Bg})
		}
		if c.added > 0 && c.removed > 0 {
			line = line.Append(" ", th.Subtle())
		}
		if c.removed > 0 {
			line = line.Append("-"+groupDigits(c.removed), render.Style{Fg: th.Danger, Bg: th.Bg})
		}
		line = line.Append(")", th.Subtle())
	}

	hint := "[Enter]"
	if expanded {
		hint = "[collapse]"
	}
	hintW := render.StringWidth(hint)
	if width >= 40 && line.Width()+2+hintW <= width {
		line = line.Pad(width-hintW, th.Base()).Append(hint, th.Subtle())
	}
	return line.Truncate(width)
}
