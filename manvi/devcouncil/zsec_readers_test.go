package devcouncil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/tools"
)

const secretBody = "API_KEY=sk-live-AUDITsecretVALUE0123456789\n"

// These tests are the failing-before set for the readers that skipped the read
// gate. Each leaves a credential where one reader can reach it and asserts that
// the bytes do not come back and that the result names what was withheld.

func requireNoSecret(t *testing.T, tool string, res tools.Result) {
	t.Helper()
	if strings.Contains(res.Text, "AUDITsecretVALUE") {
		t.Fatalf("%s returned a credential:\n%s", tool, res.Text)
	}
}

func payloadOf(t *testing.T, res tools.Result) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("unparseable payload: %v (%q)", err, res.Text)
	}
	return out
}

// withheldPaths reads {"withheld":{"paths":[{"path","rule"}]}} off a payload.
func withheldPaths(t *testing.T, payload map[string]any) map[string]string {
	t.Helper()
	w, _ := payload["withheld"].(map[string]any)
	if w == nil {
		t.Fatalf("the result does not say anything was withheld: %v", payload)
	}
	out := map[string]string{}
	entries, _ := w["paths"].([]any)
	for _, e := range entries {
		m, _ := e.(map[string]any)
		out[m["path"].(string)] = m["rule"].(string)
	}
	return out
}

func TestGetDiffWithholdsAnUntrackedSecret(t *testing.T) {
	f := newFixture(t)
	write(t, f.root, ".env", secretBody)
	write(t, f.root, "notes.md", "plain note\n")

	res := f.call("devcouncil_get_diff", map[string]any{})
	requireNoSecret(t, "get_diff", res)
	payload := payloadOf(t, res)
	if got := withheldPaths(t, payload); got[".env"] != "path.secret_read" {
		t.Fatalf("withheld = %v, want .env under path.secret_read", got)
	}
	if !strings.Contains(payload["diff"].(string), "plain note") {
		t.Errorf("an ordinary untracked file was withheld along with the secret: %v", payload["diff"])
	}
}

func TestGetDiffWithholdsATrackedSecretChange(t *testing.T) {
	f := newFixture(t)
	write(t, f.root, ".env", "OLD=1\n")
	run(t, f.root, "add", "-f", ".env")
	run(t, f.root, "commit", "-qm", "oops")
	write(t, f.root, ".env", secretBody)

	res := f.call("devcouncil_get_diff", map[string]any{})
	requireNoSecret(t, "get_diff", res)
	if got := withheldPaths(t, payloadOf(t, res)); got[".env"] == "" {
		t.Fatalf("withheld = %v", got)
	}
}

// The name notes.txt matches no secret pattern; its inode is .env's.
func TestHardLinkedSecretIsWithheldFromGrepAndDiff(t *testing.T) {
	f := newFixture(t)
	linkSecret(t, f.root, ".env", "notes.txt", secretBody)

	grep := f.call("devcouncil_grep", map[string]any{"pattern": "API_KEY", "include_ignored": true})
	requireNoSecret(t, "grep", grep)
	gp := payloadOf(t, grep)
	w, _ := gp["withheld"].(map[string]any)
	if w == nil {
		t.Fatalf("grep did not say it withheld anything: %v", gp)
	}
	rules, _ := w["rules"].(map[string]any)
	if rules["notes.txt"] != "path.aliased_read" {
		t.Errorf("grep withheld rules = %v, want notes.txt under path.aliased_read", rules)
	}

	diff := f.call("devcouncil_get_diff", map[string]any{})
	requireNoSecret(t, "get_diff", diff)
	if got := withheldPaths(t, payloadOf(t, diff)); got["notes.txt"] != "path.aliased_read" {
		t.Errorf("get_diff withheld = %v, want notes.txt under path.aliased_read", got)
	}
}

func TestGitShowWithholdsASecretCommittedInHistory(t *testing.T) {
	f := newFixture(t)
	write(t, f.root, ".env", secretBody)
	write(t, f.root, "app.go", "package app\n")
	run(t, f.root, "add", "-f", ".env", "app.go")
	run(t, f.root, "commit", "-qm", "add env and app")
	run(t, f.root, "rm", "-q", "--cached", ".env")
	run(t, f.root, "commit", "-qm", "remove env")

	res := f.call("devcouncil_git_show", map[string]any{"object": "HEAD~1"})
	requireNoSecret(t, "git_show", res)
	payload := payloadOf(t, res)
	if got := withheldPaths(t, payload); got[".env"] != "path.secret_read" {
		t.Fatalf("withheld = %v", got)
	}
	if !strings.Contains(payload["patch"].(string), "app.go") {
		t.Errorf("the rest of the commit was withheld with the secret: %v", payload["patch"])
	}

	// The removal commit's patch carries the old contents as deletions.
	res = f.call("devcouncil_git_show", map[string]any{"object": "HEAD"})
	requireNoSecret(t, "git_show HEAD", res)

	// rev:path prints a blob, not a diff. It is refused one way or another.
	res = f.call("devcouncil_git_show", map[string]any{"object": "HEAD~1:.env"})
	requireNoSecret(t, "git_show rev:path", res)
}

