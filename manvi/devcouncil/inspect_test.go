package devcouncil

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

const codeToolName = "devcouncil_code"

// batchableTools is what a devcouncil_batch call may carry, written out so the
// registry's Batchable flags are checked against a decision rather than
// against themselves.
var batchableTools = map[string]struct{}{
	"devcouncil_read_file": {}, "devcouncil_grep": {}, "devcouncil_find_files": {}, "devcouncil_list_dir": {},
	codeToolName: {}, "devcouncil_graph_query": {}, "devcouncil_graph_context": {}, "devcouncil_code_dead": {},
	"devcouncil_git_status": {}, "devcouncil_git_log": {}, "devcouncil_git_show": {},
	"devcouncil_git_branches": {}, "devcouncil_get_diff": {},
}

// TestWriteFileRefusesAnExistingFileTheAgentNeverRead is the audit-first rule:
// write_file replaces every byte, so an agent that never looked at a file may
// not replace it. Fails against the pre-change surface, which overwrote it.
func TestWriteFileRefusesAnExistingFileTheAgentNeverRead(t *testing.T) {
	f := newFixture(t)
	if out := f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"}); out["acquired"] != true {
		t.Fatalf("checkout = %v", out)
	}
	target := filepath.Join(f.root, "src", "calc.go")
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatal(err)
	}
	original := "package calc\n\nfunc Keep() {}\n"
	if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	res := f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": "package calc\n"})
	if !res.IsError || !strings.Contains(res.Text, "has not been read") {
		t.Fatalf("an unread overwrite was not refused: %+v", res)
	}
	if res.Blocked || res.Rule != "" {
		t.Errorf("the gate did not make this refusal, so it must carry no rule: %+v", res)
	}
	if got, _ := fs.ReadFile(os.DirFS(f.root), "src/calc.go"); string(got) != original {
		t.Fatalf("the refused write changed the file: %q", got)
	}

	if read := f.call("devcouncil_read_file", map[string]string{"path": "src/calc.go"}); read.IsError {
		t.Fatalf("read: %s", read.Text)
	}
	if res := f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": "package calc\n"}); res.IsError {
		t.Fatalf("the overwrite after a read was refused: %s", res.Text)
	}
}

// A file the agent created itself is one it knows; overwriting it again needs
// no read.
func TestWriteFileMayOverwriteWhatItWrote(t *testing.T) {
	f := newFixture(t)
	if out := f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"}); out["acquired"] != true {
		t.Fatalf("checkout = %v", out)
	}
	for _, content := range []string{"package calc\n", "package calc\n\nfunc A() {}\n"} {
		if res := f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": content}); res.IsError {
			t.Fatalf("write %q refused: %s", content, res.Text)
		}
	}
}

// A refused read is not an inspection.
func TestARefusedReadDoesNotCountAsInspection(t *testing.T) {
	f := newFixture(t)
	if res := f.call("devcouncil_read_file", map[string]string{"path": "does/not/exist.go"}); !res.IsError {
		t.Fatalf("reading a missing file succeeded: %s", res.Text)
	}
	if f.reg.inspected.has("does/not/exist.go") {
		t.Fatal("a failed read was recorded as an inspection")
	}
}

func TestReadFileLineRange(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.root, "lines.txt"), []byte("a\nb\nc\nd\ne\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name          string
		args          map[string]any
		want, wantErr string
	}{
		{"whole file", map[string]any{"path": "lines.txt"}, "a\nb\nc\nd\ne\n", ""},
		{"middle", map[string]any{"path": "lines.txt", "offset": 2, "limit": 2}, "[lines 2-3 of 5]\nb\nc\n", ""},
		{"to the end", map[string]any{"path": "lines.txt", "offset": 4}, "[lines 4-5 of 5]\nd\ne\n", ""},
		{"limit past the end", map[string]any{"path": "lines.txt", "offset": 5, "limit": 10}, "[lines 5-5 of 5]\ne\n", ""},
		{"limit only", map[string]any{"path": "lines.txt", "limit": 1}, "[lines 1-1 of 5]\na\n", ""},
		{"past the end", map[string]any{"path": "lines.txt", "offset": 6}, "", "past the end: the file has 5 lines"},
		{"negative", map[string]any{"path": "lines.txt", "offset": -1}, "", "must not be negative"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := f.call("devcouncil_read_file", c.args)
			if c.wantErr != "" {
				if !res.IsError || !strings.Contains(res.Text, c.wantErr) {
					t.Fatalf("got %+v, want an error containing %q", res, c.wantErr)
				}
				return
			}
			if res.IsError || res.Text != c.want {
				t.Fatalf("got %q (error %v), want %q", res.Text, res.IsError, c.want)
			}
		})
	}
}

