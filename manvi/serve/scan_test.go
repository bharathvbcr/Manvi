package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/dcgrep"
	mapclient "github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
)

func goodStatus() *mapclient.Status {
	return &mapclient.Status{
		DBPath: "db", GenerationID: 7, NodeCount: 100, EdgeCount: 200, IsFresh: true,
		HostContractVersion: 1, SchemaRelation: "current", ReaderReady: true, QueryReady: true,
		Capabilities: map[string]any{"affected": true},
	}
}

type scanFakeDevmap struct {
	mu       sync.Mutex
	status   *mapclient.Status
	dead     mapclient.Result[mapclient.DeadSymbol]
	deadWait func(context.Context) error
	deadRuns int
	affected func(target string) string
}

func (f *scanFakeDevmap) Status(context.Context) (*mapclient.Status, error) { return f.status, nil }

func (f *scanFakeDevmap) Dead(ctx context.Context) (mapclient.Result[mapclient.DeadSymbol], error) {
	f.mu.Lock()
	f.deadRuns++
	wait := f.deadWait
	f.mu.Unlock()
	if wait != nil {
		if err := wait(ctx); err != nil {
			return mapclient.Result[mapclient.DeadSymbol]{}, err
		}
	}
	return f.dead, nil
}

func (f *scanFakeDevmap) Advanced(_ context.Context, q mapclient.AdvancedQuery) (mapclient.AdvancedResult, error) {
	return mapclient.AdvancedResult{Data: json.RawMessage(f.affected(q.Targets[0])), Index: f.status}, nil
}

type scanFakeText struct {
	refs      map[string][]string // symbol -> paths that mention it besides the definition
	truncated map[string]bool
}

func (f *scanFakeText) Search(_ context.Context, req dcgrep.Request) (*dcgrep.Result, error) {
	res := &dcgrep.Result{OK: true}
	for name, paths := range f.refs {
		if strings.Contains(req.Pattern, name) {
			for _, p := range paths {
				res.Matches = append(res.Matches, dcgrep.Match{Path: p, LineNumber: 1})
			}
			res.Truncated = f.truncated[name]
		}
	}
	return res, nil
}

type scanFakeBoard struct {
	mu    sync.Mutex
	items []map[string]any
	calls []string
}

func (f *scanFakeBoard) Workbench(_ context.Context, method string, _ json.RawMessage) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method)
	if method != "items.list" {
		return nil, fmt.Errorf("scan wrote to the board: %s", method)
	}
	return json.Marshal(map[string]any{"ok": true, "items": f.items, "has_more": false})
}

type scanFakeGaps struct {
	rows      []store.GapRow
	truncated bool
}

func (f scanFakeGaps) Gaps(context.Context, string) ([]store.GapRow, bool, error) {
	return f.rows, f.truncated, nil
}

// asScanView names what the runner answered when it is not a ScanView,
// instead of panicking with "interface conversion".
func asScanView(t *testing.T, got any) ScanView {
	t.Helper()
	v, ok := got.(ScanView)
	if !ok {
		t.Fatalf("the scan runner answered %T, want ScanView", got)
	}
	return v
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	// #nosec G204 -- the binary is the literal "git"; the arguments are this test's own.
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// scanRepo makes a canonical (symlink-free) git work tree with one commit.
func scanRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "init", "-q")
	writeFiles(t, dir, files)
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

type scanHarness struct {
	runner *ScanRunner
	dm     *scanFakeDevmap
	text   *scanFakeText
	board  *scanFakeBoard
	gaps   ScanGaps
	roots  []string // roots the factories were asked about
	mu     sync.Mutex
}

func newScanHarness(t *testing.T, deadline time.Duration) *scanHarness {
	t.Helper()
	h := &scanHarness{
		dm:    &scanFakeDevmap{status: goodStatus(), affected: func(string) string { return affectedJSON(1, "") }},
		text:  &scanFakeText{refs: map[string][]string{}, truncated: map[string]bool{}},
		board: &scanFakeBoard{},
	}
	record := func(root string) {
		h.mu.Lock()
		h.roots = append(h.roots, root)
		h.mu.Unlock()
	}
	runner, err := NewScanRunner(ScanDeps{
		Devmap:   func(root string) ScanDevmap { record(root); return h.dm },
		Text:     func(string) ScanText { return h.text },
		Gaps:     func(string) ScanGaps { return h.gaps },
		Board:    h.board,
		Deadline: deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Close() })
	h.runner = runner
	return h
}

