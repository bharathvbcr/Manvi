package devcouncil

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"manvi/flags"
	"manvi/policy"
)

// The git tools shell out to git, so — like gitdiff_test.go — they are tested
// against real repositories. The fixture in tools_test.go already builds one;
// these tests reuse it so the gate, the store and the repo are the same
// objects production wires together.

func gitHead(t *testing.T, root string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func modifySeed(t *testing.T, f *fixture) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, "seed.txt"), []byte("seed\nchanged\n"), 0o644); err != nil {
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
	cmd := exec.Command("git", "-C", root, "status", "--porcelain=v1", "-z", "-b")
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
	out, err := exec.Command("git", "-C", root, "rev-list", "--count", "HEAD").Output()
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
