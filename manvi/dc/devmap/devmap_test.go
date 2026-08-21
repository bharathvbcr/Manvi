package devmap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fake writes a stand-in devmap that answers each subcommand from a table.
// Driving the real binary would test DevCouncil's port; what needs testing here
// is this boundary's behaviour when that binary answers badly.
func fake(t *testing.T, replies map[string]string) *Client {
	t.Helper()
	return fakeSaying(t, replies, nil)
}

// fakeSaying is fake with a second table for stderr.
//
// It exists because devmap's answer is split across both streams: the counts
// arrive as JSON on stdout and what the command could not do arrives as text on
// stderr. A fake that could only speak on stdout could not reproduce the case
// that matters, which is a command exiting zero while saying it left something
// out.
// The artifacts variadic lists paths the fake creates when it answers
// `manifest`. Manifest verifies its output is on disk afterwards — a command
// that exits zero having written nothing leaves the gate reading the previous
// graph — so a fake that only speaks cannot stand in for one that writes.
func fakeSaying(t *testing.T, replies, said map[string]string, artifacts ...string) *Client {
	t.Helper()
	dir := t.TempDir()
	commands := map[string]bool{}
	for command := range replies {
		commands[command] = true
	}
	for command := range said {
		commands[command] = true
	}
	script := "#!/bin/sh\n" +
		// clap answers `manifest --help` with the flag list and exits before
		// doing any work, whatever else is on the line; the capability probe
		// depends on that short-circuit, so the fake honours it first.
		"case \" $* \" in *\" --help \"*) cat <<'HELP'\nusage: devmap manifest [OPTIONS]\n" +
		"      --graph-output <PATH>\nHELP\nexit 0;;\nesac\n" +
		"for a in \"$@\"; do case \"$a\" in\n"
	for command := range commands {
		script += "  " + command + ")\n"
		if command == "manifest" {
			for _, artifact := range artifacts {
				script += "echo '{\"nodes\":[],\"edges\":[]}' > " + artifact + "\n"
			}
		}
		if reply, ok := replies[command]; ok {
			script += "cat <<'JSON'\n" + reply + "\nJSON\n"
		}
		if text, ok := said[command]; ok {
			script += "cat >&2 <<'ERR'\n" + text + "\nERR\n"
		}
		script += "  exit 0;;\n"
	}
	script += "esac; done\necho '{}'\n"
	path := filepath.Join(dir, "devmap")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return New(path, dir)
}

// clapHelp prepends the `--help` short-circuit a real devmap answers with
// before doing any work, whatever else is on its command line. Fakes used by
// tests that reach Build or Available need it because those entry points probe
// capability first; without it the fake fails the probe instead of exercising
// the behaviour the test is about.
func clapHelp(script string) string {
	body := strings.TrimPrefix(script, "#!/bin/sh\n")
	return "#!/bin/sh\ncase \" $* \" in *\" --help \"*) echo '--graph-output <PATH>'; exit 0;; esac\n" + body
}

const healthyStatus = `{"db_path":"x","generation_id":3,"node_count":1200,"edge_count":9000,
"pending_count":0,"quarantined_count":0,"is_fresh":true,"degraded_reason":null}`

// TestAnUnbuiltIndexIsUnavailableNotEmpty is the consequential one. An unbuilt
// index answers every query with an empty list, and "no callers found" is a
// conclusion somebody deletes code on.
func TestAnUnbuiltIndexIsUnavailableNotEmpty(t *testing.T) {
	cases := map[string]string{
		"never built":   `{"generation_id":0,"node_count":0,"edge_count":0,"is_fresh":false,"degraded_reason":null}`,
		"no symbols":    `{"generation_id":4,"node_count":0,"edge_count":0,"is_fresh":true,"degraded_reason":null}`,
		"self-reported": `{"generation_id":4,"node_count":10,"edge_count":2,"is_fresh":true,"degraded_reason":"fts index is corrupt"}`,
	}
	for name, status := range cases {
		c := fake(t, map[string]string{"status": status})
		if _, err := c.Available(context.Background()); err == nil {
			t.Errorf("%s: reported available", name)
		}
	}

	healthy := fake(t, map[string]string{"status": healthyStatus})
	if _, err := healthy.Available(context.Background()); err != nil {
		t.Fatalf("a committed generation with symbols must be available: %v", err)
	}
}

