package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
)

// --- map ---

// mapCommand builds or reports the repo-navigation index.
func mapCommand(out io.Writer, reg *flags.Registry, args []string) error {
	client := mapClient(projectRoot())
	ctx := context.Background()

	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "build":
		return mapBuild(out, client, ctx)
	case "rebuild":
		// The remedy for an index whose analysis cannot be trusted.
		//
		// devmap builds incrementally, and an index committed by an older
		// binary keeps whatever that binary concluded: a build that finds the
		// tree unchanged re-derives nothing, so a wrong dead-code or subsystem
		// result survives every subsequent build. Nothing can detect that from
		// outside — recomputing is the only way to know — so the operator is
		// given the way to discard it rather than being told to delete files by
		// hand. Only the derived index goes; the artifacts are rewritten from
		// the rebuild that follows.
		if err := os.RemoveAll(indexDir()); err != nil {
			return fmt.Errorf("discarding the index at %s: %w", indexDir(), err)
		}
		_, _ = fmt.Fprintf(out, "discarded %s; rebuilding from nothing\n", indexDir())
		return mapBuild(out, client, ctx)
	case "status", "":
		return mapStatus(out, client, ctx, nil)
	default:
		return fmt.Errorf("unknown map subcommand %q (build, rebuild, status)", sub)
	}
}

// mapBuild rebuilds the index and reports what it did and did not cover.
func mapBuild(out io.Writer, client *devmap.Client, ctx context.Context) error {
	report, err := refreshIndex(ctx, client)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, describeIndex(report))
	// Kept so the status report below does not repeat what this one just said.
	// The two describe the same artifact from two directions — what the build
	// wrote, and what is on disk now — and a fact stated twice in one command's
	// output reads as a rendering bug rather than as emphasis.
	said := map[string]bool{}
	for _, line := range report.Degraded() {
		said[line] = true
		_, _ = fmt.Fprintf(out, "                %s\n", line)
	}
	_, _ = fmt.Fprintf(out, "wrote %s\n", graphArtifactPath())
	return mapStatus(out, client, ctx, said)
}

// said names lines a caller has already printed, so one command does not report
// the same fact twice. Nil when nothing has been printed yet.
func mapStatus(out io.Writer, client *devmap.Client, ctx context.Context, said map[string]bool) error {
	_, _ = fmt.Fprintln(out)
	status, err := client.Available(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(out, "  index         UNAVAILABLE: %v\n", err)
		_, _ = fmt.Fprintln(out, "                the neighbour rule cannot run; unplanned writes will record repo_map.unavailable")
		return nil
	}
	_, _ = fmt.Fprintf(out, "  index         generation %d — %d symbols, %d edges\n",
		status.GenerationID, status.NodeCount, status.EdgeCount)
	if !status.IsFresh {
		_, _ = fmt.Fprintln(out, "                STALE: built before the current working tree; answers describe older code")
	}

	m, err := loadGraph(graphArtifactPath())
	if err != nil {
		return err
	}
	if m == nil {
		_, _ = fmt.Fprintf(out, "  areas         no graph artifact at %s — run `manvi map build`\n", graphArtifactPath())
		return nil
	}
	s := m.Stats()
	p := m.Provenance()
	// Printed on the areas line rather than below it, because the two lines
	// above and below were being read as one report about one graph. They were
	// not: the index stood at generation 4 with 4,249 symbols while this file
	// carried generation 2 and 2,713, and nothing here said so.
	generation := "unstamped"
	if p.Stamped {
		generation = fmt.Sprintf("generation %d", p.GenerationID)
	}
	_, _ = fmt.Fprintf(out, "  areas         %d over %d files, %d adjacencies from %d edges (%s)\n",
		s.Areas, s.Files, s.Adjacencies, s.Edges, generation)
	for _, note := range m.DisagreementsWith(status.GenerationID, status.NodeCount) {
		_, _ = fmt.Fprintf(out, "                DIVERGED: %s\n", note)
	}
	for _, note := range m.Degraded() {
		if said[note] {
			continue
		}
		_, _ = fmt.Fprintf(out, "                %s\n", note)
	}
	if s.AmbiguousSkipped > 0 {
		// Reported because it is the difference between "these areas are not
		// coupled" and "the analyser could not tell".
		_, _ = fmt.Fprintf(out, "                %d coupling edge(s) excluded as unresolved; adjacency rests only on resolved ones\n",
			s.AmbiguousSkipped)
	}
	if s.Permissive() {
		// Two shapes reach this, and one of them has no widest area to name:
		// an index that holds a single subsystem has nothing to be adjacent
		// to, so degree zero is the *most* permissive reading rather than the
		// least. Printing the ratio there would say `"" neighbours 0 of 1`,
		// which reads as the opposite of what it means.
		if s.MaxDegree == 0 {
			_, _ = fmt.Fprintf(out, "                PERMISSIVE: the index holds one subsystem, so every indexed file is in the same subsystem as any planned file and the scope rung separates nothing\n")
		} else {
			_, _ = fmt.Fprintf(out, "                PERMISSIVE: %q neighbours %d of %d areas, so the neighbour rule allows most writes\n",
				s.WidestArea, s.MaxDegree, s.Areas)
		}
	}
	return nil
}
