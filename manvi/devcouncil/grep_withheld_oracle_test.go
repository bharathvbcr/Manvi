package devcouncil

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The read gate removes credential files from a grep result. If anything the
// tool reports about those files depends on the pattern, the removal is
// decorative: `^TOKEN=[a-m]` answers "is the first character of the secret in
// a-m" by whether the withheld count is 1 or 0, and bisecting the class
// recovers the value. These tests hold the invariant directly: two patterns
// that differ only in whether they match inside a refused file must produce
// byte-identical output once the pattern echo is set aside.

// grepBytes runs the grep tool and returns the reply with the pattern echo
// removed, which is the one field that legitimately differs between the two
// patterns being compared.
func grepBytes(t *testing.T, f *fixture, args map[string]any) string {
	t.Helper()
	res := f.call("devcouncil_grep", args)
	if res.IsError {
		t.Fatalf("grep %v failed: %s", args, res.Text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("unparseable grep reply: %v (%q)", err, res.Text)
	}
	delete(out, "pattern")
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// requireSameOutput fails when the two patterns, one matching inside the
// refused file and one not, are distinguishable from what the tool printed.
func requireSameOutput(t *testing.T, f *fixture, matching, nonMatching string, extra map[string]any) {
	t.Helper()
	args := func(pattern string) map[string]any {
		out := map[string]any{"pattern": pattern, "include_ignored": true}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	hit := grepBytes(t, f, args(matching))
	miss := grepBytes(t, f, args(nonMatching))
	if hit != miss {
		t.Fatalf("grep output depends on whether the pattern matches inside a withheld file:\n"+
			"  matching:     %s\n  non-matching: %s", hit, miss)
	}
}

func TestGrepOutputDoesNotDependOnMatchesInAWithheldEnvFile(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, ".env", "TOKEN=abc123\n")
	writeRepoFile(t, f.root, "src/a.go", "package src\n")

	requireSameOutput(t, f, `^TOKEN=[a-m]`, `^TOKEN=[n-z]`, nil)
}

// The reply still has to say that the read gate may have withheld something:
// a result that dropped a file without saying so is the failure the original
// note existed to prevent. What it says is derived from the files, not the
// matches.
func TestGrepStillSaysWhichFilesTheReadGateWithholds(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, ".env", "TOKEN=abc123\n")
	writeRepoFile(t, f.root, "src/a.go", "package src\n")

	// A pattern matching nothing anywhere: the withheld note must still appear.
	payload := f.payload("devcouncil_grep", map[string]any{"pattern": "zzzzzz_nowhere", "include_ignored": true})
	withheld := jsonObj(t, payload["withheld"], "withheld")
	rules := jsonObj(t, withheld["rules"], "withheld.rules")
	if rules[".env"] != "path.secret_read" {
		t.Errorf("withheld rules = %v, want .env under path.secret_read", rules)
	}
	if _, leaks := withheld["matches"]; leaks {
		t.Errorf("withheld still reports a match count: %v", withheld)
	}
	if note := jsonStr(t, withheld["note"], "withheld.note"); !strings.Contains(note, "does not cover") {
		t.Errorf("note does not say the result is incomplete: %q", note)
	}
}

// A file refused by the hard-link rule is withheld by an inode property, not a
// name, so it is its own rule class to check.
func TestGrepOutputDoesNotDependOnMatchesInAHardLinkedFile(t *testing.T) {
	f := newFixture(t)
	linkSecret(t, f.root, ".env", "notes.txt", "TOKEN=abc123\n")
	writeRepoFile(t, f.root, "src/a.go", "package src\n")

	requireSameOutput(t, f, `^TOKEN=[a-m]`, `^TOKEN=[n-z]`, nil)

	payload := f.payload("devcouncil_grep", map[string]any{"pattern": "TOKEN", "include_ignored": true})
	rules := jsonObj(t, jsonObj(t, payload["withheld"], "withheld")["rules"], "withheld.rules")
	if rules["notes.txt"] != "path.aliased_read" {
		t.Errorf("withheld rules = %v, want notes.txt under path.aliased_read", rules)
	}
}

// dcgrep stops at max_results smallest (path, line) keys, and ".env" sorts
// before "src/". Matches inside the refused file used to occupy the slots, so
// the visible match fell off the end and `count`, `truncated` and `limit`
// changed with the pattern.
func TestGrepTruncationCannotBeInfluencedByWithheldMatches(t *testing.T) {
	f := newFixture(t)
	var env strings.Builder
	for i := range 60 {
		fmt.Fprintf(&env, "TOKEN=a%02d\n", i)
	}
	writeRepoFile(t, f.root, ".env", env.String())
	writeRepoFile(t, f.root, "src/a.go", "package src // marker\n")

	requireSameOutput(t, f, `marker|^TOKEN=[a-m]`, `marker|^TOKEN=[n-z]`, map[string]any{"max_results": 1})

	// And the visible match is still delivered, not crowded out.
	payload := f.payload("devcouncil_grep", map[string]any{
		"pattern": `marker|^TOKEN=`, "include_ignored": true, "max_results": 1,
	})
	if got := grepPaths(t, payload); len(got) != 1 || got[0] != "src/a.go" {
		t.Errorf("matches = %v, want the one visible match src/a.go", got)
	}
	if _, truncated := payload["truncated"]; truncated {
		t.Errorf("a result holding exactly the visible match was reported truncated: %v", payload)
	}
}

// A cap that visible matches really do hit is still reported.
func TestGrepStillReportsATruncationVisibleMatchesCause(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, ".env", "TOKEN=abc\n")
	writeRepoFile(t, f.root, "src/a.go", "marker one\nmarker two\nmarker three\n")

	payload := f.payload("devcouncil_grep", map[string]any{
		"pattern": "marker", "include_ignored": true, "max_results": 2,
	})
	if payload["truncated"] != true || payload["limit"] != float64(2) {
		t.Errorf("a capped search did not say so: %v", payload)
	}
	if got := grepPaths(t, payload); len(got) != 2 {
		t.Errorf("matches = %v, want 2", got)
	}
}