// TestAShapeChangeIsAnErrorNotAnEmptyAnswer pins the guard for the defect that
// produced it: decoding into a mismatched struct yields a full slice of blanks
// and no error, so a file with 166 dependencies reported none.
func TestAShapeChangeIsAnErrorNotAnEmptyAnswer(t *testing.T) {
	c := fake(t, map[string]string{
		"status": healthyStatus,
		// The shape this build no longer understands: the identifying fields
		// are gone, everything else parses.
		"deps": `{"hidden":0,"items":[{"from":"a.go","to":"b.go"},{"from":"c.go","to":"d.go"}]}`,
	})
	_, err := c.Deps(context.Background(), "a.go")
	if err == nil {
		t.Fatal("a response whose fields this build does not understand decoded as an empty answer")
	}
	if !strings.Contains(err.Error(), "shape has changed") {
		t.Fatalf("err = %v, want it to name the contract change", err)
	}

	// A genuinely empty answer is still fine — that is a real result.
	empty := fake(t, map[string]string{"status": healthyStatus, "deps": `{"hidden":0,"items":[]}`})
	result, err := empty.Deps(context.Background(), "a.go")
	if err != nil {
		t.Fatalf("a genuinely empty answer must not error: %v", err)
	}
	if len(result.Items) != 0 || !result.Clean() {
		t.Fatalf("result = %+v", result)
	}
}

// TestStalenessAndSuppressionRideOnEveryAnswer: an index built ten minutes ago
// answers confidently about code that has since changed, and a budgeted search
// returns a sample that reads like the whole answer.
func TestStalenessAndSuppressionRideOnEveryAnswer(t *testing.T) {
	c := fake(t, map[string]string{
		"status": `{"generation_id":2,"node_count":900,"edge_count":40,"pending_count":3,
		"quarantined_count":1,"is_fresh":false,"degraded_reason":null}`,
		"search": `{"hidden":42,"items":[{"file_path":"a.go","symbol_name":"F","kind":"Func"}]}`,
	})
	result, err := c.Search(context.Background(), "F")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Stale {
		t.Error("an index older than the working tree must mark its answers stale")
	}
	if result.Clean() {
		t.Error("a stale, budget-capped answer must not read as clean")
	}
	joined := strings.Join(result.Degraded, " | ")
	for _, want := range []string{"older than the working tree", "pending", "quarantined", "suppressed by the budget"} {
		if !strings.Contains(joined, want) {
			t.Errorf("degraded does not mention %q: %v", want, result.Degraded)
		}
	}
}