func TestLineRangeWithoutATrailingNewline(t *testing.T) {
	got, err := lineRange("a\nb", 2, 0)
	if err != nil || got != "[lines 2-2 of 2]\nb" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := lineRange("", 1, 0); err == nil {
		t.Fatal("a range of an empty file must say it is past the end")
	}
}

// withholdGraphSources is the read gate on devcouncil_code: an excerpt of a
// refused file, or of no file at all, never reaches the model, and everything
// else in devmap's envelope does — including its completeness fields and its
// numbers, which a float round trip would rewrite.
func TestGraphAnswersWithholdSourceTheReadGateRefuses(t *testing.T) {
	raw := json.RawMessage(`{"truncated":true,"walk_incomplete":"stopped at depth 1","score":0.8141492009162903,
		"definitions":{"items":[
			{"file_path":"src/ok.go","source":"func Ok() {}","symbol_name":"Ok"},
			{"file_path":".env","source":"TOKEN=x","symbol_name":"TOKEN"},
			{"source_span":"orphan text"}
		]},
		"edges":[{"source_file":".env","target_file":"src/ok.go"}]}`)
	refuse := func(path string) string {
		if path == ".env" {
			return "path.secret"
		}
		return ""
	}
	out, withheld, err := withholdGraphSources(raw, refuse)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if withheld != 2 {
		t.Errorf("withheld %d excerpts, want 2: %s", withheld, text)
	}
	for _, leaked := range []string{"TOKEN=x", "orphan text"} {
		if strings.Contains(text, leaked) {
			t.Errorf("%q reached the answer: %s", leaked, text)
		}
	}
	for _, kept := range []string{"func Ok() {}", `"truncated":true`, "stopped at depth 1", "0.8141492009162903", "path.secret"} {
		if !strings.Contains(text, kept) {
			t.Errorf("%q was lost from the answer: %s", kept, text)
		}
	}
	if _, _, err := withholdGraphSources(json.RawMessage("not json"), refuse); err == nil {
		t.Error("an unparseable answer must be an error, not an empty one")
	}
}

