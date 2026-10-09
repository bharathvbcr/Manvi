package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/repomap"
	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// refreshIndex rebuilds the index and rewrites the artifacts read from it.
//
// The two steps are one function because they carry one fact between them: the
// gate's neighbour rule reads the code graph, not the index, so a build without
// the manifest leaves the gate deciding from the previous repository while
// every navigation tool answers about the current one.
func refreshIndex(ctx context.Context, client *devmap.Client) (*devmap.BuildReport, error) {
	report, err := client.Build(ctx, 15*time.Minute)
	if err != nil {
		return nil, &refreshError{stage: stageIndex, err: err}
	}
	manifest, err := client.Manifest(ctx, repoMapArtifactPath(), graphArtifactPath())
	if err != nil {
		return nil, &refreshError{stage: stageArtifacts, err: err}
	}
	// One artifact, one account of it: the two commands are a single operation
	// to the caller, so what either of them left out belongs to one report.
	report.Merge(manifest.Notices)
	report.Disagreements = append(report.Disagreements, manifest.Disagreements...)
	report.Disagreements = append(report.Disagreements,
		artifactDisagreements(graphArtifactPath(), manifest.GenerationID)...)
	report.Adopted = append(report.Adopted, manifest.Adopted...)
	return report, nil
}

// refreshStage names which half of a refresh failed.
//
// The two halves are two commands writing two things with two lifetimes, and
// they degrade different capabilities: `devmap build` advances the index the
// navigation tools query, `devmap manifest` writes the code graph the gate's
// scope rung reads. A failure in the second used to be reported as "the
// repository index could not be built" — a sentence about an index that had
// just been built successfully, naming a capability that was in fact working
// and not naming the one that was not. An operator following it went to look
// at the index.
type refreshStage int

const (
	stageIndex refreshStage = iota
	stageArtifacts
)

// refreshError is a refresh failure that knows which command produced it.
type refreshError struct {
	stage refreshStage
	err   error
}

func (e *refreshError) Unwrap() error { return e.err }

func (e *refreshError) Error() string {
	switch e.stage {
	case stageArtifacts:
		return "the repository index was rebuilt, but the artifacts read from it could not be written: " +
			e.err.Error()
	default:
		return "the repository index could not be built: " + e.err.Error()
	}
}

// degraded names what will answer wrongly until the failure is dealt with.
//
// Split by stage for the same reason the message is. After a failed build
// nothing has current data. After a failed manifest the index is current and
// the tools that query it are fine; it is the file on disk — the one the gate
// reads, and the one the next session loads — that is stale or absent.
func (e *refreshError) degraded() []string {
	if e.stage == stageArtifacts {
		return []string{
			"navigation tools answer from the rebuilt index, but the scope rung decides neighbour " +
				"writes from the code graph on disk, which is whatever was there before",
		}
	}
	return []string{"navigation tools and the neighbour rule will report unavailable"}
}

// refreshFailure renders a refresh error for the transcript: what happened, and
// what is degraded while it stands.
//
// It accepts a plain error because refreshIndex is not the only thing that can
// fail here and an error from somewhere else must still be reported rather than
// swallowed for not being the expected type.
func refreshFailure(err error) (string, []string) {
	var staged *refreshError
	if errors.As(err, &staged) {
		return staged.Error(), staged.degraded()
	}
	return "the repository index could not be refreshed: " + err.Error(),
		[]string{"navigation tools and the neighbour rule will report unavailable"}
}

// artifactDisagreements reads back the graph the manifest just wrote and
// compares what the file says about itself with what the command said it
// rendered.
//
// This is the third check on one operation, and each catches something the
// others cannot. Manifest verifies a file is there afterwards — but a manifest
// that failed to overwrite leaves the *previous* file there, which passes that
// check with room to spare. Its generation comparison verifies what devmap
// intended to render. Only reading the bytes back verifies what landed.
func artifactDisagreements(graphPath string, rendered int) []string {
	m, err := repomap.LoadIfPresent(graphPath)
	if err != nil {
		return []string{fmt.Sprintf(
			"the graph artifact was written but could not be read back, so the scope rung will "+
				"start the next session without it: %v", err)}
	}
	if m == nil {
		return []string{fmt.Sprintf(
			"no graph artifact is at %s after writing it, so the scope rung has nothing to read", graphPath)}
	}
	out := m.Degraded()
	if rendered > 0 {
		if p := m.Provenance(); p.Stamped && p.GenerationID != rendered {
			out = append(out, fmt.Sprintf(
				"devmap reported rendering generation %d but the file at %s carries generation %d, "+
					"so the write did not land and the scope rung would keep reading the older graph",
				rendered, graphPath, p.GenerationID))
		}
	}
	return out
}