func TestReadFileDoesNotResolveAroundTheSecretGateThroughTheCwd(t *testing.T) {
	f := newFixture(t)
	write(t, f.root, "secrets/token.txt", secretBody)
	t.Chdir(filepath.Join(f.root, "secrets"))

	res := f.call("devcouncil_read_file", map[string]any{"path": "token.txt"})
	requireNoSecret(t, "read_file", res)
	if !res.IsError {
		t.Fatalf("a read of secrets/token.txt through the cwd was allowed: %s", res.Text)
	}
}

// contentReaders is every registered read-only tool that can return repository
// bytes, with the probe that proves it applies the read gate. noContent lists
// the read-only tools that return none, with the reason. A tool in neither
// list fails the test: adding a reader means choosing which kind it is.
func TestEveryContentReaderAppliesTheReadGate(t *testing.T) {
	f := newFixture(t)
	write(t, f.root, ".env", secretBody)
	write(t, f.root, "secrets/token.txt", secretBody)
	run(t, f.root, "add", "-f", ".env", "secrets/token.txt")
	run(t, f.root, "commit", "-qm", "history holds secrets")
	write(t, f.root, "untracked.env", "")
	write(t, f.root, ".env.local", secretBody)
	// A skill whose SKILL.md is a hard link to a secret file. Its own path is not
	// secret-shaped, so only the read gate's alias check stands between the
	// skill loader and the credential; the file is well-formed so a refusal
	// cannot come from the frontmatter parser instead.
	write(t, f.root, ".env.skill", "---\nname: leak\ndescription: d\n---\n"+secretBody)
	if err := os.MkdirAll(filepath.Join(f.root, ".agents", "skills", "leak"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(f.root, ".env.skill"),
		filepath.Join(f.root, ".agents", "skills", "leak", "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	probes := map[string]map[string]any{
		"devcouncil_load_skill": {"name": "leak"},
		"devcouncil_read_file":  {"path": ".env"},
		"devcouncil_grep":       {"pattern": "API_KEY", "include_ignored": true},
		"devcouncil_get_diff":   {},
		"devcouncil_git_show":   {"object": "HEAD"},
	}
	// graph_query returns source spans from the index, which needs the DevMap
	// binary; its gate is asserted at the source below.
	sourceGated := map[string]string{"devcouncil_graph_query": "navigate.go"}
	noContent := map[string]string{
		"devcouncil_activate_tools":     "tool catalogue",
		"devcouncil_code_dead":          "symbol names and paths, no source",
		"devcouncil_dev_inspect":        "devbridge runs a fixed read of the DevCouncil store, not the tree",
		"devcouncil_find_files":         "names only",
		"devcouncil_get_gaps":           "verifier findings; the scan runs over the unfiltered diff on purpose",
		"devcouncil_get_next_actions":   "verifier findings",
		"devcouncil_get_task":           "task store",
		"devcouncil_git_branches":       "ref names",
		"devcouncil_git_log":            "subjects only; no patch",
		"devcouncil_git_status":         "paths only",
		"devcouncil_graph_context":      "paths and areas only",
		"devcouncil_list_artifacts":     "artifact store, not the tree",
		"devcouncil_list_dir":           "names only",
		"devcouncil_next_task":          "task store",
		"devcouncil_policy_check_write": "a decision, no bytes",
		"devcouncil_search_tools":       "tool catalogue",
		"devcouncil_verify_task":        "verifier report; evidence is redacted by the verifier",
		"devcouncil_fetch_url":          "the network, not the tree",
		"mcp_list_resources":            "a third-party server's data, not the tree",
		"mcp_list_tools":                "a third-party server's data, not the tree",
		"mcp_read_resource":             "a third-party server's data, not the tree",
	}

	var unclassified []string
	for _, schema := range f.pipe.ReadOnlySchemas() {
		name := schema.Name
		_, probed := probes[name]
		_, gated := sourceGated[name]
		_, silent := noContent[name]
		if !probed && !gated && !silent {
			unclassified = append(unclassified, name)
		}
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Fatalf("read-only tools with no read-gate classification: %v — if one returns repository "+
			"bytes, route it through readRefusal/withholdDiff and add a probe here", unclassified)
	}

	for name, args := range probes {
		t.Run(name, func(t *testing.T) {
			if !f.pipe.Has(name) {
				t.Fatalf("%s is not registered", name)
			}
			res := f.call(name, args)
			requireNoSecret(t, name, res)
		})
	}
	for name, file := range sourceGated {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "r.readRefusal(") {
			t.Errorf("%s no longer consults the read gate in %s", name, file)
		}
	}
}
