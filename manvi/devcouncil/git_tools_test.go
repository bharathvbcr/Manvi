package devcouncil

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
)

// The git tools shell out to git, so — like gitdiff_test.go — they are tested
// against real repositories. The fixture in tools_test.go already builds one;
// these tests reuse it so the gate, the store and the repo are the same
// objects production wires together.

func gitHead(t *testing.T, root string) string {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func modifySeed(t *testing.T, f *fixture) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, "seed.txt"), []byte("seed\nchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestGitStatusReportsBranchAndWorkingTreeChanges covers the happy read: a
// modified tracked file must appear as unstaged, HEAD must be named, and an
// untracked file must land in its own list rather than being silently absent.
func TestGitStatusReportsBranchAndWorkingTreeChanges(t *testing.T) {
	f := newFixture(t)
	modifySeed(t, f)
	write(t, f.root, "notes/new.md", "wip\n")

	res := f.call("devcouncil_git_status", map[string]any{})
	if res.IsError {
		t.Fatalf("git_status failed: %s", res.Text)
	}
	var payload struct {
		Branch    string           `json:"branch"`
		Detached  bool             `json:"detached"`
		Head      map[string]any   `json:"head"`
		Staged    []map[string]any `json:"staged"`
		Unstaged  []map[string]any `json:"unstaged"`
		Untracked []string         `json:"untracked"`
	}
	if err := json.Unmarshal([]byte(res.Text), &payload); err != nil {
		t.Fatalf("unparseable status: %v (%q)", err, res.Text)
	}

	if payload.Branch == "" || payload.Detached {
		t.Errorf("branch = %q detached=%v; expected a named branch", payload.Branch, payload.Detached)
	}
	if sha, _ := payload.Head["sha"].(string); sha != gitHead(t, f.root) {
		t.Errorf("head = %v; want %s", payload.Head["sha"], gitHead(t, f.root))
	}
	found := false
	for _, entry := range payload.Unstaged {
		if entry["path"] == "seed.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("modified seed.txt missing from unstaged: %v", payload.Unstaged)
	}
	if len(payload.Staged) != 0 {
		t.Errorf("nothing was staged but staged = %v", payload.Staged)
	}
	if len(payload.Untracked) != 1 || payload.Untracked[0] != "notes/new.md" {
		t.Errorf("untracked = %v", payload.Untracked)
	}
}

// TestGitStatusParsesRenamesAndAwkwardNames pins the -z parsing: a rename is
// a two-path record, and paths containing spaces survive it. Splitting on
// whitespace turned such names into garbage once on the diff path already.
func TestGitStatusParsesRenamesAndAwkwardNames(t *testing.T) {
	f := newFixture(t)
	run(t, f.root, "mv", "seed.txt", "my renamed file.txt")
	run(t, f.root, "add", "-A")

	raw, err := porcelainStatus(t, f.root)
	if err != nil {
		t.Fatalf("porcelain status: %v", err)
	}
	parsed, err := parsePorcelain(raw)
	if err != nil {
		t.Fatalf("parsePorcelain: %v", err)
	}
	if len(parsed.Staged) != 1 {
		t.Fatalf("staged = %v, want exactly the rename", parsed.Staged)
	}
	entry := parsed.Staged[0]
	if entry["path"] != "my renamed file.txt" || entry["orig_path"] != "seed.txt" {
		t.Errorf("rename parsed as %v", entry)
	}
}

func porcelainStatus(t *testing.T, root string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "-C", root, "status", "--porcelain=v1", "-z", "-b")
	out, err := cmd.Output()
	return string(out), err
}

func TestParsePorcelainBranchHeaders(t *testing.T) {
	cases := []struct {
		name     string
		field    string
		branch   string
		ahead    int
		hasAhead bool
		detached bool
	}{
		{"tracking with counts", "## main...origin/main [ahead 2, behind 1]", "main", 2, true, false},
		{"plain branch", "## feature/x", "feature/x", 0, false, false},
		{"unborn repository", "## No commits yet on main", "main", 0, false, false},
		{"detached head", "## HEAD (no branch)", "HEAD", 0, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ahead, _, detached := parseBranchHeader(tc.field)
			if got != tc.branch || detached != tc.detached {
				t.Fatalf("branch=%q detached=%v, want %q/%v", got, detached, tc.branch, tc.detached)
			}
			if tc.hasAhead && (ahead == nil || *ahead != tc.ahead) {
				t.Errorf("ahead = %v, want %d", ahead, tc.ahead)
			}
		})
	}
}