// describeIndex renders what a build indexed, in one line.
//
// The refusal count rides on this line rather than only in Degraded, because
// this string is the headline the TUI shows and what an operator reads as the
// outcome. "indexed 173 files" is a true sentence about a build that skipped
// the 174th, and a summary that cannot be read as complete coverage unless it
// is complete is the whole point.
//
// Both payload shapes are rendered. devmap answers a build whose tree is
// byte-for-byte the indexed one with `{"unchanged":true,"files":N}` and no
// counts at all, and this function used to format those absent fields straight
// into the line: an operator running `manvi map build` twice was told `indexed
// <nil> files, <nil> symbols, <nil> edges` the second time. A field the
// producer did not report is not a number, and printing it as one taught the
// reader to distrust the line that carries the refusal count.
func describeIndex(report *devmap.BuildReport) string {
	var line string
	switch {
	case report.Unchanged():
		line = "index already current, nothing to rebuild"
		if files, ok := report.Stat("files"); ok {
			line = fmt.Sprintf("index already current at %d files, nothing to rebuild", files)
		}
	default:
		line = fmt.Sprintf("indexed %s files, %s symbols, %s edges",
			stat(report, "files_indexed"), stat(report, "symbols"), stat(report, "edges"))
	}
	if report.Refused > 0 {
		line += fmt.Sprintf(", refused %d", report.Refused)
	}
	return line
}

// stat renders one count, or says the producer did not report it.
func stat(report *devmap.BuildReport, name string) string {
	if value, ok := report.Stat(name); ok {
		return fmt.Sprintf("%d", value)
	}
	return "an unreported number of"
}

// refreshIndexInBackground brings the navigation index up to date while the
// operator is already working.
//
// It reports into the transcript in every outcome, including the one where
// nothing needed doing. The index is what the navigation tools answer from and
// what the gate's neighbour rule decides on, and those two answer very
// differently with a stale index than with none — "no callers, safe to delete"
// and "this build has no data" are the same empty list. An operator who was not
// told which one they are looking at will read either as a fact about their
// code.
//
// Nothing here is fatal. A repository with no devmap on PATH is a repository
// the harness still runs in, with navigation reporting unavailable.
func (h *harnessHost) refreshIndexInBackground(ctx context.Context) {
	on, _, err := h.reg.Bool(flags.HarnessInitEnabled)
	if err != nil || !on {
		return
	}

	var sessionID string
	select {
	case sessionID = <-h.firstSession:
	case <-ctx.Done():
		return
	}
	sink := h.sinkFor(sessionID)

	root := projectRoot()
	client := mapClient(root)

	// The artifact is checked alongside the index because they are two files
	// with two lifetimes and only one of them was ever consulted here. The gate
	// does not read the index; it reads the code graph, which `devmap build`
	// does not write and a failed manifest does not replace.
	status, availErr := client.Available(ctx)
	plan := planIndexRefresh(status, availErr, artifactDivergence(graphArtifactPath(), status))
	sink.Emit(ui.Event{Kind: plan.Kind, At: time.Now().UTC(), Text: plan.Text, Degraded: plan.Degraded})
	if !plan.Build {
		return
	}

	report, err := refreshIndex(ctx, client)
	if err != nil {
		if ctx.Err() != nil {
			// The session is ending; the failure is the shutdown, not the index.
			return
		}
		// The message names which of the two commands failed and degrades only
		// what that one feeds: see refreshError. Reporting a failed artifact
		// write as a failed index build named a capability that was working and
		// left the one that was not unnamed.
		text, degraded := refreshFailure(err)
		sink.Emit(ui.Event{Kind: ui.KindError, At: time.Now().UTC(), Text: text})
		sink.Emit(ui.Event{Kind: ui.KindNotice, At: time.Now().UTC(),
			Text:     "run 'manvi map build' to see the whole error",
			Degraded: degraded})
		return
	}
	// A build that refused files is emitted as a notice rather than a report,
	// on the same rule indexPlan uses below: a report is for an index that
	// needs nothing, a notice for one that is less than it appears.
	kind := ui.KindReport
	if !report.Clean() {
		kind = ui.KindNotice
	}
	sink.Emit(ui.Event{Kind: kind, At: time.Now().UTC(),
		Text:     describeIndex(report),
		Degraded: report.Degraded(),
		// Sessions load the code graph when they open. This one opened first,
		// which is how it got this report, and it is holding whatever the graph
		// said then.
		Detail: "open a new session to navigate and gate against the rebuilt graph",
	})
}