func affectedJSON(tests int, extra string) string {
	items := strings.TrimSuffix(strings.Repeat(`{"path":"x_test.go"},`, tests), ",")
	return fmt.Sprintf(`{"blast_radius":{"unmatched_targets":[]%s},"targets":[],"tests":{"items":[%s],"total":%d,"hidden":0,"truncated":false}}`, extra, items, tests)
}

func (h *scanHarness) start(t *testing.T, params string) ScanView {
	t.Helper()
	got, e := h.runner.Start(context.Background(), json.RawMessage(params))
	if e != nil {
		t.Fatalf("Start(%s) refused: %+v", params, e)
	}
	return asScanView(t, got)
}

func (h *scanHarness) wait(t *testing.T, id string) ScanView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, e := h.runner.Get(json.RawMessage(fmt.Sprintf(`{"scan_id":%q}`, id)))
		if e != nil {
			t.Fatalf("Get: %+v", e)
		}
		if v := asScanView(t, got); v.State != scanRunning {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("scan did not finish")
	return ScanView{}
}

func (h *scanHarness) scan(t *testing.T, root string, extra string) ScanView {
	t.Helper()
	params := fmt.Sprintf(`{"repo_root":%q%s}`, root, extra)
	return h.wait(t, h.start(t, params).ScanID)
}

func gapRows(n int) []store.GapRow {
	rows := make([]store.GapRow, n)
	for i := range rows {
		rows[i] = store.GapRow{ID: fmt.Sprint("g", i), Severity: "high", GapType: "missing_evidence", TaskID: fmt.Sprint("task-", i), Description: "criterion lacks evidence", Blocking: true}
	}
	return rows
}

func coverageOf(t *testing.T, v ScanView, category string) ScanCoverage {
	t.Helper()
	for _, c := range v.Coverage {
		if c.Category == category {
			return c
		}
	}
	t.Fatalf("no coverage for %s in %+v", category, v.Coverage)
	return ScanCoverage{}
}

func fingerprints(v ScanView) map[string]string {
	out := map[string]string{}
	for _, p := range v.Proposals {
		out[p.Fingerprint] = p.Status
	}
	return out
}

func TestScanRescanOfAnUnchangedRepoFilesNothingNew(t *testing.T) {
	root := scanRepo(t, map[string]string{"README.md": "see `docs/gone.md`\n"})
	h := newScanHarness(t, 0)
	h.gaps = scanFakeGaps{rows: gapRows(2)}

	first := h.scan(t, root, "")
	if first.State != scanDone || len(first.Proposals) != 3 || first.NewCount != 3 {
		t.Fatalf("first scan: state=%s proposals=%d new=%d (%+v)", first.State, len(first.Proposals), first.NewCount, first.Coverage)
	}
	second := h.scan(t, root, "")
	if second.NewCount != 0 || len(second.Proposals) != 3 {
		t.Fatalf("rescan filed new proposals: new=%d proposals=%d", second.NewCount, len(second.Proposals))
	}
	for _, p := range second.Proposals {
		if p.Status != "known" {
			t.Errorf("rescan proposal %s is %q, want known", p.Fingerprint, p.Status)
		}
	}
	a, b := fingerprints(first), fingerprints(second)
	for fp := range a {
		if _, ok := b[fp]; !ok {
			t.Errorf("fingerprint %s unstable across rescans", fp)
		}
	}
	for _, call := range h.board.calls {
		if call != "items.list" {
			t.Errorf("scan used board method %s", call)
		}
	}
}