// TestGitLogAndBranchesAndShow cover the remaining reads against the seeded
// history the fixture commits.
func TestGitLogAndBranchesAndShow(t *testing.T) {
	f := newFixture(t)

	logRes := f.payload("devcouncil_git_log", map[string]any{"max": 5})
	commits, _ := logRes["commits"].([]any)
	if len(commits) != 1 {
		t.Fatalf("commits = %v, want the single seed commit", commits)
	}
	first, _ := commits[0].(map[string]any)
	if first["subject"] != "seed" || first["sha"] != gitHead(t, f.root) {
		t.Errorf("commit = %v", first)
	}

	branchRes := f.payload("devcouncil_git_branches", map[string]any{})
	branches, _ := branchRes["branches"].([]any)
	current := 0
	for _, b := range branches {
		if m, ok := b.(map[string]any); ok && m["current"] == true {
			current++
		}
	}
	if current != 1 {
		t.Errorf("%d branches marked current: %v", current, branches)
	}

	showRes := f.payload("devcouncil_git_show", map[string]string{"object": gitHead(t, f.root)})
	if showRes["subject"] != "seed" {
		t.Errorf("show subject = %v", showRes["subject"])
	}
	if patch, _ := showRes["patch"].(string); !strings.Contains(patch, "+seed") {
		t.Errorf("patch does not contain the seed addition: %q", patch)
	}

	// An option-shaped argument is refused before git can interpret it.
	if res := f.call("devcouncil_git_show", map[string]string{"object": "--all"}); !res.IsError {
		t.Errorf("--all was accepted as an object: %s", res.Text)
	}
}

// grantCommand asks for — and expects — an agent-issued command override,
// which requires the operator flag that makes command rules agent-grantable.
func grantCommand(t *testing.T, f *fixture, target string) {
	t.Helper()
	out := f.payload("devcouncil_request_override", map[string]string{
		"command": target,
		"rule":    string(policy.RuleCommandNotAllowed),
		"reason":  "the task's work includes committing the change",
	})
	if out["granted"] != true {
		t.Fatalf("command override refused: %v", out)
	}
}

// TestGitStageAndCommitLifecycle drives the writes end to end under the
// strict posture with an explicit grant per command: without a lease the
// stage is refused; with the lease and grants both operations run through the
// same ladder exec_command answers to, the commit lands, and the result cites
// the new HEAD.
func TestGitStageAndCommitLifecycle(t *testing.T) {
	f := newFixtureWith(t, map[string]string{
		flags.HarnessPosture:      flags.PostureStrict,
		flags.GrantsAgentCommands: "true",
	})

	stageLine := renderCommandLine([]string{"git", "add", "--", "seed.txt"})
	commitLine := renderCommandLine([]string{"git", "commit", "-m", "update seed"})

	// No lease yet: the gate refuses exactly as it would refuse the shell
	// equivalent, naming the lease rule.
	blockedStage := f.call("devcouncil_git_stage", map[string]any{"paths": []string{"seed.txt"}})
	if !blockedStage.IsError {
		t.Fatalf("stage without a lease succeeded: %s", blockedStage.Text)
	}
	if rule, denied := policyDenial(blockedStage.Text); !denied || rule != string(policy.RuleCommandNoLease) {
		t.Fatalf("refusal = %q, want rule %s", blockedStage.Text, policy.RuleCommandNoLease)
	}

	checkout(t, f)

	// With a lease but no grant: still refused, by scope this time.
	noGrant := f.call("devcouncil_git_stage", map[string]any{"paths": []string{"seed.txt"}})
	if rule, denied := policyDenial(noGrant.Text); !denied || rule != string(policy.RuleCommandNotAllowed) {
		t.Fatalf("refusal = %q, want rule %s", noGrant.Text, policy.RuleCommandNotAllowed)
	}

	modifySeed(t, f)

	grantCommand(t, f, stageLine)
	staged := f.payload("devcouncil_git_stage", map[string]any{"paths": []string{"seed.txt"}})
	if staged["exit_code"] != float64(0) {
		t.Fatalf("stage = %v", staged)
	}

	grantCommand(t, f, commitLine)
	committed := f.payload("devcouncil_git_commit", map[string]string{"message": "update seed"})
	if committed["exit_code"] != float64(0) {
		t.Fatalf("commit = %v", committed)
	}
	head, _ := committed["head"].(map[string]any)
	if head == nil || head["subject"] != "update seed" {
		t.Errorf("commit did not report the new HEAD: %v", committed)
	}
	if head != nil && head["sha"] != gitHead(t, f.root) {
		t.Errorf("reported HEAD %v is not the new HEAD", head["sha"])
	}
	// The commit is real history now.
	if changed := gitHead(t, f.root); changed == "" {
		t.Error("HEAD unreadable after commit")
	}
}