// TestFreshnessIsReadPerQuery: the case that matters is the query issued after
// ten minutes of editing, when a status checked once at startup still says fresh.
func TestFreshnessIsReadPerQuery(t *testing.T) {
	c := fake(t, map[string]string{
		"status": healthyStatus,
		"dead":   `{"hidden":0,"items":[{"file_path":"a.go","symbol_name":"Unused","confidence":0.9,"is_exempt":false,"exemption_reason":null}]}`,
	})
	result, err := c.Dead(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Clean() || len(result.Items) != 1 {
		t.Fatalf("result = %+v", result)
	}
	if result.Items[0].Name != "Unused" || result.Items[0].Confidence != 0.9 {
		t.Fatalf("item = %+v", result.Items[0])
	}
}

// TestAnUnreachableBinaryFailsRatherThanAnswering.
func TestAnUnreachableBinaryFailsRatherThanAnswering(t *testing.T) {
	for _, c := range []*Client{
		New("", t.TempDir()),
		New(filepath.Join(t.TempDir(), "absent"), t.TempDir()),
		New("/bin/echo", ""),
	} {
		if _, err := c.Available(context.Background()); err == nil {
			t.Errorf("client %+v reported available", c)
		}
	}

	// A binary that exits zero printing nothing useful is also unavailable.
	c := fake(t, map[string]string{"status": `not json at all`})
	if _, err := c.Available(context.Background()); err == nil {
		t.Error("unparseable output reported available")
	}
}

// TestEdgesCarryBothEndpoints is what lets a caller derive direction from the
// data rather than from the subcommand's name.
func TestEdgesCarryBothEndpoints(t *testing.T) {
	c := fake(t, map[string]string{
		"status": healthyStatus,
		"deps": `{"hidden":0,"items":[
		 {"source_file":"a.go","source_symbol":"a.go::F","target_file":"b.go","target_symbol":"b.go::G","edge_kind":"Calls","confidence":1.0}]}`,
	})
	result, err := c.Deps(context.Background(), "a.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("items = %+v", result.Items)
	}
	e := result.Items[0]
	if e.SourceFile != "a.go" || e.TargetFile != "b.go" || e.Kind != "Calls" {
		t.Fatalf("edge = %+v", e)
	}
}

func TestBuildAndManifest(t *testing.T) {
	dir := t.TempDir()
	mapPath := filepath.Join(dir, "repo_map.json")
	graphPath := filepath.Join(dir, "code_graph.json")
	c := fakeSaying(t, map[string]string{
		"build":    `{"files_indexed":12,"symbols":80,"edges":150}`,
		"manifest": `{"generation_id":3,"written":true}`,
		"status":   healthyStatus,
	}, nil, mapPath, graphPath)
	ctx := context.Background()
	report, err := c.Build(ctx, 0)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	if report == nil || report.Stats["files_indexed"] != float64(12) {
		t.Fatalf("unexpected build report: %+v", report)
	}

	manifest, err := c.Manifest(ctx, mapPath, graphPath)
	if err != nil {
		t.Fatalf("Manifest failed: %v", err)
	}
	if !manifest.Clean() {
		t.Fatalf("expected a clean manifest from empty stderr, got %v", manifest.Degraded())
	}
	// healthyStatus stands at generation 3 and the manifest rendered 3, which
	// is the case that must stay silent.
	if manifest.GenerationID != 3 {
		t.Fatalf("GenerationID = %d, want the 3 the manifest reported", manifest.GenerationID)
	}
}

func TestReadNoticesClassification(t *testing.T) {
	sampleStderr := `
discovery refused 3 file(s)
  src/large.bin: Oversized { size: 10000000 }
  src/bad.utf8: NotUTF8
  ... and 1 more
unclassified error from downstream
`
	n := readNotices(said{text: []byte(sampleStderr)})
	if n.Refused != 3 {
		t.Errorf("Refused = %d, want 3", n.Refused)
	}
	if len(n.Refusals) != 2 {
		t.Errorf("len(Refusals) = %d, want 2", len(n.Refusals))
	}
	if len(n.Unrecognised) != 1 || n.Unrecognised[0] != "unclassified error from downstream" {
		t.Errorf("Unrecognised = %v, want ['unclassified error from downstream']", n.Unrecognised)
	}
	if n.Clean() {
		t.Error("notices with refusals and unrecognised lines must not be Clean()")
	}

	degraded := n.Degraded()
	if len(degraded) != 2 {
		t.Fatalf("len(degraded) = %d, want 2 (%v)", len(degraded), degraded)
	}

	// Test Merge
	other := Notices{
		Refused:      2,
		Refusals:     []Refusal{{Path: "x.go", Reason: "Unreadable"}},
		Unrecognised: []string{"warning"},
	}
	n.Merge(other)
	if n.Refused != 5 {
		t.Errorf("merged Refused = %d, want 5", n.Refused)
	}
	if len(n.Refusals) != 3 {
		t.Errorf("merged len(Refusals) = %d, want 3", len(n.Refusals))
	}
}

// refusedStderr is recorded verbatim from devmap 0.1.0 (DevCouncil 6bc0eff)
// building a fixture holding one oversized source and one that is not UTF-8.
//
// It is pasted rather than paraphrased on purpose. This is the producer's text,
// not a format this repository chose, and a test written against a tidied-up
// version of it would pass while the real thing went unparsed.
const refusedStderr = "  discovery refused 2 file(s) — these are absent from the graph:\n" +
	"    big.py: Oversized { bytes: 1800000, limit: 1048576 }\n" +
	`    blob.py: Unreadable { reason: "stream did not contain valid UTF-8" }`

// TestABuildReportsWhatItRefused is the regression on the build path.
//
// devmap prints its counts as JSON on stdout and the files discovery refused as
// text on stderr. This package kept the first and dropped the second on every
// run that exited zero, so a build that indexed three of five files reported
// `indexed 3 files` with nothing anywhere saying two more existed — and a query
// about either of them returns the same empty answer as code that does not
// exist.
func TestABuildReportsWhatItRefused(t *testing.T) {
	c := fakeSaying(t,
		map[string]string{"build": `{"files_indexed":3,"symbols":5,"edges":5}`},
		map[string]string{"build": refusedStderr})

	report, err := c.Build(context.Background(), 0)
	if err != nil {
		t.Fatalf("a build that refused files is still a build: %v", err)
	}
	if report.Refused != 2 {
		t.Fatalf("Refused = %d, want 2 — the count devmap itself reported", report.Refused)
	}
	if report.Clean() {
		t.Error("a build missing two files must not report itself clean")
	}
	if len(report.Unrecognised) != 0 {
		t.Errorf("the producer's own format was not understood: %v", report.Unrecognised)
	}
	byPath := map[string]string{}
	for _, refusal := range report.Refusals {
		byPath[refusal.Path] = refusal.Reason
	}
	if got := byPath["big.py"]; !strings.Contains(got, "Oversized") {
		t.Errorf("big.py reason = %q, want it to name the refusal", got)
	}
	if got := byPath["blob.py"]; !strings.Contains(got, "Unreadable") {
		// Split at the first ": " rather than the last: the reason carries one
		// of its own inside `Unreadable { reason: "..." }`.
		t.Errorf("blob.py reason = %q, want it to name the refusal", got)
	}

	degraded := strings.Join(report.Degraded(), " | ")
	for _, want := range []string{"absent from the graph", "big.py", "blob.py"} {
		if !strings.Contains(degraded, want) {
			t.Errorf("degraded does not mention %q: %v", want, report.Degraded())
		}
	}
}

// TestARewordedNoticeIsCarriedNotDropped guards the parser the way assertShape
// guards the decoder.
//
// The producer is built from another repository, so its wording can change
// without anything here failing to compile. A parser that quietly matched
// nothing would then report a clean build for one that refused half the tree,
// which is the exact failure this whole path was changed to prevent.
func TestARewordedNoticeIsCarriedNotDropped(t *testing.T) {
	c := fakeSaying(t,
		map[string]string{"build": `{"files_indexed":3}`},
		map[string]string{"build": "  the indexer declined 9 file(s), wording since reworded upstream"})

	report, err := c.Build(context.Background(), 0)
	if err != nil {
		t.Fatalf("an unreadable notice is not a failed build: %v", err)
	}
	if report.Clean() {
		t.Error("a build whose notice this parser could not read must not report itself clean")
	}
	if report.Refused != 0 {
		t.Errorf("Refused = %d: an unparsed line must not be counted as a number this build invented", report.Refused)
	}
	if !strings.Contains(strings.Join(report.Degraded(), " "), "reworded upstream") {
		t.Errorf("the producer's own words were dropped: %v", report.Degraded())
	}
}

// TestTheRefusalCountIsTheProducersTotal: devmap names the first twenty
// refusals and summarises the rest. Counting the printed lines would
// under-report exactly when there is most to report.
func TestTheRefusalCountIsTheProducersTotal(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("  discovery refused 25 file(s) — these are absent from the graph:\n")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&sb, "    vendor/blob%d.py: Unreadable { reason: \"stream did not contain valid UTF-8\" }\n", i)
	}
	sb.WriteString("    … and 5 more")

	c := fakeSaying(t,
		map[string]string{"build": `{"files_indexed":100}`},
		map[string]string{"build": sb.String()})

	report, err := c.Build(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Refused != 25 {
		t.Errorf("Refused = %d, want the producer's total of 25, not the %d it printed",
			report.Refused, len(report.Refusals))
	}
	if len(report.Refusals) != 20 {
		t.Errorf("named %d refusals, want the 20 devmap printed", len(report.Refusals))
	}
	if len(report.Unrecognised) != 0 {
		t.Errorf("the summary line was treated as unreadable: %v", report.Unrecognised)
	}
	// The one line must still carry the whole number, not the tenth it names.
	if line := report.Degraded()[0]; !strings.Contains(line, "25 file(s)") || !strings.Contains(line, "and 15 more") {
		t.Errorf("degraded line hides the total: %q", line)
	}
}