// indexPlan is what the background refresh decided about the index it found,
// and what the transcript is told about it.
type indexPlan struct {
	// Build says the index has to be rebuilt.
	Build bool
	// Kind is how Text is emitted: a report when the index needs nothing, a
	// notice when something about it is less than it appears.
	Kind ui.Kind
	Text string
	// Degraded names what will answer wrongly until the rebuild lands.
	Degraded []string
}

// planIndexRefresh reads the index's self-report and decides.
//
// It is separated from the emitting so it can be tested without a terminal, and
// because the decision is the part with a wrong answer available: a stale index
// and a missing one both need a build, but they degrade different things, and
// telling an operator "unavailable" about an index that is merely old — or
// "stale" about one that was never built — sends them to the wrong problem.
// diverged is what the code graph on disk disagrees with the index about, empty
// when they agree or when there is nothing to compare.
func planIndexRefresh(status *devmap.Status, err error, diverged []string) indexPlan {
	switch {
	case err != nil:
		return indexPlan{
			Build: true, Kind: ui.KindNotice,
			Text:     "building the repository index in the background: " + err.Error(),
			Degraded: []string{"navigation tools and the neighbour rule report unavailable until it lands"},
		}
	case status == nil:
		// No status and no error is a contract the client is not supposed to
		// be able to produce. Building is the answer that cannot mislead.
		return indexPlan{
			Build: true, Kind: ui.KindNotice,
			Text:     "the repository index reported neither a state nor an error; rebuilding it in the background",
			Degraded: []string{"navigation tools and the neighbour rule report unavailable until it lands"},
		}
	case !status.IsFresh:
		return indexPlan{
			Build: true, Kind: ui.KindNotice,
			Text:     "the repository index was built before the current working tree; rebuilding it in the background",
			Degraded: []string{"navigation answers describe the previous state until the rebuild lands"},
		}
	case len(diverged) > 0:
		// The index is current and the file the gate reads is not. This is the
		// state the check missed for as long as it asked only devmap: the
		// index's own freshness says nothing about an artifact written by a
		// different command, and reporting "index current" here sent an
		// operator away from the half that was wrong.
		return indexPlan{
			Build: true, Kind: ui.KindNotice,
			Text: "the code graph the scope rung reads did not come from the current index; " +
				"rebuilding it in the background: " + strings.Join(diverged, "; "),
			Degraded: []string{
				"until it lands, the scope rung decides neighbour writes from an older graph " +
					"while the navigation tools answer from the current index"},
		}
	default:
		return indexPlan{
			Kind: ui.KindReport,
			Text: fmt.Sprintf("repository index current — generation %d, %d symbols, %d edges",
				status.GenerationID, status.NodeCount, status.EdgeCount),
		}
	}
}

// artifactDivergence reports what the code graph on disk disagrees with the
// index about, for the session-start check.
//
// A status that could not be read yields nothing rather than a complaint: the
// caller already reports the unavailable index, and a second line inferring
// something about the artifact from an index nobody could read would be a
// conclusion drawn from a check that did not run.
func artifactDivergence(graphPath string, status *devmap.Status) []string {
	if status == nil {
		return nil
	}
	m, err := repomap.LoadIfPresent(graphPath)
	if err != nil || m == nil {
		// Absent or unreadable is not divergence; it is the unavailability the
		// gate already records as repo_map.unavailable on every decision.
		return nil
	}
	return m.DisagreementsWith(status.GenerationID, status.NodeCount)
}