// TestGitStageRejectsSecretPathsUnderEveryPosture pins the guard that runs
// before and independent of the gate: staging .env.local is refused even
// under yolo, because a demoted soft denial is a decision about scope while
// this is a statement about what belongs in history at all.
func TestGitStageRejectsSecretPathsUnderEveryPosture(t *testing.T) {
	for _, posture := range []string{flags.PostureDev, flags.PostureStrict, flags.PostureYolo} {
		t.Run(posture, func(t *testing.T) {
			f := newFixtureWith(t, map[string]string{flags.HarnessPosture: posture})
			write(t, f.root, ".env.local", "TOKEN=secret\n")

			// Even a valid lease does not authorise this.
			checkout(t, f)
			res := f.call("devcouncil_git_stage", map[string]any{"paths": []string{".env.local"}})
			if !res.IsError || res.Severity != string(policy.Hard) || res.Rule != string(policy.RuleSecretPath) {
				t.Fatalf("stage of a secret path under %s returned %s (rule=%q severity=%q)",
					posture, res.Text, res.Rule, res.Severity)
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(res.Text), &payload); err != nil {
				t.Fatalf("refusal is unparseable: %v (%q)", err, res.Text)
			}
			if paths, _ := payload["paths"].([]any); len(paths) != 1 || paths[0] != ".env.local" {
				t.Errorf("refusal names %v, want the offending path", payload["paths"])
			}
		})
	}
}

// TestGitCommitRefusesSecretAlreadyStaged covers the other direction: the
// index was filled by something that did not use devcouncil_git_stage. The
// commit re-checks the staged set from the index itself.
func TestGitCommitRefusesSecretAlreadyStaged(t *testing.T) {
	f := newFixtureWith(t, map[string]string{
		flags.HarnessPosture:      flags.PostureYolo,
		flags.GrantsAgentCommands: "true",
	})
	write(t, f.root, "id_rsa", "private key material\n")
	run(t, f.root, "add", "id_rsa")
	checkout(t, f)

	res := f.call("devcouncil_git_commit", map[string]string{"message": "oops"})
	if !res.IsError || res.Rule != string(policy.RuleSecretPath) {
		t.Fatalf("commit over a staged key returned %s (rule=%q)", res.Text, res.Rule)
	}
	// And nothing landed: HEAD is still the seed commit.
	if n := headCount(t, f.root); n != 1 {
		t.Errorf("a commit escaped the refusal: %d commits exist", n)
	}
}

func headCount(t *testing.T, root string) int {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "git", "-C", root, "rev-list", "--count", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-list --count: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parsing count %q: %v", out, err)
	}
	return n
}

// TestGitCommitFailsLoudlyWhenNothingStaged distinguishes "the gate said no"
// from "git ran and failed". An empty commit attempt must come back as an
// error carrying git's own output, not as a policy refusal.
func TestGitCommitFailsLoudlyWhenNothingStaged(t *testing.T) {
	f := newFixtureWith(t, map[string]string{
		flags.HarnessPosture:      flags.PostureStrict,
		flags.GrantsAgentCommands: "true",
	})
	checkout(t, f)
	grantCommand(t, f, renderCommandLine([]string{"git", "commit", "-m", "nothing"}))

	res := f.call("devcouncil_git_commit", map[string]any{"message": "nothing"})
	if !res.IsError {
		t.Fatalf("empty commit reported success: %s", res.Text)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(res.Text), &payload); err != nil {
		t.Fatalf("failure is unparseable: %v (%q)", err, res.Text)
	}
	if code, _ := payload["exit_code"].(float64); code == 0 {
		t.Errorf("exit_code = %v, want non-zero", payload["exit_code"])
	}
	if _, hasRule := payload["rule"]; hasRule {
		t.Errorf("a git failure must not masquerade as a policy refusal: %v", payload["rule"])
	}
	// git writes "nothing to commit" on stdout, not stderr; either channel
	// explaining the failure satisfies the rule.
	explanation := fmt.Sprintf("%v %v", payload["output"], payload["git_stderr"])
	if !strings.Contains(explanation, "nothing to commit") &&
		!strings.Contains(explanation, "no changes added") {
		t.Errorf("neither output nor stderr explains the failure: %q", explanation)
	}
}