// A scan of one repository reads that repository. A tracked document that is
// a symlink out of it is not read through, and a referenced path that climbs
// out of it is not looked up outside: it is reported as missing from the
// work tree, which is what it is.
func TestScanDocsStayInsideTheRepository(t *testing.T) {
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFiles(t, outside, map[string]string{
		"notes.md": "see `pkg/only_outside.go`\n",
		"real.go":  "package outside\n",
	})
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(root, outside)
	if err != nil {
		t.Fatal(err)
	}
	climb := "x/../" + filepath.ToSlash(rel) + "/real.go"
	gitIn(t, root, "init", "-q")
	writeFiles(t, root, map[string]string{"README.md": "see `" + climb + "`\n"})
	if err := os.Symlink(filepath.Join(outside, "notes.md"), filepath.Join(root, "link.md")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "init")

	view := newScanHarness(t, 0).scan(t, root, "")
	var climbed bool
	for _, p := range view.Proposals {
		if p.Category != scanDocs {
			continue
		}
		if strings.Contains(p.Description, "only_outside.go") {
			t.Errorf("a document outside the repository was read through a tracked symlink: %+v", p)
		}
		if strings.Contains(p.Description, climb) {
			climbed = true
		}
	}
	if !climbed {
		t.Errorf("a reference that leaves the repository was found outside it instead of reported missing: %+v", view.Proposals)
	}
}

func TestScanAttachesAFindingTheBoardAlreadyHolds(t *testing.T) {
	root := scanRepo(t, map[string]string{"README.md": "see `docs/gone.md`\n"})
	h := newScanHarness(t, 0)
	h.gaps = scanFakeGaps{rows: gapRows(1)}
	first := h.scan(t, root, "")
	var docFP, gapFP string
	for _, p := range first.Proposals {
		if p.Category == scanDocs {
			docFP = p.Fingerprint
		} else {
			gapFP = p.Fingerprint
		}
	}
	// The host filed the doc proposal with its marker line; a person filed the
	// gap one with the label only.
	h.board.items = []map[string]any{
		{"id": "ft-doc", "title": "Fix README", "description": "from a scan\n\n" + scanMarker + " " + docFP, "labels": []string{}},
		{"id": "ft-gap", "title": "Resolve gaps", "description": "", "labels": []string{scanLabelPrefix + gapFP}},
	}
	again := h.scan(t, root, "")
	if again.NewCount != 0 || again.AttachedCount != 2 || len(again.Proposals) != 2 {
		t.Fatalf("expected both attached: %+v", again.Proposals)
	}
	got := map[string]string{}
	for _, p := range again.Proposals {
		if p.Status != "attached" {
			t.Errorf("%s status %q", p.Fingerprint, p.Status)
		}
		got[p.Fingerprint] = p.AttachedItemID
	}
	if got[docFP] != "ft-doc" || got[gapFP] != "ft-gap" {
		t.Fatalf("attached to %v", got)
	}
}

func TestScanRefusesAnIndexItCannotTrustAndNamesDevmapStatus(t *testing.T) {
	root := scanRepo(t, map[string]string{"a.go": "package a\n"})
	reason := "source tree differs from the indexed generation"
	cases := map[string]*mapclient.Status{
		"empty store":     {HostContractVersion: 1, ReaderReady: true, QueryReady: true, IsFresh: true},
		"not query ready": func() *mapclient.Status { s := goodStatus(); s.QueryReady = false; return s }(),
		"degraded":        func() *mapclient.Status { s := goodStatus(); s.DegradedReason = &reason; return s }(),
		"stale":           func() *mapclient.Status { s := goodStatus(); s.IsFresh = false; return s }(),
		"no status":       nil,
	}
	for name, status := range cases {
		t.Run(name, func(t *testing.T) {
			h := newScanHarness(t, 0)
			h.dm.status = status
			_, e := h.runner.Start(context.Background(), json.RawMessage(fmt.Sprintf(`{"repo_root":%q}`, root)))
			if e == nil || e.Code != ErrDependency || !strings.Contains(e.Message, "devmap status") {
				t.Fatalf("refusal = %+v", e)
			}
			if len(h.runner.jobs) != 0 {
				t.Fatal("a refused scan left a job")
			}
		})
	}
}