// TestManifestNoticesJoinTheBuildsOwn: two commands produce one artifact, so
// what either left out belongs to one report.
func TestManifestNoticesJoinTheBuildsOwn(t *testing.T) {
	dir := t.TempDir()
	mapPath := filepath.Join(dir, "repo_map.json")
	graphPath := filepath.Join(dir, "code_graph.json")
	c := fakeSaying(t,
		map[string]string{"build": `{"files_indexed":3}`, "manifest": `{"generation_id":3,"output":"x"}`, "status": healthyStatus},
		map[string]string{"build": refusedStderr, "manifest": "  discovery refused 1 file(s) — these are absent from the graph:\n    late.py: Oversized { bytes: 2, limit: 1 }"},
		mapPath, graphPath)

	ctx := context.Background()
	report, err := c.Build(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	notices, err := c.Manifest(ctx, mapPath, graphPath)
	if err != nil {
		t.Fatal(err)
	}
	// Manifest now answers with a ManifestReport — the notices plus the
	// generation it rendered — so the notices half is what merges.
	report.Merge(notices.Notices)
	if report.Refused != 3 {
		t.Errorf("Refused = %d, want 3 across both commands", report.Refused)
	}
	if len(report.Refusals) != 3 {
		t.Errorf("named %d refusals, want 3", len(report.Refusals))
	}
}

// TestAFloodOfUnreadableLinesIsBoundedAndCounted.
//
// stderr is unbounded input from another process, and since these lines now
// reach an operator's terminal and the session transcript, a producer that
// spewed would flood both. The cap is on what the report keeps, never on what
// it admits happened: the remainder is counted and said out loud, because a cap
// that cannot be seen is the failure this whole path was changed to prevent.
func TestAFloodOfUnreadableLinesIsBoundedAndCounted(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&sb, "unreadable line %d\n", i)
	}
	c := fakeSaying(t,
		map[string]string{"build": `{"files_indexed":3}`},
		map[string]string{"build": sb.String()})

	report, err := c.Build(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Unrecognised) != noticeLimit {
		t.Errorf("kept %d lines, want the %d cap", len(report.Unrecognised), noticeLimit)
	}
	if report.UnrecognisedHidden != 200-noticeLimit {
		t.Errorf("hidden = %d, want %d — the rest must be counted, not dropped",
			report.UnrecognisedHidden, 200-noticeLimit)
	}
	if report.Clean() {
		t.Error("a build this build could not read must not report itself clean")
	}
	if last := report.Degraded()[len(report.Degraded())-1]; !strings.Contains(last, "136 further") {
		t.Errorf("the remainder is not reported: %q", last)
	}
}