// TestRenderCommandLineMatchesShellQuoting keeps the evaluated command line
// faithful to what exec_command would have been asked: allowlists match
// against this text, so quoting differences are authorization differences.
func TestRenderCommandLineMatchesShellQuoting(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"git", "add", "--", "src/a.go"}, `git add -- src/a.go`},
		{[]string{"git", "add", "--", "my file.go"}, `git add -- "my file.go"`},
		{[]string{"git", "commit", "-m", `say "hi"`}, `git commit -m "say \"hi\""`},
	}
	for _, tc := range cases {
		if got := renderCommandLine(tc.argv); got != tc.want {
			t.Errorf("renderCommandLine(%v) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

type downMatcher struct{}

func (downMatcher) MatchAny([]string, string) (bool, error) {
	return false, fmt.Errorf("engine is down")
}

func (downMatcher) MatchAnyFold([]string, string) (bool, error) {
	return false, fmt.Errorf("engine is down")
}

// The secret check asks the gate's matcher, as the ladder does, and a
// matcher that cannot answer refuses the stage rather than reporting the
// path clean.
func TestGitStageRefusesWhenTheSecretCheckCannotAnswer(t *testing.T) {
	f := newFixture(t)
	modifySeed(t, f)
	f.reg.Gate().Matcher = downMatcher{}
	res := f.call("devcouncil_git_stage", map[string]any{"paths": []string{"seed.txt"}})
	if !res.IsError || !strings.Contains(res.Text, "secret-path check could not be determined") {
		t.Fatalf("stage with a failing matcher = %v %q, want the check reported as not run", res.IsError, res.Text)
	}
	if leaked, err := secretPaths(downMatcher{}, []string{"seed.txt"}); err == nil || leaked != nil {
		t.Fatalf("secretPaths = %v, %v; want an error", leaked, err)
	}
}

// TestGitShowRefusesObjectsThatAreNotCommits pins the class, not the `rev:path`
// spelling: whatever syntax names a blob or a tree, the tool answers with a
// commit or refuses. `git show -s` prints a blob's bytes whatever the format
// says, and the NUL-split of that output used to hand the remainder back as
// `subject` — the same read the command gate refuses as git_read_argument.
func TestGitShowRefusesObjectsThatAreNotCommits(t *testing.T) {
	f := newFixture(t)
	blob := "k\x00e\x00y\x00-\x00material-the-model-must-not-read\n"
	write(t, f.root, "deploy/cert.p12", blob)
	run(t, f.root, "add", "deploy/cert.p12")
	run(t, f.root, "commit", "-qm", "add cert")

	for _, object := range []string{
		"HEAD:deploy/cert.p12", "HEAD:seed.txt", "HEAD:deploy", "HEAD:", "HEAD^{tree}",
		"HEAD~1^{tree}", "HEAD^{blob}", ":deploy/cert.p12", ":0:deploy/cert.p12",
		"HEAD:deploy/cert.p12^{commit}", "HEAD~1:seed.txt",
	} {
		t.Run(object, func(t *testing.T) {
			res := f.call("devcouncil_git_show", map[string]string{"object": object})
			if !res.IsError {
				t.Fatalf("%q was accepted: %s", object, res.Text)
			}
			if strings.Contains(res.Text, "material-the-model-must-not-read") {
				t.Fatalf("%q leaked object bytes into the refusal: %q", object, res.Text)
			}
		})
	}

	// What remains accepted still works, and an annotated tag peels to its commit.
	run(t, f.root, "tag", "-a", "-m", "release", "v1")
	for _, object := range []string{"HEAD", "HEAD~1", "v1", gitHead(t, f.root)[:10]} {
		if out := f.payload("devcouncil_git_show", map[string]string{"object": object}); out["sha"] == nil || out["sha"] == "" {
			t.Errorf("%q no longer resolves to a commit: %v", object, out)
		}
	}
}

// TestGitCapsAreNamedInThePayload: the model reads Text. A cap noted only in
// Result.Degraded reaches the session log and nothing else, and a prefix reads
// as the whole.
func TestGitCapsAreNamedInThePayload(t *testing.T) {
	f := newFixture(t)

	log := f.payload("devcouncil_git_log", map[string]any{"max": maxGitLogCommits + 50})
	if notes, _ := log["degraded"].([]any); len(notes) != 1 || !noteSays(notes[0], "capped") {
		t.Errorf("git_log cap is not in the payload: %v", log)
	}

	write(t, f.root, "big.txt", strings.Repeat("a line of text that adds up to a patch past the cap\n", maxShowPatchBytes/40))
	run(t, f.root, "add", "big.txt")
	run(t, f.root, "commit", "-qm", "big")
	show := f.payload("devcouncil_git_show", map[string]string{})
	if notes, _ := show["degraded"].([]any); len(notes) != 1 || !noteSays(notes[0], "truncated") {
		t.Errorf("git_show truncation is not in the payload: %v", show["degraded"])
	}
	if patch, _ := show["patch"].(string); len(patch) != maxShowPatchBytes {
		t.Errorf("patch is %d bytes, want it cut at %d", len(patch), maxShowPatchBytes)
	}
}

// TestGitLogAndStatusSeparateNoCommitsFromAFailedRead: git exits 128 for "no
// commits yet" and for "not a repository" and "bad object" alike, so treating
// 128 as the first answered `{"commits":[]}` for a directory git could not read
// at all, and a history nobody could see read as a history that was empty.
func TestGitLogAndStatusSeparateNoCommitsFromAFailedRead(t *testing.T) {
	t.Run("unborn", func(t *testing.T) {
		f := newFixture(t)
		if err := os.RemoveAll(filepath.Join(f.root, ".git")); err != nil {
			t.Fatal(err)
		}
		run(t, f.root, "init", "-q")
		log := f.payload("devcouncil_git_log", map[string]any{})
		if commits, _ := log["commits"].([]any); len(commits) != 0 || log["note"] == nil {
			t.Errorf("an unborn repository did not report 'no commits yet': %v", log)
		}
		status := f.payload("devcouncil_git_status", map[string]any{})
		if head, present := status["head"]; !present || head != nil {
			t.Errorf("an unborn repository's head = %v, want null", head)
		}
	})
	t.Run("not a repository", func(t *testing.T) {
		f := newFixture(t)
		if err := os.RemoveAll(filepath.Join(f.root, ".git")); err != nil {
			t.Fatal(err)
		}
		for _, tool := range []string{"devcouncil_git_log", "devcouncil_git_status"} {
			if res := f.call(tool, map[string]any{}); !res.IsError {
				t.Errorf("%s answered for a directory that is not a repository: %s", tool, res.Text)
			}
		}
	})
	t.Run("unreadable object store", func(t *testing.T) {
		f := newFixture(t)
		objects := filepath.Join(f.root, ".git", "objects")
		if err := os.RemoveAll(objects); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(objects, 0o750); err != nil {
			t.Fatal(err)
		}
		for _, tool := range []string{"devcouncil_git_log", "devcouncil_git_status"} {
			res := f.call(tool, map[string]any{})
			if !res.IsError {
				t.Errorf("%s answered over an object store that cannot be read: %s", tool, res.Text)
			}
		}
	})
}

// treeEntries lists the files a revision holds.
func treeEntries(t *testing.T, root, rev string) []string {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "git", "-C", root, "ls-tree", "-r", "--name-only", rev).Output()
	if err != nil {
		t.Fatalf("ls-tree %s: %v", rev, err)
	}
	return strings.Fields(string(out))
}

// TestGitCommitRunsNoRepositoryHook: a repository hook runs a program of the
// repository's choosing, outside the command gate, in the environment git was
// given, and one that stages a file puts content in the commit that the name,
// alias and content checks never saw. `.husky` and `.githooks` are ordinary
// writable directories, so the agent can author the hook. --no-verify would not
// be enough — it leaves prepare-commit-msg and post-commit running — so hooks
// are off at gitSpec, the construction site this package's git children share.
func TestGitCommitRunsNoRepositoryHook(t *testing.T) {
	type hook struct{ name, dir string }
	for _, h := range []hook{
		{"pre-commit via core.hooksPath", ".husky"},
		{"prepare-commit-msg via core.hooksPath", ".githooks"},
		{"post-commit in .git/hooks", ""},
	} {
		t.Run(h.name, func(t *testing.T) {
			f := newFixtureWith(t, map[string]string{
				flags.HarnessPosture:      flags.PostureYolo,
				flags.GrantsAgentCommands: "true",
			})
			dir := filepath.Join(f.root, ".git", "hooks")
			if h.dir != "" {
				dir = filepath.Join(f.root, h.dir)
				run(t, f.root, "config", "core.hooksPath", h.dir)
			}
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			script := "#!/bin/sh\ntouch hook-ran.marker\necho 'planted by a hook' > leak.txt\ngit add leak.txt\n"
			for _, name := range []string{"pre-commit", "prepare-commit-msg", "post-commit"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil { //nolint:gosec // a hook has to be executable
					t.Fatal(err)
				}
			}
			modifySeed(t, f)
			run(t, f.root, "add", "seed.txt")
			checkout(t, f)

			res := f.call("devcouncil_git_commit", map[string]string{"message": "update seed"})
			if res.IsError {
				t.Fatalf("commit failed: %s", res.Text)
			}
			if _, err := os.Stat(filepath.Join(f.root, "hook-ran.marker")); err == nil {
				t.Error("a repository hook ran during git_commit")
			}
			for _, name := range treeEntries(t, f.root, "HEAD") {
				if name == "leak.txt" {
					t.Error("a file a hook staged landed in the commit")
				}
			}
			var payload struct {
				Paths []string `json:"committed_paths"`
			}
			if err := firstJSON(res.Text, &payload); err != nil {
				t.Fatal(err)
			}
			if len(payload.Paths) != 1 || payload.Paths[0] != "seed.txt" {
				t.Errorf("committed_paths = %v, want exactly what landed: seed.txt", payload.Paths)
			}
		})
	}
}

// firstJSON decodes the first JSON value of a tool result, which a demoted
// gate decision follows with a prose note.
func firstJSON(text string, into any) error {
	return json.NewDecoder(strings.NewReader(text)).Decode(into)
}

// An empty root commit is the degenerate path of the post-condition: no parent
// to return to, an empty checked tree and nothing for diff-tree to list.
func TestGitCommitAllowEmptyInAnUnbornRepository(t *testing.T) {
	f := newFixtureWith(t, map[string]string{
		flags.HarnessPosture:      flags.PostureYolo,
		flags.GrantsAgentCommands: "true",
	})
	if err := os.RemoveAll(filepath.Join(f.root, ".git")); err != nil {
		t.Fatal(err)
	}
	run(t, f.root, "init", "-q")
	run(t, f.root, "config", "user.email", "h@test")
	run(t, f.root, "config", "user.name", "h")
	checkout(t, f)

	res := f.call("devcouncil_git_commit", map[string]any{"message": "empty root", "allow_empty": true})
	if res.IsError {
		t.Fatalf("an empty root commit was refused: %s", res.Text)
	}
	var out map[string]any
	if err := firstJSON(res.Text, &out); err != nil {
		t.Fatal(err)
	}
	if head, _ := out["head"].(map[string]any); head == nil || head["sha"] != gitHead(t, f.root) {
		t.Errorf("head = %v, want %s", out["head"], gitHead(t, f.root))
	}
	if out["committed_paths"] != nil {
		t.Errorf("an empty commit listed files: %v", out["committed_paths"])
	}
}

// TestGitCommitReportsWhatLanded: committed_paths and head describe the commit
// that exists, so a root commit (where `diff-tree` needs --root) reports its
// files and not the pre-check list.
func TestGitCommitReportsWhatLanded(t *testing.T) {
	f := newFixtureWith(t, map[string]string{
		flags.HarnessPosture:      flags.PostureYolo,
		flags.GrantsAgentCommands: "true",
	})
	if err := os.RemoveAll(filepath.Join(f.root, ".git")); err != nil {
		t.Fatal(err)
	}
	run(t, f.root, "init", "-q")
	run(t, f.root, "config", "user.email", "h@test")
	run(t, f.root, "config", "user.name", "h")
	run(t, f.root, "add", "seed.txt")
	checkout(t, f)

	// Under yolo the gate's demotion note follows the JSON; the payload is the
	// first value.
	var out map[string]any
	if err := firstJSON(f.call("devcouncil_git_commit", map[string]string{"message": "root"}).Text, &out); err != nil {
		t.Fatal(err)
	}
	paths, _ := out["committed_paths"].([]any)
	if len(paths) != 1 || paths[0] != "seed.txt" {
		t.Fatalf("a root commit reported committed_paths %v", out["committed_paths"])
	}
	head, _ := out["head"].(map[string]any)
	if head == nil || head["sha"] != gitHead(t, f.root) {
		t.Errorf("head = %v, want %s", out["head"], gitHead(t, f.root))
	}
}

// TestGitShowSeparatesNotACommitFromAnUnreadableStore: only rev-parse's exit 1
// says the spelling does not name a commit. A store git cannot read (128) is
// "could not be determined", as git_log and git_status say it, and not a
// refusal that blames the model's argument.
func TestGitShowSeparatesNotACommitFromAnUnreadableStore(t *testing.T) {
	f := newFixture(t)
	if res := f.call("devcouncil_git_show", map[string]string{"object": "HEAD:seed.txt"}); !res.IsError ||
		!strings.Contains(res.Text, "does not name a commit") {
		t.Fatalf("a blob was not refused as not-a-commit: %s", res.Text)
	}

	head := gitHead(t, f.root)
	obj := filepath.Join(f.root, ".git", "objects", head[:2], head[2:])
	if err := os.Chmod(obj, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(obj, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := f.call("devcouncil_git_show", map[string]string{})
	if !res.IsError || strings.Contains(res.Text, "does not name a commit") || !strings.Contains(res.Text, "could not be determined") {
		t.Fatalf("a corrupt object store was reported as %q", res.Text)
	}
}

// TestConfirmLandedLeavesAForeignCommitAlone: HEAD is read after git commit
// returns, so a commit another process lands in that window is what is read
// back. It is not this call's commit and its tree is not the checked one, and
// undoing it would drop the other process's work with ours. Nothing moves.
func TestConfirmLandedLeavesAForeignCommitAlone(t *testing.T) {
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureYolo})
	ctx := context.Background()
	prev := gitHead(t, f.root)

	write(t, f.root, "ours.txt", "ours\n")
	gitIn(t, f.root, "add", "ours.txt")
	gitIn(t, f.root, "commit", "-qm", "ours")
	checked := gitIn(t, f.root, "rev-parse", "HEAD^{tree}")
	ours := gitHead(t, f.root)
	write(t, f.root, "theirs.txt", "theirs\n")
	gitIn(t, f.root, "add", "theirs.txt")
	gitIn(t, f.root, "commit", "-qm", "theirs")
	foreign := gitHead(t, f.root)

	err := f.reg.confirmLanded(ctx, checked, prev, foreign)
	if err == nil || !strings.Contains(err.Error(), "not the commit this call made") || !strings.Contains(err.Error(), "nothing was moved") {
		t.Fatalf("a foreign commit was judged as ours: %v", err)
	}
	if got := gitHead(t, f.root); got != foreign {
		t.Errorf("HEAD = %s, want it untouched at %s (ours %s, prev %s)", got, foreign, ours, prev)
	}

	// A foreign commit under ours is the same: HEAD's parent is not prev.
	gitIn(t, f.root, "reset", "-q", "--hard", ours)
	if err := f.reg.confirmLanded(ctx, checked, prev+"0", ours); err == nil || gitHead(t, f.root) != ours {
		t.Fatalf("a commit on the wrong parent was kept or moved: %v, HEAD %s", err, gitHead(t, f.root))
	}
}

// TestUndoLandedCommitRestoresTheBranch is the post-condition's failure path,
// driven directly because a real mismatch needs the index to move between the
// check and the commit. A commit whose tree is not the one that was checked is
// taken back, to the parent or, for a root commit, to no commit, and the caller
// gets an error, not a degraded pass.
func TestUndoLandedCommitRestoresTheBranch(t *testing.T) {
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureYolo})
	ctx := context.Background()
	before := gitHead(t, f.root)
	checked := gitIn(t, f.root, "rev-parse", "HEAD^{tree}")

	write(t, f.root, "unchecked.txt", "content the scan never saw\n")
	gitIn(t, f.root, "add", "unchecked.txt")
	gitIn(t, f.root, "commit", "-qm", "landed")
	landed := gitHead(t, f.root)

	if err := f.reg.confirmLanded(ctx, checked, before, landed); err == nil || !strings.Contains(err.Error(), "taken back") {
		t.Fatalf("a commit whose tree was not the checked tree was kept: %v", err)
	}
	if got := gitHead(t, f.root); got != before {
		t.Errorf("HEAD = %s after the undo, want the parent %s", got, before)
	}
	if st := gitIn(t, f.root, "status", "--porcelain"); !strings.Contains(st, "unchecked.txt") {
		t.Errorf("the unchecked file should stay staged for a re-scan, status:\n%s", st)
	}

	// A matching tree is kept, and nothing moves.
	gitIn(t, f.root, "commit", "-qm", "checked")
	kept := gitHead(t, f.root)
	if err := f.reg.confirmLanded(ctx, gitIn(t, f.root, "rev-parse", "HEAD^{tree}"), before, kept); err != nil {
		t.Fatalf("a commit of the checked tree was refused: %v", err)
	}
	if got := gitHead(t, f.root); got != kept {
		t.Errorf("HEAD moved from %s to %s on a matching tree", kept, got)
	}

	// A root commit has no parent to return to: the branch goes back to unborn.
	root := t.TempDir()
	gitIn(t, root, "init", "-q")
	write(t, root, "a.txt", "a\n")
	gitIn(t, root, "add", "a.txt")
	gitIn(t, root, "commit", "-qm", "root")
	rr := benchRegistry(root)
	err := rr.confirmLanded(ctx, "4b825dc642cb6eb9a060e54bf8d69288fbee4904", "", gitIn(t, root, "rev-parse", "HEAD"))
	if err == nil || !strings.Contains(err.Error(), "taken back") {
		t.Fatalf("root commit with a foreign tree was kept: %v", err)
	}
	if born, herr := headExists(ctx, root); herr != nil || born {
		t.Errorf("after undoing a root commit HEAD exists=%v err=%v, want unborn", born, herr)
	}
}