func TestScanDeadCodeNeedsTwoIndependentSignalsAndStatesBoth(t *testing.T) {
	root := scanRepo(t, map[string]string{"pkg/a.go": "package pkg\n"})
	h := newScanHarness(t, 0)
	reason := "registered via reflection"
	h.dm.dead = mapclient.Result[mapclient.DeadSymbol]{Items: []mapclient.DeadSymbol{
		{FilePath: "pkg/a.go", Name: "Plugin.Handle", Confidence: 0.95, IsExempt: true, ExemptionReason: &reason},
		{FilePath: "pkg/a.go", Name: "RegisteredByName", Confidence: 0.95},
		{FilePath: "pkg/a.go", Name: "LowConfidence", Confidence: 0.5},
		{FilePath: "pkg/a.go", Name: "TrulyDead", Confidence: 0.9},
		{FilePath: "pkg/a.go", Name: "SearchTruncated", Confidence: 0.9},
	}}
	h.text.refs = map[string][]string{
		"RegisteredByName": {"pkg/a.go", "registry/table.go"}, // looked up by string elsewhere
		"TrulyDead":        {"pkg/a.go"},                      // only its definition
		"SearchTruncated":  {"pkg/a.go"},
	}
	h.text.truncated["SearchTruncated"] = true

	v := h.scan(t, root, "")
	var dead []ScanProposal
	for _, p := range v.Proposals {
		if p.Category == scanDead {
			dead = append(dead, p)
		}
	}
	if len(dead) != 1 {
		t.Fatalf("dead proposals = %+v", dead)
	}
	text := dead[0].Description
	for _, bad := range []string{"Plugin.Handle", "RegisteredByName", "LowConfidence", "SearchTruncated"} {
		if strings.Contains(text, bad) {
			t.Errorf("%s was proposed: %s", bad, text)
		}
	}
	if !strings.Contains(text, "TrulyDead") || !strings.Contains(text, "devmap dead_symbols confidence 0.90") || !strings.Contains(text, "no match outside its own definition") {
		t.Fatalf("proposal does not state both signals: %s", text)
	}
	if len(dead[0].Signals) != 2 || dead[0].Signals[0].Source == dead[0].Signals[1].Source {
		t.Fatalf("signals = %+v", dead[0].Signals)
	}
	if c := coverageOf(t, v, scanDead); c.Status == "ok" || v.Complete {
		t.Fatalf("an inconclusive search must not read as complete: %+v", c)
	}
}

func TestScanCarriesDevmapTruncationAndNeverReportsCompleteWithHoles(t *testing.T) {
	root := scanRepo(t, map[string]string{"a.go": "package a\n"})
	h := newScanHarness(t, 0)
	h.dm.dead = mapclient.Result[mapclient.DeadSymbol]{Hidden: 9}
	v := h.scan(t, root, "")
	c := coverageOf(t, v, scanDead)
	if c.Status != "truncated" || c.Hidden != 9 || v.Complete {
		t.Fatalf("coverage = %+v complete=%t", c, v.Complete)
	}
	h.dm.dead = mapclient.Result[mapclient.DeadSymbol]{Stale: true}
	v = h.scan(t, root, "")
	if c := coverageOf(t, v, scanDead); c.Status != "degraded" || v.Complete {
		t.Fatalf("stale answer coverage = %+v", c)
	}
}