// files_searched is part of the reply too. It has to count the same files
// whatever the pattern did inside a refused one.
func TestGrepFilesSearchedDoesNotDependOnAWithheldFile(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, ".env", "TOKEN=abc123\n")
	writeRepoFile(t, f.root, "a.txt", "one\n")
	writeRepoFile(t, f.root, "sub/b.txt", "two\n")

	requireSameOutput(t, f, `^TOKEN=[a-m]`, `^TOKEN=[n-z]`, map[string]any{"max_results": 1})
}

// A refused file in a subdirectory, searched from above it, partitions the
// search around it: everything else must still be found.
func TestGrepSearchesAroundAWithheldFileInANestedDirectory(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, "cfg/.env", "TOKEN=abc123 needle\n")
	writeRepoFile(t, f.root, "cfg/app.go", "needle in app\n")
	writeRepoFile(t, f.root, "cfg/deep/x.go", "needle deep\n")
	writeRepoFile(t, f.root, "lib/y.go", "needle lib\n")
	writeRepoFile(t, f.root, "top.go", "needle top\n")

	payload := f.payload("devcouncil_grep", map[string]any{"pattern": "needle", "include_ignored": true})
	got := map[string]bool{}
	for _, p := range grepPaths(t, payload) {
		got[p] = true
	}
	for _, want := range []string{"cfg/app.go", "cfg/deep/x.go", "lib/y.go", "top.go"} {
		if !got[want] {
			t.Errorf("%s was lost when the search was partitioned around cfg/.env: %v", want, got)
		}
	}
	if got["cfg/.env"] {
		t.Error("the refused file's match reached the model")
	}
	if payload["count"] != float64(4) {
		t.Errorf("count = %v, want 4", payload["count"])
	}
}

// A directory holding one refused file makes each sibling its own search, so a
// large one exceeds the fan-out bound. That is refused outright, with the
// numbers, rather than run as a sample: the message is pinned because it is the
// documented behaviour of the bound.
func TestGrepRefusesAFanOutPastTheSubsearchBound(t *testing.T) {
	f := newFixture(t)
	for i := range grepMaxSubsearches + 88 {
		writeRepoFile(t, f.root, fmt.Sprintf("config/f%d.txt", i), "x\n")
	}
	writeRepoFile(t, f.root, "config/.env", "TOKEN=abc\n")

	res := f.call("devcouncil_grep", map[string]any{"pattern": "x", "include_ignored": true})
	if !res.IsError {
		t.Fatalf("a search needing %d sub-searches ran: %s", grepMaxSubsearches+88, res.Text)
	}
	want := "working around 1 withheld file(s) would take 600 separate searches, " +
		"over the limit of 512; search a narrower path — this is not a negative result, no search ran"
	if !strings.Contains(res.Text, want) {
		t.Errorf("refusal = %q, want it to contain %q", res.Text, want)
	}
}