// TestABuildWhoseAnalysisDisagreesWithTheIndexIsNotClean is the outside check
// on the producer.
//
// devmap once resolved only the files a change could reach and handed its
// analyser 63 edges of the 15,017 it stored. Every query about the graph was
// right; only the analysis of it was wrong, so `dead` named plainly-called
// symbols as callerless and nothing — not the exit code, not the JSON, not the
// index's own status — said so. The build's edge count and the index's edge
// count are the two numbers that had to disagree for that to happen.
func TestABuildWhoseAnalysisDisagreesWithTheIndexIsNotClean(t *testing.T) {
	c := fake(t, map[string]string{
		"build":  `{"files_indexed":155,"symbols":2577,"edges":63}`,
		"status": `{"generation_id":2,"node_count":2577,"edge_count":15017,"is_fresh":true,"degraded_reason":null}`,
	})

	report, err := c.Build(context.Background(), 0)
	if err != nil {
		t.Fatalf("the build succeeded; the doubt is about its analysis: %v", err)
	}
	if report.Clean() {
		t.Fatal("a build whose analysis covered 63 of 15017 edges reported itself clean")
	}
	joined := strings.Join(report.Degraded(), " ")
	for _, want := range []string{"63", "15017", "callerless"} {
		if !strings.Contains(joined, want) {
			t.Errorf("degraded does not mention %q: %v", want, report.Degraded())
		}
	}
}

