package devcouncil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
)

// stubIndex is a devmap whose status, explore and impact answers are files the
// test rewrites between calls, so one session can watch the index fall behind
// its own writes and then catch up.
type stubIndex struct {
	dir string
}

func statusDoc(generation int, fresh bool) string {
	return fmt.Sprintf(`{"host_contract_version":1,"schema_version":29,"expected_schema_version":29,`+
		`"schema_relation":"current","reader_ready":true,"query_ready":true,"db_path":"db",`+
		`"generation_id":%d,"node_count":10,"edge_count":20,"is_fresh":%t,`+
		`"capabilities":{"explore":true,"impact":true,"trace":true,"affected":true}}`, generation, fresh)
}

const impactTwoCallers = `{"hidden":0,"items":[` +
	`{"edge_kind":"Calls","source_file":"b/b.go","source_symbol":"b/b.go::Bar","target_file":"src/calc.go","target_symbol":"src/calc.go::Add"},` +
	`{"edge_kind":"Calls","source_file":"c/c.go","source_symbol":"c/c.go::Baz","target_file":"src/calc.go","target_symbol":"src/calc.go::Add"},` +
	`{"edge_kind":"Calls","source_file":"src/calc.go","source_symbol":"src/calc.go::Twice","target_file":"src/calc.go","target_symbol":"src/calc.go::Add"}],` +
	`"resolution":"Available","shown":3,"total":3,"tokens_used":40,"truncated":false,` +
	`"source_freshness":{"fresh":null,"reason":"not checked"}}`

func newStubIndex(t *testing.T, f *fixture, generation int, fresh bool) *stubIndex {
	t.Helper()
	s := &stubIndex{dir: t.TempDir()}
	script := "#!/bin/sh\n" +
		"case \" $* \" in *\" --help \"*) printf '      --graph-output <PATH>\\n'; exit 0;; esac\n" +
		"case \"$2\" in\n" +
		"  status) cat " + filepath.Join(s.dir, "status") + ";;\n" +
		"  explore) cat " + filepath.Join(s.dir, "explore") + ";;\n" +
		"  impact) cat " + filepath.Join(s.dir, "impact") + ";;\n" +
		"  search) echo '{\"items\":[],\"hidden\":0}';;\n" +
		"  *) echo '{}'; exit 1;;\n" +
		"esac\n"
	bin := filepath.Join(s.dir, "devmap")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s.set(t, "status", statusDoc(generation, fresh))
	s.set(t, "impact", impactTwoCallers)
	f.reg.deps.Map = devmap.New(bin, f.root)
	f.reopenSession()
	return s
}

func (s *stubIndex) set(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func checkedOut(t *testing.T, f *fixture) {
	t.Helper()
	if out := f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"}); out["acquired"] != true {
		t.Fatalf("checkout = %v", out)
	}
}

func realExplore(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "devmap_explore_real.json"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(raw))
}

const emptyExplore = `{"definitions":{"hidden":0,"items":[],"resolution":"Available","shown":0,"total":0,"tokens_used":0,` +
	`"truncated":false,"source_freshness":{"fresh":null,"reason":"not checked"}},` +
	`"blast_radius":{"layers":{"hidden":0,"items":[],"resolution":"Available","shown":0,"total":0,"tokens_used":0,` +
	`"truncated":false,"source_freshness":{"fresh":null,"reason":"not checked"}},"seeds":[],"seeds_total":0,"total_impacted":0,"unmatched_targets":[]},` +
	`"budget":{"total":3000},"limit":10,"query":"Sub"}`

// A write's result names who calls into the file, from which generation, and
// says so when the index has not seen the write.
func TestWriteResultCarriesTheImpactSummary(t *testing.T) {
	f := newFixture(t)
	checkedOut(t, f)
	newStubIndex(t, f, 4, false)

	res := f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": "package calc\n"})
	if res.IsError {
		t.Fatalf("write refused: %s", res.Text)
	}
	for _, want := range []string{"wrote src/calc.go", "index generation 4", "2 caller(s) of src/calc.go",
		"b/b.go::Bar", "c/c.go::Baz", "The index is stale"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("write result lacks %q:\n%s", want, res.Text)
		}
	}
	if strings.Contains(res.Text, "src/calc.go::Twice") {
		t.Errorf("a caller inside the written file was listed as an outside caller:\n%s", res.Text)
	}
	if len(res.Wrote) != 1 || res.Wrote[0] != "src/calc.go" {
		t.Errorf("Wrote = %v", res.Wrote)
	}
}

func TestPatchResultCarriesTheImpactSummary(t *testing.T) {
	f := newFixture(t)
	checkedOut(t, f)
	if res := f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": "package calc\n// a\n"}); res.IsError {
		t.Fatal(res.Text)
	}
	newStubIndex(t, f, 5, true)
	res := f.call("devcouncil_patch_file", map[string]any{
		"path": "src/calc.go", "target_content": "// a", "replacement_content": "// b"})
	if res.IsError {
		t.Fatalf("patch refused: %s", res.Text)
	}
	if !strings.Contains(res.Text, "patched src/calc.go") || !strings.Contains(res.Text, "index generation 5") ||
		!strings.Contains(res.Text, "b/b.go::Bar") {
		t.Errorf("patch result has no impact summary:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "stale") {
		t.Errorf("a fresh index was described as stale:\n%s", res.Text)
	}
}

