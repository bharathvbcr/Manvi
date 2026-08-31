package devcouncil

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// The tests here cover what changed when the search moved onto ripgrep's
// engine. The contract that did not change — an unparseable pattern is an
// error, an alternation matches, an uncontained root is refused — is pinned in
// tools_test.go, and those tests run against this implementation unaltered.

// seedGrepTree writes a repository with one source file and one file the
// ignore rules exclude, both containing the same needle. Every test below
// distinguishes the two.
func seedGrepTree(t *testing.T, root string) {
	t.Helper()
	write := func(rel, contents string) {
		t.Helper()
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "build/\n")
	write("src/handler.go", "func handle() { grepNeedle() }\n")
	write("build/generated.go", "func generated() { grepNeedle() }\n")
}

func grepPaths(t *testing.T, payload map[string]any) []string {
	t.Helper()
	raw, ok := payload["matches"].([]any)
	if !ok {
		t.Fatalf("no matches array in payload: %v", payload)
	}
	var out []string
	for _, entry := range raw {
		match, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("match is not an object: %v", entry)
		}
		out = append(out, match["path"].(string))
	}
	return out
}

// TestGrepHonoursIgnoreRulesAndSaysThatItDid is the behaviour change, stated
// twice: once in what comes back, and once in the field that lets a caller
// report which repository was actually searched.
//
// The change is worth a test rather than a note because it is the kind that
// looks like a bug from the outside. A model that greps for a symbol it just
// wrote into a generated file and gets nothing has been told something true
// about the source tree and something false about the repository, and only the
// ignore_rules_applied field distinguishes the two readings.
func TestGrepHonoursIgnoreRulesAndSaysThatItDid(t *testing.T) {
	f := newFixture(t)
	seedGrepTree(t, f.root)

	payload := f.payload("devcouncil_grep", map[string]any{"pattern": "grepNeedle"})

	if got := grepPaths(t, payload); len(got) != 1 || got[0] != "src/handler.go" {
		t.Fatalf("an ignored build tree must not be searched by default, got %v", got)
	}
	if applied, _ := payload["ignore_rules_applied"].(bool); !applied {
		t.Fatalf("the reply must say the rules were applied: %v", payload)
	}
	if searched, _ := payload["files_searched"].(float64); searched < 1 {
		t.Fatalf("files_searched must report real coverage: %v", payload)
	}
}

// TestGrepIncludeIgnoredReachesTheBuildTree is the escape hatch. Without one,
// the ignore rules would be a wall rather than a default, and the only way to
// search generated code would be to leave the tool.
func TestGrepIncludeIgnoredReachesTheBuildTree(t *testing.T) {
	f := newFixture(t)
	seedGrepTree(t, f.root)

	payload := f.payload("devcouncil_grep", map[string]any{
		"pattern": "grepNeedle", "include_ignored": true,
	})

	got := grepPaths(t, payload)
	if len(got) != 2 {
		t.Fatalf("include_ignored must reach the ignored tree, got %v", got)
	}
	if applied, _ := payload["ignore_rules_applied"].(bool); applied {
		t.Fatalf("the reply must say the rules were off: %v", payload)
	}
}