func TestScanChangedSinceRevFlagsOnlySymbolsNoTestReaches(t *testing.T) {
	root := scanRepo(t, map[string]string{"covered.go": "package a\n", "bare.go": "package a\n", "capped.go": "package a\n", "x_test.go": "package a\n"})
	base := gitIn(t, root, "rev-parse", "HEAD")
	writeFiles(t, root, map[string]string{"covered.go": "package a // 2\n", "bare.go": "package a // 2\n", "capped.go": "package a // 2\n", "x_test.go": "package a // 2\n"})
	gitIn(t, root, "commit", "-q", "-am", "change")
	h := newScanHarness(t, 0)
	h.dm.affected = func(target string) string {
		switch target {
		case "bare.go":
			return affectedJSON(0, "")
		case "capped.go":
			return affectedJSON(0, `,"walk_incomplete":"depth cap"`)
		}
		return affectedJSON(2, "")
	}
	v := h.scan(t, root, fmt.Sprintf(`,"since":%q`, base))
	var files []string
	for _, p := range v.Proposals {
		if p.Category == scanUntested {
			files = append(files, p.Title)
		}
	}
	if len(files) != 1 || !strings.Contains(files[0], "bare.go") {
		t.Fatalf("untested proposals = %v", files)
	}
	if c := coverageOf(t, v, scanUntested); c.Status != "walk_incomplete" || !strings.Contains(c.Detail, "capped.go") {
		t.Fatalf("capped walk must be reported: %+v", c)
	}
	// Without since the category is skipped, never a clean zero.
	if c := coverageOf(t, h.scan(t, root, ""), scanUntested); c.Status != "skipped" {
		t.Fatalf("no since: %+v", c)
	}
}

func TestScanReportsUnavailableGapSourceAndTruncatedGapList(t *testing.T) {
	root := scanRepo(t, map[string]string{"a.go": "package a\n"})
	h := newScanHarness(t, 0)
	if c := coverageOf(t, h.scan(t, root, ""), scanGaps); c.Status != "unavailable" {
		t.Fatalf("nil source: %+v", c)
	}
	h.gaps = scanFakeGaps{rows: gapRows(1), truncated: true}
	if c := coverageOf(t, h.scan(t, root, ""), scanGaps); c.Status != "truncated" {
		t.Fatalf("truncated source: %+v", c)
	}
}

func TestScanProposalCapIsPerScanAndReported(t *testing.T) {
	root := scanRepo(t, map[string]string{"a.go": "package a\n"})
	h := newScanHarness(t, 0)
	h.gaps = scanFakeGaps{rows: gapRows(10)}
	v := h.scan(t, root, `,"max_proposals":3`)
	if len(v.Proposals) != 3 || v.OverCap != 7 || v.Complete {
		t.Fatalf("proposals=%d over_cap=%d complete=%t", len(v.Proposals), v.OverCap, v.Complete)
	}
	if _, e := h.runner.Start(context.Background(), json.RawMessage(fmt.Sprintf(`{"repo_root":%q,"max_proposals":101}`, root))); e == nil || e.Code != ErrBadRequest {
		t.Fatalf("cap above the maximum: %+v", e)
	}
}