func TestWriteImpactCapsTheCallers(t *testing.T) {
	f := newFixture(t)
	checkedOut(t, f)
	s := newStubIndex(t, f, 4, true)
	var items []string
	for i := 0; i < 25; i++ {
		items = append(items, fmt.Sprintf(`{"source_file":"p/f%02d.go","source_symbol":"p/f%02d.go::Fn%02d","target_file":"src/calc.go"}`, i, i, i))
	}
	s.set(t, "impact", `{"items":[`+strings.Join(items, ",")+`],"resolution":"Available","shown":25,"total":40,"hidden":15,"tokens_used":300,"truncated":true}`)
	res := f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": "package calc\n"})
	if !strings.Contains(res.Text, "25 caller(s)") || !strings.Contains(res.Text, "and 15 more") ||
		!strings.Contains(res.Text, "lower bound") {
		t.Errorf("cap or truncation not stated:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "Fn10") {
		t.Errorf("more than 10 callers were named:\n%s", res.Text)
	}
	if extra := len(res.Text) - len("wrote src/calc.go (14 bytes)"); extra > writeImpactMaxBytes+8 {
		t.Errorf("the summary is %d bytes, past the %d bound", extra, writeImpactMaxBytes)
	}
}

// An index that cannot answer leaves the write intact and says the check did
// not run, rather than saying nothing or reporting no callers.
func TestWriteImpactSaysWhenTheCheckDidNotRun(t *testing.T) {
	f := newFixture(t)
	checkedOut(t, f)
	s := newStubIndex(t, f, 4, true)
	s.set(t, "status", `{"generation_id":0}`)
	res := f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": "package calc\n"})
	if res.IsError || !strings.Contains(res.Text, "wrote src/calc.go") {
		t.Fatalf("an unavailable index failed the write: %+v", res)
	}
	if !strings.Contains(res.Text, "impact check did not run") || !strings.Contains(res.Text, "unknown") {
		t.Errorf("the write result does not say the impact check did not run:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "no callers") {
		t.Errorf("an unavailable index was reported as having no callers:\n%s", res.Text)
	}
}

// With no navigator the write result is exactly what it was.
func TestWriteResultIsUnchangedWithoutAnIndex(t *testing.T) {
	f := newFixture(t)
	checkedOut(t, f)
	res := f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": "package calc\n"})
	if strings.Contains(res.Text, "impact") {
		t.Errorf("a session with no index got an impact line:\n%s", res.Text)
	}
}

// After a write the index is behind. An explore for the new function answers
// empty; the notice comes first and says the empty answer is not a negative.
func TestCodeAnswerLeadsWithAStaleNoticeAfterAWrite(t *testing.T) {
	f := newFixture(t)
	checkedOut(t, f)
	s := newStubIndex(t, f, 4, false)
	s.set(t, "explore", emptyExplore)
	if res := f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": "package calc\nfunc Sub() {}\n"}); res.IsError {
		t.Fatal(res.Text)
	}

	res := f.call(codeToolName, map[string]string{"kind": "explore", "query": "Sub"})
	if res.IsError {
		t.Fatalf("explore failed: %s", res.Text)
	}
	if !strings.HasPrefix(res.Text, "{\n  \"index_notice\": ") {
		t.Fatalf("the notice is not the first field:\n%.400s", res.Text)
	}
	head := res.Text[:strings.Index(res.Text, "\"answer\"")]
	for _, want := range []string{"generation 4", "src/calc.go", "not a negative"} {
		if !strings.Contains(head, want) {
			t.Errorf("notice lacks %q:\n%s", want, head)
		}
	}
	if p := payloadOf(t, res); p["stale"] != true {
		t.Errorf("stale flag lost: %v", p["stale"])
	}

	// The index catches up: a newer generation and a fresh tree, so no notice.
	s.set(t, "status", statusDoc(5, true))
	res = f.call(codeToolName, map[string]string{"kind": "explore", "query": "Sub"})
	if strings.Contains(res.Text, "index_notice") {
		t.Errorf("a fresh index still carried the notice:\n%.400s", res.Text)
	}
}

// A path written under an earlier generation than the current one is in the
// index; only the paths the current generation cannot contain are named.
func TestStaleNoticeNamesOnlyPathsTheIndexHasNotSeen(t *testing.T) {
	f := newFixture(t)
	checkedOut(t, f)
	s := newStubIndex(t, f, 4, false)
	s.set(t, "explore", emptyExplore)
	if res := f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": "package calc\n"}); res.IsError {
		t.Fatal(res.Text)
	}
	// The index is rebuilt (generation 5) but some other file differs, so it is
	// still not fresh. src/calc.go was in that build.
	s.set(t, "status", statusDoc(5, false))
	res := f.call(codeToolName, map[string]string{"kind": "explore", "query": "Sub"})
	head := res.Text[:strings.Index(res.Text, "\"answer\"")]
	if !strings.Contains(head, "behind the tree") || strings.Contains(head, "src/calc.go") {
		t.Errorf("notice should be generic and omit the seen path:\n%s", head)
	}
}

func TestWrittenSetBoundsAndSaysWhatItDropped(t *testing.T) {
	var s writtenSet
	for i := 0; i < maxWrittenPaths+5; i++ {
		s.record(fmt.Sprintf("p/f%d.go", i), 1)
	}
	paths, dropped := s.unseen(1, false)
	if len(paths) != maxWrittenPaths || dropped != 5 {
		t.Fatalf("kept %d dropped %d, want %d and 5", len(paths), dropped, maxWrittenPaths)
	}
	if paths, dropped = s.unseen(1, true); paths != nil || dropped != 0 {
		t.Fatalf("a fresh index left %d paths and %d dropped", len(paths), dropped)
	}
}