// TestGrepNeverSearchesTheHarnessOwnState is the one exclusion include_ignored
// does not lift. .devcouncil holds the session log and the store; matches out
// of it are the agent reading its own notes back to itself and finding them
// persuasive.
func TestGrepNeverSearchesTheHarnessOwnState(t *testing.T) {
	f := newFixture(t)
	seedGrepTree(t, f.root)
	if err := os.MkdirAll(filepath.Join(f.root, ".devcouncil"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, ".devcouncil", "log.json"),
		[]byte(`{"note":"grepNeedle"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	payload := f.payload("devcouncil_grep", map[string]any{
		"pattern": "grepNeedle", "include_ignored": true,
	})

	for _, path := range grepPaths(t, payload) {
		if strings.HasPrefix(path, ".devcouncil/") || strings.HasPrefix(path, ".git/") {
			t.Fatalf("the harness's own state is never a search result: %v", path)
		}
	}
}

// TestGrepWithoutASearcherRefusesRatherThanReportingNoMatches is the invariant
// the whole boundary is built around, at the one place the boundary can fail
// wholesale.
//
// A harness whose analysis plane has not been built would otherwise answer
// every search with {"count":0}. That is indistinguishable from a repository
// that genuinely does not contain the symbol, and a model cannot recover from
// being told the second when the first is true.
func TestGrepWithoutASearcherRefusesRatherThanReportingNoMatches(t *testing.T) {
	f := newFixture(t)
	seedGrepTree(t, f.root)

	// The registry is the same one the fixture built; only the searcher goes
	// away, which is exactly the state of a checkout where cargo never ran.
	f.reg.deps.Grep = nil

	result := f.call("devcouncil_grep", map[string]any{"pattern": "grepNeedle"})
	if !result.IsError {
		t.Fatalf("a missing searcher must be an error, not an empty result: %q", result.Text)
	}
	if !strings.Contains(result.Text, "no search ran") {
		t.Fatalf("the error must say no search ran, got %q", result.Text)
	}
	if !strings.Contains(result.Text, "cargo build") {
		t.Fatalf("the error must name the remedy, got %q", result.Text)
	}
}

// TestGrepReportsWhatItCouldNotSearch keeps a capped sample from passing as
// complete coverage. A binary file is not searched, and a reply that stayed
// silent about it would let "no matches" mean "none in the files I felt like
// opening".
func TestGrepReportsWhatItCouldNotSearch(t *testing.T) {
	f := newFixture(t)
	seedGrepTree(t, f.root)
	if err := os.WriteFile(filepath.Join(f.root, "src", "blob.bin"),
		append([]byte("grepNeedle\n"), 0x00, 0x01, 0x02), 0o644); err != nil {
		t.Fatal(err)
	}

	payload := f.payload("devcouncil_grep", map[string]any{"pattern": "grepNeedle"})

	skipped, ok := payload["skipped"].(map[string]any)
	if !ok {
		t.Fatalf("an unsearched file must be reported: %v", payload)
	}
	if count, _ := skipped["binary"].(float64); count != 1 {
		t.Fatalf("the binary file must be counted: %v", skipped)
	}
	if note, _ := skipped["note"].(string); !strings.Contains(note, "not searched") {
		t.Fatalf("the note must say the result does not cover them: %v", skipped)
	}
}

// TestGrepStaysSilentWhenItSkippedNothing is the other half: the note means
// something only if a clean search does not carry it. A tree with nothing
// unsearchable in it must produce no skipped key at all, rather than a row of
// zeros that reads like a warning.
func TestGrepStaysSilentWhenItSkippedNothing(t *testing.T) {
	f := newFixture(t)
	seedGrepTree(t, f.root)

	payload := f.payload("devcouncil_grep", map[string]any{"pattern": "grepNeedle"})
	if _, present := payload["skipped"]; present {
		t.Fatalf("a search that skipped nothing must not carry the note: %v", payload)
	}
}

// TestGrepMatchesCaseInsensitivelyOnRequest covers the argument added with the
// engine. The inline (?i) form has always worked and is asserted beside it,
// because that is what a model reaches for and it must not have regressed.
func TestGrepMatchesCaseInsensitivelyOnRequest(t *testing.T) {
	f := newFixture(t)
	seedGrepTree(t, f.root)

	if payload := f.payload("devcouncil_grep", map[string]any{"pattern": "GREPNEEDLE"}); payload["count"].(float64) != 0 {
		t.Fatalf("case-sensitive is still the default: %v", payload)
	}
	if payload := f.payload("devcouncil_grep", map[string]any{
		"pattern": "GREPNEEDLE", "case_insensitive": true,
	}); payload["count"].(float64) == 0 {
		t.Fatalf("case_insensitive must match: %v", payload)
	}
	if payload := f.payload("devcouncil_grep", map[string]any{"pattern": "(?i)GREPNEEDLE"}); payload["count"].(float64) == 0 {
		t.Fatalf("the inline form must keep working: %v", payload)
	}
}

// TestGrepMatchesCarryTheFieldsTheModelReadsPins the result shape. The tool's
// whole value is that a match can be opened afterwards, which takes a path
// relative to the repository and a line number that is real.
func TestGrepMatchesCarryTheFieldsTheModelReads(t *testing.T) {
	f := newFixture(t)
	seedGrepTree(t, f.root)

	payload := f.payload("devcouncil_grep", map[string]any{"pattern": "grepNeedle"})
	matches := payload["matches"].([]any)
	if len(matches) != 1 {
		t.Fatalf("expected one match: %v", payload)
	}
	match := matches[0].(map[string]any)
	if match["path"] != "src/handler.go" {
		t.Fatalf("path must be repository-relative: %v", match)
	}
	if line, _ := match["line_number"].(float64); line != 1 {
		t.Fatalf("line_number must be the real line: %v", match)
	}
	if text, _ := match["line"].(string); !strings.Contains(text, "grepNeedle") {
		t.Fatalf("line must carry the matched text: %v", match)
	}
}

// TestGrepEmptyMatchListIsAnArrayNotNull is small and load-bearing. A nil Go
// slice renders as JSON null, and null is a different statement from "no
// matches" to anything that iterates the field.
func TestGrepEmptyMatchListIsAnArrayNotNull(t *testing.T) {
	f := newFixture(t)
	seedGrepTree(t, f.root)

	result := f.call("devcouncil_grep", map[string]any{"pattern": "NOTHING_MATCHES_THIS_XYZ"})
	if result.IsError {
		t.Fatalf("a real negative is not an error: %q", result.Text)
	}
	if strings.Contains(result.Text, `"matches":null`) {
		t.Fatalf("an empty match list must render as []: %q", result.Text)
	}
}

// TestFindFilesAndGrepAgreeAboutTheRepository is the invariant the two tools
// broke when only one of them moved onto ripgrep's ignore rules.
//
// For a while devcouncil_find_files reported `dist/generated.go` in a
// repository whose .gitignore excluded `dist/`, and devcouncil_grep would never
// open that file. Both answers were internally consistent and one of them was
// about a tree the agent was not editing. Nothing in either reply said which.
//
// This asserts the agreement through the tool surface rather than in the
// engine, because that is where a future second walker would be added.
func TestFindFilesAndGrepAgreeAboutTheRepository(t *testing.T) {
	f := newFixture(t)
	seedGrepTree(t, f.root)

	for _, includeIgnored := range []bool{false, true} {
		args := map[string]any{"include_ignored": includeIgnored}

		found := f.payload("devcouncil_find_files",
			merge(args, map[string]any{"pattern": "*.go", "max_results": 1000}))
		var listed []string
		for _, entry := range found["files"].([]any) {
			listed = append(listed, entry.(string))
		}

		searched := f.payload("devcouncil_grep",
			merge(args, map[string]any{"pattern": "grepNeedle", "max_results": 1000}))
		matched := grepPaths(t, searched)

		sort.Strings(listed)
		sort.Strings(matched)
		if strings.Join(listed, ",") != strings.Join(matched, ",") {
			t.Fatalf("include_ignored=%v: find_files sees %v, grep sees %v",
				includeIgnored, listed, matched)
		}
		if found["ignore_rules_applied"] != searched["ignore_rules_applied"] {
			t.Fatalf("include_ignored=%v: the two tools disagree about which mode they ran in",
				includeIgnored)
		}
	}
}

func merge(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// TestFindFilesWithoutASearcherRefusesRatherThanReportingAnEmptyRepository is
// the same contract grep has, at the tool that now shares its walk. An empty
// file list is a statement about the repository, and a harness that cannot walk
// must not be able to make one.
func TestFindFilesWithoutASearcherRefusesRatherThanReportingAnEmptyRepository(t *testing.T) {
	f := newFixture(t)
	seedGrepTree(t, f.root)
	f.reg.deps.Grep = nil

	result := f.call("devcouncil_find_files", map[string]any{"pattern": "*.go"})
	if !result.IsError {
		t.Fatalf("a missing searcher must be an error, not an empty listing: %q", result.Text)
	}
	if !strings.Contains(result.Text, "no listing ran") || !strings.Contains(result.Text, "cargo build") {
		t.Fatalf("the error must say nothing ran and name the remedy, got %q", result.Text)
	}
}

// TestASearchThatOpenedNoFilesSaysSo covers the one zero that carries no
// information about the repository and is otherwise identical to the zero that
// carries a great deal.
//
// It is reachable without anything being broken: a .gitignore of `*`, a path
// naming an empty directory, a tree whose every file is hidden. Unmarked, a
// model reads "not present" from a search that never opened a line.
func TestASearchThatOpenedNoFilesSaysSo(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.root, ".gitignore"), []byte("*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "code.go"), []byte("grepNeedle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	payload := f.payload("devcouncil_grep", map[string]any{"pattern": "grepNeedle"})
	if payload["count"].(float64) != 0 {
		t.Fatalf("everything is ignored, so nothing may match: %v", payload)
	}
	if marked, _ := payload["searched_nothing"].(bool); !marked {
		t.Fatalf("a search that opened no file must say so: %v", payload)
	}
	if note, _ := payload["note"].(string); !strings.Contains(note, "not evidence") {
		t.Fatalf("the note must deny that this is evidence about the repository: %v", payload)
	}

	// And include_ignored finds it, which is what the note tells the caller to
	// try — so the advice is not merely reassuring, it works.
	reached := f.payload("devcouncil_grep",
		map[string]any{"pattern": "grepNeedle", "include_ignored": true})
	if reached["count"].(float64) != 1 {
		t.Fatalf("the remedy the note names must actually find it: %v", reached)
	}
	if _, marked := reached["searched_nothing"]; marked {
		t.Fatalf("a search that opened files must not carry the marker: %v", reached)
	}
}

// TestAnOverLongPatternIsRefusedAtTheToolSurface keeps the engine's bound
// reachable through the layer a model actually calls, and keeps the refusal
// shaped like a refusal.
func TestAnOverLongPatternIsRefusedAtTheToolSurface(t *testing.T) {
	f := newFixture(t)
	seedGrepTree(t, f.root)

	result := f.call("devcouncil_grep", map[string]any{"pattern": strings.Repeat("a", 8193)})
	if !result.IsError {
		t.Fatalf("an over-long pattern must be refused: %q", result.Text)
	}
	if !strings.Contains(result.Text, "no search ran") {
		t.Fatalf("the refusal must not read as a negative result: %q", result.Text)
	}
}

// TestEveryToolThatEnumeratesTheRepositoryAgrees is the whole class, asserted
// at once.
//
// Three tools answer "what is in this repository": devcouncil_grep searches it,
// devcouncil_find_files globs it, and devcouncil_list_dir with recursive
// enumerates it. Each began with its own tree walk and its own hardcoded skip
// list, and the migration to ripgrep's ignore rules moved exactly one of them —
// so for a while all three could be asked the same question and give three
// answers, each internally consistent, at most one of them about the tree the
// agent was editing.
//
// This is the test that fails if a fourth walk is ever added beside them.
func TestEveryToolThatEnumeratesTheRepositoryAgrees(t *testing.T) {
	f := newFixture(t)
	seedGrepTree(t, f.root)

	for _, includeIgnored := range []bool{false, true} {
		args := map[string]any{"include_ignored": includeIgnored}

		searched := grepPaths(t, f.payload("devcouncil_grep",
			merge(args, map[string]any{"pattern": "grepNeedle", "max_results": 1000})))

		globbed := []string{}
		found := f.payload("devcouncil_find_files",
			merge(args, map[string]any{"pattern": "*.go", "max_results": 1000}))
		for _, entry := range found["files"].([]any) {
			globbed = append(globbed, entry.(string))
		}

		walked := []string{}
		listed := f.payload("devcouncil_list_dir", merge(args, map[string]any{"recursive": true}))
		for _, raw := range listed["entries"].([]any) {
			item := raw.(map[string]any)
			if isDir, _ := item["is_dir"].(bool); isDir {
				continue
			}
			if name := item["name"].(string); strings.HasSuffix(name, ".go") {
				walked = append(walked, name)
			}
		}

		sort.Strings(searched)
		sort.Strings(globbed)
		sort.Strings(walked)

		if strings.Join(searched, ",") != strings.Join(globbed, ",") {
			t.Fatalf("include_ignored=%v: grep sees %v, find_files sees %v",
				includeIgnored, searched, globbed)
		}
		if strings.Join(searched, ",") != strings.Join(walked, ",") {
			t.Fatalf("include_ignored=%v: grep sees %v, list_dir sees %v",
				includeIgnored, searched, walked)
		}
	}
}

// TestListDirFlatStillReportsTheLiteralDirectory keeps the deliberate exception
// honest.
//
// The recursive branch enumerates the repository and honours ignore rules. The
// flat branch answers a different question — what is actually in this directory
// — and an operator checking whether a build output directory exists is asking
// that one. If this ever starts hiding entries, the two branches have been
// wrongly unified.
func TestListDirFlatStillReportsTheLiteralDirectory(t *testing.T) {
	f := newFixture(t)
	seedGrepTree(t, f.root)

	listed := f.payload("devcouncil_list_dir", map[string]any{})
	var names []string
	for _, raw := range listed["entries"].([]any) {
		names = append(names, raw.(map[string]any)["name"].(string))
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "build") {
		t.Fatalf("the flat listing reports the directory as it is on disk, ignored or not: %v", names)
	}
	if _, present := listed["ignore_rules_applied"]; present {
		t.Fatalf("the flat listing applies no ignore rules and must not claim to: %v", listed)
	}
}

// TestListDirRecursiveReconstructsAncestorsAndTerminates is the regression test
// for a hang I wrote into the ancestor-reconstruction loop.
//
// The first version advanced its index with `strings.Index(path[idx+1:], "/") +
// idx + 1`. That returns -1 when no further separator exists, so the arithmetic
// resolved to the index it already held and the loop spun forever. A single
// path of `src/a.go` was enough. Nothing in the harness bounds a tool handler —
// it is pure computation between the gate and the reply — so the turn did not
// time out, it simply stopped, and the package test hit Go's ten-minute
// deadline with no failure message.
//
// The deadline below is what turns a hang into a failure with a name.
func TestListDirRecursiveReconstructsAncestorsAndTerminates(t *testing.T) {
	f := newFixture(t)
	seedGrepTree(t, f.root)
	// One file per directory depth, including the shallow case that hung.
	for _, rel := range []string{
		"top.go",
		"src/one.go",
		"src/deep/two.go",
		"src/deep/deeper/three.go",
	} {
		full := filepath.Join(f.root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("grepNeedle\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan map[string]any, 1)
	go func() { done <- f.payload("devcouncil_list_dir", map[string]any{"recursive": true}) }()

	var payload map[string]any
	select {
	case payload = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("devcouncil_list_dir did not return; the ancestor walk does not terminate")
	}

	dirs := map[string]bool{}
	files := map[string]bool{}
	for _, raw := range payload["entries"].([]any) {
		item := raw.(map[string]any)
		if isDir, _ := item["is_dir"].(bool); isDir {
			dirs[item["name"].(string)] = true
		} else {
			files[item["name"].(string)] = true
		}
	}

	// Every ancestor of a listed file, named once each.
	for _, want := range []string{"src", "src/deep", "src/deep/deeper"} {
		if !dirs[want] {
			t.Fatalf("ancestor %q missing from %v", want, dirs)
		}
	}
	// And a file at the root contributes no ancestor at all.
	if !files["top.go"] {
		t.Fatalf("a root-level file must still be listed: %v", files)
	}
	if dirs[""] {
		t.Fatal("the empty string is not a directory")
	}
}