// TestABuildThatAgreesWithItsIndexIsClean: the check must not cry wolf, or it
// will be the line everyone learns to skip.
func TestABuildThatAgreesWithItsIndexIsClean(t *testing.T) {
	c := fake(t, map[string]string{
		"build":  `{"files_indexed":155,"symbols":2577,"edges":15017}`,
		"status": `{"generation_id":2,"node_count":2577,"edge_count":15017,"is_fresh":true,"degraded_reason":null}`,
	})
	report, err := c.Build(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Clean() {
		t.Fatalf("a build that agrees with its index is clean: %v", report.Degraded())
	}
}

// TestAnUnreadableIndexIsUnverifiedNotVerified: a check that could not run must
// not report what a check that ran and passed reports.
func TestAnUnreadableIndexIsUnverifiedNotVerified(t *testing.T) {
	c := fake(t, map[string]string{
		"build":  `{"files_indexed":155,"symbols":2577,"edges":15017}`,
		"status": `not json at all`,
	})
	report, err := c.Build(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Clean() {
		t.Fatal("an index that could not be read back reported itself verified")
	}
	if !strings.Contains(strings.Join(report.Degraded(), " "), "unverified") {
		t.Errorf("degraded does not say the check could not run: %v", report.Degraded())
	}
}

// TestAnUnchangedBuildReportsNoCountsRatherThanZeroes.
//
// devmap answers a build over an unchanged tree with a different payload
// entirely — no counts at all. Absent must not read as zero, and it must not be
// compared against an index it never analysed.
func TestAnUnchangedBuildReportsNoCountsRatherThanZeroes(t *testing.T) {
	c := fake(t, map[string]string{
		"build":  `{"unchanged":true,"files":155,"generation":7}`,
		"status": `{"generation_id":7,"node_count":2577,"edge_count":15017,"is_fresh":true,"degraded_reason":null}`,
	})
	report, err := c.Build(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Unchanged() {
		t.Fatal("an unchanged build was not recognised as one")
	}
	if _, ok := report.Stat("edges"); ok {
		t.Error("a field the producer never sent was reported as a number")
	}
	if files, ok := report.Stat("files"); !ok || files != 155 {
		t.Errorf("files = %d, %v; want 155, true", files, ok)
	}
	// Nothing was analysed, so there is nothing to disagree with — and an
	// unchanged build must not be flagged merely for reporting no counts.
	if !report.Clean() {
		t.Errorf("an unchanged build was reported degraded: %v", report.Degraded())
	}
}

// TestTheNoStorePayloadIsUnavailableNotEmpty.
//
// `devmap status` against a repository with no index answers rather than
// failing, and its payload carries a null generation and zero counts. Decoding
// null into an int leaves the zero value and returns no error, so this is
// exactly the shape that reads as a healthy empty repository if nothing checks
// it — the same trap as a query answering "no results" from an unbuilt index.
func TestTheNoStorePayloadIsUnavailableNotEmpty(t *testing.T) {
	c := fake(t, map[string]string{"status": `{
		"generation_id": null, "pending_count": 0, "node_count": 0, "edge_count": 0,
		"is_fresh": false, "db_path": ".devcouncil/codeintel/index.sqlite",
		"degraded_reason": "no devmap store at this path (run ` + "`devmap build`" + `)",
		"quarantined_count": 0}`})

	status, err := c.Available(context.Background())
	if err == nil {
		t.Fatal("a repository with no index reported itself available")
	}
	if status == nil || status.GenerationID != 0 {
		t.Fatalf("the payload decoded to %+v; a null generation must read as none", status)
	}
}
