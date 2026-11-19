package main

import (
	"fmt"
	"io"
	"sync"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/internal/hygiene"
	"github.com/bharathvbcr/Manvi/manvi/mcp"
)

// Bounds on how many refusals each surface prints. A repository chooses how many
// declarations it makes, so a flood of bad ones must not become a flood of
// output; the count that was cut is always said.
const (
	bootRefusalNotices   = 8
	doctorRefusalNotices = 32
)

// mcpRefusalLines renders what the manager refused or could not read, one
// string each, at most limit of them, and returns how many more there were. Every
// string came out of a declaration file or a manifest, so each goes through
// hygiene once more on its way to a terminal; the manager has already escaped
// them, and the escape is idempotent.
func mcpRefusalLines(skipped []mcp.SkippedManifest, limit int) (lines []string, more int) {
	shown := skipped
	if len(shown) > limit {
		shown = shown[:limit]
	}
	for _, s := range shown {
		lines = append(lines, fmt.Sprintf("%s: %s", hygiene.EscapeLine(s.Path), hygiene.EscapeLine(s.Reason)))
	}
	return lines, len(skipped) - len(shown)
}

// reportedRefusals is the refusals this process has already told the operator
// about, by file and reason.
var (
	reportedMu       sync.Mutex
	reportedRefusals = map[string]bool{}
)

// unreported returns the entries not reported before, and marks every entry
// reported: those the notice cuts off are counted in its '+N more' line once, not
// again by every later surface.
func unreported(all []mcp.SkippedManifest) []mcp.SkippedManifest {
	reportedMu.Lock()
	defer reportedMu.Unlock()
	var fresh []mcp.SkippedManifest
	for _, s := range all {
		key := s.Path + "\x00" + s.Reason
		if !reportedRefusals[key] {
			reportedRefusals[key] = true
			fresh = append(fresh, s)
		}
	}
	return fresh
}

// reportMCPRefusals writes one notice per declaration the manager refused to
// offer, then a line counting any it left out, so that a duplicate or empty
// server name is not silent. Skipped had no reader outside tests, which made
// every refusal invisible.
//
// Harness boot runs once per tool surface (every TUI tab, the tool listing, the
// verify critic), and each builds its own manager over the same declarations, so a
// refusal already reported in this process is not reported again.
func reportMCPRefusals(w io.Writer, mgr *mcp.Manager) {
	if mgr == nil || w == nil {
		return
	}
	lines, more := mcpRefusalLines(unreported(mgr.Skipped()), bootRefusalNotices)
	for _, line := range lines {
		_, _ = fmt.Fprintf(w, "manvi: mcp: not offered: %s\n", line)
	}
	if more > 0 {
		_, _ = fmt.Fprintf(w, "manvi: mcp: +%d more declarations not offered; `manvi doctor` lists them\n", more)
	}
}

// reportMCPDoctor is doctor's MCP row: whether the subsystem is on, how many
// servers it will offer, and every declaration it refused, each with its reason.
// A subsystem that could not be built is reported as that, not left out, because
// an omitted row reads as a healthy one.
func reportMCPDoctor(out io.Writer, reg *flags.Registry, root string) {
	mgr, err := buildMCP(reg, root)
	if err != nil {
		_, _ = fmt.Fprintf(out, "  mcp             UNAVAILABLE: %s\n", hygiene.EscapeLine(err.Error()))
		return
	}
	if why := mgr.Unavailable(); why != nil {
		_, _ = fmt.Fprintf(out, "  mcp             off: %s\n", hygiene.EscapeLine(why.Error()))
		return
	}
	skipped := mgr.Skipped()
	_, _ = fmt.Fprintf(out, "  mcp             %d server(s) declared, %d declaration(s) not offered\n", len(mgr.ServerNames()), len(skipped))
	lines, more := mcpRefusalLines(skipped, doctorRefusalNotices)
	for _, line := range lines {
		_, _ = fmt.Fprintf(out, "                  NOT OFFERED %s\n", line)
	}
	if more > 0 {
		_, _ = fmt.Fprintf(out, "                  +%d more not shown\n", more)
	}
}