func TestCodeQueryArguments(t *testing.T) {
	q, err := codeQueryFor("explore", " Loop ", "", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if q.Depth != 2 || q.Budget != codeDefaultBudget || q.Query != "Loop" || q.Kind != devmap.QueryExplore {
		t.Errorf("defaults not applied: %+v", q)
	}
	for _, bad := range []struct {
		kind, query, to string
		targets         []string
		depth, budget   int
	}{
		{kind: "search", query: "x"},
		{kind: "", query: "x"},
		{kind: "explore", query: "x", depth: codeMaxDepth + 1},
		{kind: "explore", query: "x", budget: codeMaxBudget + 1},
		{kind: "explore", query: ""},
		{kind: "trace", query: "a"},
		{kind: "affected"},
	} {
		if _, err := codeQueryFor(bad.kind, bad.query, bad.to, bad.targets, bad.depth, bad.budget); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
}

// devcouncil_code is core exactly when a navigator is attached: forced when it
// can answer, absent from narrowed profiles when it cannot.
func TestCodeToolIsCoreOnlyWithANavigator(t *testing.T) {
	without := &Registry{}
	if !without.codeTool().Extended {
		t.Error("with no navigator the graph tool must stay Extended")
	}
	with := &Registry{deps: Deps{Map: &devmap.Client{}}}
	if with.codeTool().Extended {
		t.Error("with a navigator the graph tool must be core")
	}
}

// The batch allowlist names registered, read-only tools, and nothing that
// changes what the next call may do.
func TestBatchableToolsAreRegisteredLookups(t *testing.T) {
	f := newFixture(t)
	byName := map[string]tools.Tool{}
	for _, tool := range f.reg.Tools() {
		byName[tool.Schema.Name] = tool
	}
	for name := range batchableTools {
		tool, ok := byName[name]
		if !ok {
			t.Errorf("%s is batchable but not registered; the list has drifted", name)
			continue
		}
		if !tool.ReadOnly || !tool.Batchable {
			t.Errorf("%s: ReadOnly=%v Batchable=%v", name, tool.ReadOnly, tool.Batchable)
		}
	}
	for name, tool := range byName {
		if _, listed := batchableTools[name]; tool.Batchable && !listed {
			t.Errorf("%s is marked Batchable but is not on the decided list", name)
		}
	}
	for _, never := range []string{tools.BatchToolName, tools.ActivateToolName, "devcouncil_spawn_subagents",
		"devcouncil_write_file", "devcouncil_exec_command", "devcouncil_fetch_url"} {
		if byName[never].Batchable {
			t.Errorf("%s must not run inside a batch", never)
		}
	}
	if _, ok := byName[tools.BatchToolName]; !ok {
		t.Error("the batch tool is not registered")
	}
}

// The batch runs lookups concurrently through one registry, so the lookups
// have to tolerate it. Run under -race.
func TestConcurrentLookupsThroughOneRegistry(t *testing.T) {
	f := newFixture(t)
	for i := range 4 {
		name := filepath.Join(f.root, "pkg", "f"+string(rune('a'+i))+".go")
		if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("package pkg\n\nfunc Needle() {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	calls := []struct {
		name string
		args map[string]any
	}{
		{"devcouncil_read_file", map[string]any{"path": "pkg/fa.go"}},
		{"devcouncil_read_file", map[string]any{"path": "pkg/fb.go", "offset": 2}},
		{"devcouncil_list_dir", map[string]any{"path": "pkg"}},
		{"devcouncil_find_files", map[string]any{"pattern": "*.go"}},
		{"devcouncil_grep", map[string]any{"pattern": "Needle"}},
	}
	var wg sync.WaitGroup
	errs := make(chan string, len(calls)*4)
	for range 4 {
		for _, c := range calls {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if res := f.call(c.name, c.args); res.IsError {
					errs <- c.name + ": " + res.Text
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// Reading one line of a file, or a batch view that cut the answer, is not
// reading it: neither may satisfy the overwrite guard.
func TestAPartialReadDoesNotSatisfyTheOverwriteGuard(t *testing.T) {
	f := newFixture(t)
	if out := f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"}); out["acquired"] != true {
		t.Fatalf("checkout = %v", out)
	}
	target := filepath.Join(f.root, "src", "calc.go")
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("package calc\n\nfunc Keep() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	write := func() tools.Result {
		return f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": "package calc\n"})
	}

	if res := f.call("devcouncil_read_file", map[string]any{"path": "src/calc.go", "limit": 1}); res.IsError {
		t.Fatalf("ranged read: %s", res.Text)
	}
	if res := write(); !res.IsError {
		t.Fatal("a one-line read satisfied the overwrite guard")
	}

	cut := tools.WithView(context.Background(), tools.View{MaxLines: 200, MaxBytes: 1 << 15, Filtered: true})
	if res := f.pipe.Run(cut, tools.Call{ID: "c2", Name: "devcouncil_read_file", Arguments: []byte(`{"path":"src/calc.go"}`)}); res.IsError {
		t.Fatalf("filtered read: %s", res.Text)
	}
	if res := write(); !res.IsError {
		t.Fatal("a read whose answer a batch filtered satisfied the overwrite guard")
	}

	whole := tools.WithView(context.Background(), tools.View{MaxLines: 200, MaxBytes: 1 << 15})
	if res := f.pipe.Run(whole, tools.Call{ID: "c3", Name: "devcouncil_read_file", Arguments: []byte(`{"path":"src/calc.go"}`)}); res.IsError {
		t.Fatalf("batched whole read: %s", res.Text)
	}
	if res := write(); res.IsError {
		t.Fatalf("a whole read inside a batch view did not satisfy the guard: %s", res.Text)
	}
}

// Every batchable tool runs concurrently with every other through one
// registry, because that is what a batch does. Run under -race. The graph
// tools have no navigator here and answer "unavailable"; their error path is
// still exercised concurrently, which is the property under test.
func TestEveryBatchableToolToleratesConcurrentCalls(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.root, "a.go"), []byte("package a\n\nfunc Needle() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := map[string]map[string]any{
		"devcouncil_read_file":     {"path": "a.go"},
		"devcouncil_grep":          {"pattern": "Needle"},
		"devcouncil_find_files":    {"pattern": "*.go"},
		"devcouncil_list_dir":      {"path": "."},
		codeToolName:               {"kind": "explore", "query": "Needle"},
		"devcouncil_graph_query":   {"query": "Needle"},
		"devcouncil_graph_context": {"files": []string{"a.go"}},
		"devcouncil_code_dead":     {},
		"devcouncil_git_status":    {},
		"devcouncil_git_log":       {},
		"devcouncil_git_show":      {},
		"devcouncil_git_branches":  {},
		"devcouncil_get_diff":      {},
	}
	if len(args) != len(batchableTools) {
		t.Fatalf("this test covers %d tools and the allowlist names %d", len(args), len(batchableTools))
	}
	mustAnswer := map[string]bool{"devcouncil_read_file": true, "devcouncil_grep": true,
		"devcouncil_find_files": true, "devcouncil_list_dir": true, "devcouncil_git_status": true}
	var wg sync.WaitGroup
	errs := make(chan string, len(args)*3)
	for range 3 {
		for name, a := range args {
			if _, ok := batchableTools[name]; !ok {
				t.Fatalf("%s is not on the allowlist", name)
			}
			raw, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				res := f.pipe.Run(context.Background(), tools.Call{ID: "c", Name: name, Arguments: raw})
				if mustAnswer[name] && res.IsError {
					errs <- name + ": " + res.Text
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// An edge's evidence.source is devmap's ResolutionSource (Resolver, Stored or
// Reconstructed): where the resolution came from, not file content. Stripping
// it threw away the provenance and told the agent an excerpt had been withheld
// when there was none. Only those three values, and only under "evidence",
// pass; anything else there is still treated as an excerpt.
func TestCodeGraphKeepsEvidenceProvenance(t *testing.T) {
	raw := []byte(`{"items":[
		{"edge_kind":"Calls","source_file":"a.go","evidence":{"kind":"ReceiverType","source":"Stored"}},
		{"edge_kind":"Calls","source_file":"b.go","evidence":{"kind":"ReceiverType","source":"Reconstructed"}},
		{"edge_kind":"Calls","source_file":"c.go","evidence":{"kind":"ReceiverType","source":"TOKEN=leak"}}],
		"definitions":{"items":[{"name":"Stored","source":"Stored"}]}}`)
	out, withheld, err := withholdGraphSources(raw, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if withheld != 2 {
		t.Errorf("withheld %d, want 2 (the non-enum evidence and the unattributed definition): %s", withheld, text)
	}
	for _, kept := range []string{`"source":"Stored"`, `"source":"Reconstructed"`} {
		if !strings.Contains(text, kept) {
			t.Errorf("evidence provenance %s was stripped: %s", kept, text)
		}
	}
	if strings.Contains(text, "TOKEN=leak") {
		t.Errorf("a non-enum evidence source reached the answer: %s", text)
	}
	if strings.Count(text, `"source":"Stored"`) != 1 {
		t.Errorf("the exemption must apply under evidence only: %s", text)
	}
}

// The ask_evidence pack is the one answer whose excerpts sit inside nested
// units (files[].units[].source_span). The fixture is real devmap 0.2.5 output,
// copied from DevCouncil's dc/devmap testdata, so this pins the shape the read
// gate will meet once the pin moves: every unit names its own file, so a
// readable excerpt survives and a refused one is withheld, never blanked
// wholesale for want of attribution.
func TestCodeGraphGatesAskEvidenceUnits(t *testing.T) {
	raw, err := fs.ReadFile(os.DirFS("testdata"), "devmap_ask_evidence.json")
	if err != nil {
		t.Fatal(err)
	}
	spans := strings.Count(string(raw), `"source_span":`)
	if spans == 0 {
		t.Fatal("fixture carries no source_span: it no longer exercises the gate")
	}
	kept, withheld, err := withholdGraphSources(raw, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if withheld != 0 || strings.Count(string(kept), `"source_span":`) != spans {
		t.Errorf("readable units lost excerpts: withheld %d of %d", withheld, spans)
	}
	_, withheld, err = withholdGraphSources(raw, func(string) string { return "path.secret_read" })
	if err != nil {
		t.Fatal(err)
	}
	if withheld != spans {
		t.Errorf("refused units: withheld %d, want all %d", withheld, spans)
	}
}

// A file of exactly the view's line count, ending in a newline, is shown whole
// by a batch and must count as read in full. Lines were counted as "\n"+1, so
// it counted as one line over and the write guard refused an overwrite of a
// file the agent had seen entirely.
func TestAFileThatFillsTheViewCountsAsRead(t *testing.T) {
	f := newFixture(t)
	if out := f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"}); out["acquired"] != true {
		t.Fatalf("checkout = %v", out)
	}
	target := filepath.Join(f.root, "src", "calc.go")
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(strings.Repeat("// line\n", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	view := tools.WithView(context.Background(), tools.View{MaxLines: 200, MaxBytes: 1 << 15})
	if res := f.pipe.Run(view, tools.Call{ID: "c1", Name: "devcouncil_read_file", Arguments: []byte(`{"path":"src/calc.go"}`)}); res.IsError {
		t.Fatalf("read: %s", res.Text)
	}
	if res := f.call("devcouncil_write_file", map[string]string{"path": "src/calc.go", "content": "package calc\n"}); res.IsError {
		t.Fatalf("a file shown whole under a 200-line view did not count as read: %s", res.Text)
	}
}
