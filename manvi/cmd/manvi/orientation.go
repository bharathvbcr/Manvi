package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"manvi/fetch"
	"manvi/mcp"
	"manvi/prompt"
	"manvi/repomap"
	"manvi/tools"
)

// What the harness can actually do, resolved once and handed to the prompt.
//
// The sections these answers gate used to ship unconditionally, and each named
// a capability that is not always there: a code index that needs a binary, an
// index file and an activated tool group; documentation lookup that this
// harness has no first-party way to perform at all. A prompt that instructs
// work the harness cannot do is worse than one that says nothing, because the
// only available form of compliance is to produce something that looks like the
// work — a recalled API presented as a checked one.

// maxOrientationAreas bounds the repository shape put in front of the model.
//
// Twelve. The list exists to answer "where would this live", and the areas that
// answer it are the large ones; a tail of single-file areas is noise paid for
// in tokens on every turn. The cap reports itself, because a list that silently
// stops at twelve reads as a repository with twelve areas.
const maxOrientationAreas = 12

// repoOrientationSection renders the repository's own shape.
//
// Empty when there is no map, which is the honest answer: a section that said
// "this repository has areas" without naming them would be an instruction to go
// looking, which is the thing being replaced.
func repoOrientationSection(areas []repomap.AreaSummary, density prompt.Density) string {
	if len(areas) == 0 {
		return ""
	}
	// Sorted here as well as by the producer, because the heading claims
	// "largest first" and a claim in output the model reads has to be true of
	// whatever the caller passed, not of what one caller happens to pass.
	shown := slices.Clone(areas)
	sort.SliceStable(shown, func(i, j int) bool {
		if shown[i].Files != shown[j].Files {
			return shown[i].Files > shown[j].Files
		}
		return shown[i].Name < shown[j].Name
	})
	var omitted int
	if len(shown) > maxOrientationAreas {
		omitted = len(shown) - maxOrientationAreas
		shown = shown[:maxOrientationAreas]
	}

	var b strings.Builder
	b.WriteString("Repository shape (from the code index, largest first):\n")
	for _, a := range shown {
		fmt.Fprintf(&b, "  %s (%d files)\n", a.Name, a.Files)
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "  … and %d smaller area(s) not listed\n", omitted)
	}
	if density == prompt.DensityFull {
		b.WriteString("Work inside the area a change belongs to, and look there for an existing seam\n" +
			"before adding a new one.")
	}
	return strings.TrimRight(b.String(), "\n")
}

// codeMapAvailable reports whether the code graph can actually be reached this
// turn.
//
// Three independent conditions, all of which have to hold: the navigation tools
// are offered now or can be fetched during the turn, a navigator is configured,
// and the graph itself loaded. Any one of them can be false on its own — an
// index that was never built, a binary that is not installed, a group the model
// has not activated on a surface that starts small — and a prompt that assumes
// all three because one of them is true is the defect this replaces.
func codeMapAvailable(caps harnessCapability, pipeline *tools.Registry, dynamic, coreOnly bool) bool {
	// Deliberately not conditioned on the repository areas. Those come from the
	// code_graph.json artifact and the navigation tools come from the devmap
	// binary and its own index: two artifacts, either of which can be present
	// without the other. An earlier draft required both, which silenced the
	// navigation guidance on a repository that had a working index and no
	// graph — the exact false negative this function exists to avoid.
	if !caps.CodeMapConfigured {
		return false
	}
	if pipeline == nil {
		return false
	}
	if dynamic {
		// The group is Extended, so it is not offered at the start of a
		// dynamic turn — but the model has search_tools and activate_tools and
		// can fetch it, and tool-discovery tells it so. The capability is real;
		// only its immediacy is not.
		return true
	}
	// The profile the model is actually handed, not the registry behind it.
	// core_tools_only narrows the offered schemas without touching the group
	// list, so asking which groups are "active" answers a different question
	// than the one that matters — and answering it produced a prompt that named
	// the graph tools to a model that had been given neither.
	offered := pipeline.Schemas()
	if coreOnly {
		offered = pipeline.CoreSchemas()
	}
	for _, schema := range offered {
		if schema.Name == graphQueryTool {
			return true
		}
	}
	return false
}

// graphQueryTool is the code graph's entry point, and the tool whose presence
// decides whether the prompt may talk about navigating by the graph.
const graphQueryTool = "devcouncil_graph_query"

// docLookupAvailable reports whether anything attached to this harness can
// fetch documentation.
//
// Two ways it can be true, and the difference matters to nobody downstream: the
// prompt section this gates says "check the documentation", not "call this
// particular tool".
//
// The first-party fetcher is the stronger of the two, and it is the one whose
// egress this harness actually controls. The second is an MCP server, where a
// registered server is the weakest claim that is still true — it may offer no
// documentation tool at all, and the model finds that out cheaply. A registered
// server is asked for rather than the mcp_* tools being counted: those are on
// the surface whenever a manager exists, including a disabled one that exists
// precisely so it can refuse.
func docLookupAvailable(client *fetch.Client, mgr *mcp.Manager) bool {
	if client.Enabled() {
		return true
	}
	if mgr == nil || !mgr.Enabled() {
		return false
	}
	return len(mgr.ServerNames()) > 0
}