func TestScanRootIsValidatedAndNeverTheSidecarCwd(t *testing.T) {
	repo := scanRepo(t, map[string]string{"a.go": "package a\n"})
	plain, _ := filepath.EvalSymlinks(t.TempDir())
	linkDir, _ := filepath.EvalSymlinks(t.TempDir())
	toRepo := filepath.Join(linkDir, "to-repo")
	toPlain := filepath.Join(linkDir, "to-plain")
	if err := os.Symlink(repo, toRepo); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(plain, toPlain); err != nil {
		t.Fatal(err)
	}
	h := newScanHarness(t, 0)
	for name, root := range map[string]string{
		"empty":            "",
		"relative":         "some/dir",
		"dot":              ".",
		"missing":          filepath.Join(plain, "nope"),
		"not a git tree":   plain,
		"subdirectory":     filepath.Join(repo, "sub"),
		"symlink to repo":  toRepo,
		"symlink to plain": toPlain,
		"file":             filepath.Join(repo, "a.go"),
		"nul":              repo + "\x00x",
	} {
		t.Run(name, func(t *testing.T) {
			if name == "subdirectory" {
				if err := os.MkdirAll(root, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := json.Marshal(GapScanParams{RepoRoot: root})
			_, e := h.runner.Start(context.Background(), raw)
			if e == nil || e.Code != ErrBadRoot {
				t.Fatalf("root %q: %+v", root, e)
			}
		})
	}
	for _, since := range []string{"--upload-pack=x", "-1", "nonexistent-rev", "a b"} {
		raw, _ := json.Marshal(GapScanParams{RepoRoot: repo, Since: since})
		if _, e := h.runner.Start(context.Background(), raw); e == nil || e.Code != ErrBadRequest {
			t.Errorf("since %q: %+v", since, e)
		}
	}
	if _, e := h.runner.Start(context.Background(), json.RawMessage(`{"repo_root":"/x","extra":1}`)); e == nil || e.Code != ErrBadRequest {
		t.Errorf("unknown field: %+v", e)
	}
	// The factories are only ever asked about the validated root.
	if v := h.scan(t, repo, ""); v.RepoRoot != repo {
		t.Fatalf("scanned %q", v.RepoRoot)
	}
	if len(h.roots) != 1 || h.roots[0] != repo {
		t.Fatalf("devmap built for %v, want only %s", h.roots, repo)
	}
}

func TestScanDocDriftFlagsOnlyPathsThatNoLongerExist(t *testing.T) {
	root := scanRepo(t, map[string]string{
		"README.md":        "run `scripts/build.sh`, see `docs/gone.md`, `docs/here.md` and `https://x/y.md`, also `src/old/mod.go`\n",
		"docs/here.md":     "see `docs/here.md` and `sibling.md`\n",
		"docs/sibling.md":  "ok\n",
		"scripts/build.sh": "#!/bin/sh\n",
		"CHANGELOG.md":     "removed `src/ancient/file.go`\n",
	})
	v := newScanHarness(t, 0).scan(t, root, "")
	var docs []ScanProposal
	for _, p := range v.Proposals {
		if p.Category == scanDocs {
			docs = append(docs, p)
		}
	}
	if len(docs) != 1 || !strings.Contains(docs[0].Title, "README.md") || !strings.Contains(docs[0].Title, "2 stale") {
		t.Fatalf("doc proposals = %+v", docs)
	}
	for _, want := range []string{"docs/gone.md", "src/old/mod.go"} {
		if !strings.Contains(docs[0].Description, want) {
			t.Errorf("missing %s: %s", want, docs[0].Description)
		}
	}
	if strings.Contains(docs[0].Description, "here.md") || strings.Contains(docs[0].Description, "ancient") {
		t.Errorf("false positive: %s", docs[0].Description)
	}
}

func TestScanRunsOffTheRequestAndCancellationStopsIt(t *testing.T) {
	root := scanRepo(t, map[string]string{"a.go": "package a\n"})
	h := newScanHarness(t, 0)
	started := make(chan struct{})
	h.dm.deadWait = func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	begin := time.Now()
	view := h.start(t, fmt.Sprintf(`{"repo_root":%q}`, root))
	if view.State != scanRunning || time.Since(begin) > 2*time.Second {
		t.Fatalf("Start did not answer immediately: %+v", view)
	}
	<-started
	if _, e := h.runner.Cancel(json.RawMessage(fmt.Sprintf(`{"scan_id":%q}`, view.ScanID))); e != nil {
		t.Fatal(e)
	}
	done := h.wait(t, view.ScanID)
	if done.State != scanCancelled || len(done.Proposals) != 0 || done.Complete {
		t.Fatalf("cancelled scan = %+v", done)
	}
	// The root is free again once the scan has ended.
	h.dm.deadWait = nil
	if v := h.scan(t, root, ""); v.State != scanDone {
		t.Fatalf("rescan after cancel: %+v", v)
	}
}

func TestScanRequestCancelledBeforeClaimStartsNothing(t *testing.T) {
	root := scanRepo(t, map[string]string{"a.go": "package a\n"})
	h := newScanHarness(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := h.runner.Start(ctx, json.RawMessage(fmt.Sprintf(`{"repo_root":%q}`, root))); e == nil {
		t.Fatal("a cancelled request was accepted")
	}
	if len(h.runner.jobs) != 0 || h.dm.deadRuns != 0 {
		t.Fatalf("work started: jobs=%d dead=%d", len(h.runner.jobs), h.dm.deadRuns)
	}
}

func TestScanDeadlineEndsAStuckScanWithPartialCoverage(t *testing.T) {
	root := scanRepo(t, map[string]string{"a.go": "package a\n"})
	h := newScanHarness(t, 50*time.Millisecond)
	h.gaps = scanFakeGaps{rows: gapRows(1)}
	h.dm.deadWait = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	v := h.scan(t, root, "")
	if v.State != scanDeadline || v.Complete {
		t.Fatalf("state = %s complete=%t", v.State, v.Complete)
	}
	if c := coverageOf(t, v, scanDocs); c.Status != "skipped" {
		t.Fatalf("category after the deadline: %+v", c)
	}
	if len(v.Proposals) != 1 {
		t.Fatalf("what was found before the deadline is kept: %+v", v.Proposals)
	}
}

func TestConcurrentScansOfOneRootShareOneJob(t *testing.T) {
	root := scanRepo(t, map[string]string{"a.go": "package a\n"})
	h := newScanHarness(t, 0)
	release := make(chan struct{})
	h.dm.deadWait = func(ctx context.Context) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	params := fmt.Sprintf(`{"repo_root":%q}`, root)
	var wg sync.WaitGroup
	ids := make([]string, 8)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, e := h.runner.Start(context.Background(), json.RawMessage(params))
			if e != nil {
				t.Errorf("start %d: %+v", i, e)
				return
			}
			v, ok := got.(ScanView)
			if !ok {
				t.Errorf("start %d answered %T, want ScanView", i, got)
				return
			}
			ids[i] = v.ScanID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("concurrent starts made different scans: %v", ids)
		}
	}
	if _, e := h.runner.Start(context.Background(), json.RawMessage(fmt.Sprintf(`{"repo_root":%q,"max_proposals":5}`, root))); e == nil || e.Code != ErrBusy {
		t.Fatalf("a different request for a running root: %+v", e)
	}
	close(release)
	h.wait(t, ids[0])
	if h.dm.deadRuns != 1 {
		t.Fatalf("dead-symbol query ran %d times for one shared scan", h.dm.deadRuns)
	}
}

func TestScanFailsClosedWhenTheBoardCannotBeRead(t *testing.T) {
	root := scanRepo(t, map[string]string{"README.md": "`docs/gone.md`\n"})
	h := newScanHarness(t, 0)
	broken := &errBoard{}
	h.runner.deps.Board = broken
	v := h.scan(t, root, "")
	if v.State != scanFailed || len(v.Proposals) != 0 || !strings.Contains(v.Error, "deduped") {
		t.Fatalf("scan with an unreadable board = %+v", v)
	}
}

type errBoard struct{}

func (errBoard) Workbench(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("store down")
}

func TestScanOpsAreServedOnTheirOwnLaneAndRefuseBadRoots(t *testing.T) {
	h := newScanHarness(t, 0)
	client := store.New(filepath.Join(t.TempDir(), "missing-store"), filepath.Join(t.TempDir(), "profile.sqlite"))
	t.Cleanup(client.Close)
	module := WorkbenchModule{Client: client, Scan: h.runner}
	responses := roundTrip(t, Options{Modules: []Module{module}},
		Request{ID: "hello", Op: OpHello},
		Request{ID: "rel", Op: OpScanGaps, Params: json.RawMessage(`{"repo_root":"relative/path"}`)},
		Request{ID: "get", Op: OpScanGet, Params: json.RawMessage(`{"scan_id":"nope"}`)},
		Request{ID: "cancel", Op: OpScanCancel, Params: json.RawMessage(`{"scan_id":"nope"}`)},
	)
	responses = byID(t, responses, "hello", "rel", "get", "cancel")
	for _, op := range []string{OpScanGaps, OpScanGet, OpScanCancel} {
		if !strings.Contains(string(responses[0].Result), `"`+op+`"`) {
			t.Errorf("hello does not advertise %s", op)
		}
	}
	for _, r := range responses[1:] {
		want := ErrBadRequest
		if r.ID == "rel" {
			want = ErrBadRoot
		}
		if r.OK || r.Error == nil || r.Error.Code != want {
			t.Errorf("%s: %+v", r.ID, r)
		}
	}
	if laneFor(OpScanGaps) != "scan" || laneFor(OpScanGet) != "scan" {
		t.Fatal("scan ops share a lane with board traffic")
	}
}