// stagedNames reads the index itself, the one place a stage that got past the
// name check shows up.
func stagedNames(t *testing.T, root string) []string {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "git", "-C", root, "diff", "--cached", "--name-only", "-z").Output()
	if err != nil {
		t.Fatalf("diff --cached: %v", err)
	}
	var names []string
	for _, n := range strings.Split(string(out), "\x00") {
		if n != "" {
			names = append(names, n)
		}
	}
	return names
}

// TestGitStagePathsAreLiteral: secretPaths judges the spelling the agent gave,
// and git read that spelling as a pathspec, so `.e*`, `*` and `:(glob).env`
// matched no secret pattern and staged `.env`. There is no unstage tool, so the
// commit-time refusal then wedged the session. A path is a path: a name no file
// has stages nothing, and a directory is judged by the files it would stage.
func TestGitStagePathsAreLiteral(t *testing.T) {
	f := newFixtureWith(t, map[string]string{
		flags.HarnessPosture:      flags.PostureYolo,
		flags.GrantsAgentCommands: "true",
	})
	write(t, f.root, ".env", "TOKEN=hunter2\n")
	write(t, f.root, "app/.env", "TOKEN=nested\n")
	write(t, f.root, "app/main.go", "package app\n")
	write(t, f.root, "we*rd.txt", "a file whose name is a glob\n")
	checkout(t, f)

	for _, spec := range []string{".e*", "*", ":(glob).env", ":(glob)**/.env", ":/.env", ":!seed.txt", ".", "app", "app/"} {
		t.Run(spec, func(t *testing.T) {
			f.call("devcouncil_git_stage", map[string]any{"paths": []string{spec}})
			for _, name := range stagedNames(t, f.root) {
				if strings.HasSuffix(name, ".env") {
					t.Fatalf("staging %q put %s in the index", spec, name)
				}
			}
		})
	}
	if names := stagedNames(t, f.root); len(names) != 0 {
		t.Fatalf("a refused or unmatched stage left %v in the index", names)
	}

	// A file whose name is a glob is still stageable by that name.
	res := f.call("devcouncil_git_stage", map[string]any{"paths": []string{"we*rd.txt"}})
	if res.IsError {
		t.Fatalf("a literal name with glob characters was refused: %s", res.Text)
	}
	if names := stagedNames(t, f.root); len(names) != 1 || names[0] != "we*rd.txt" {
		t.Fatalf("staged %v, want exactly we*rd.txt", names)
	}
}

// noteSays reports whether a degraded note decoded from a payload is a string
// containing want; a note of any other type is not.
func noteSays(note any, want string) bool {
	s, ok := note.(string)
	return ok && strings.Contains(s, want)
}
