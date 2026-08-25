package gate

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"

	"manvi/dc"
	"manvi/flags"
	"manvi/policy"
)

// This file holds the one check that compares what the command gate says with
// what the shell actually does.
//
// Every other test in this package asserts a verdict against an expectation
// someone wrote down, which is exactly how the defect it was written for
// survived: the gate refused `echo x > .env` and allowed
// `echo $(echo x > .env)`, and both answers matched a hand-written expectation
// because nobody had written down that the two are the same write. The arbiter
// here is the filesystem — the command is executed in a throwaway tree and the
// files it touched are compared against the gate's own verdict on those files.

// redirectTask is the scope these cases are judged against. src/calc.go is
// planned and writable; nothing else is.
func redirectTask() *dc.Task {
	return &dc.Task{
		ID:              "TASK-001",
		PlannedFiles:    []dc.PlannedFile{{Path: "src/calc.go", AllowedChange: dc.ChangeModify}},
		AllowedCommands: []string{"echo *", "printf *", "cat *"},
	}
}

// hiddenWriteCorpus is the shapes a redirection can take. Each is harmless —
// they write only relative paths, inside the temporary tree the runner gives
// them — and each one either hides a redirection from a linear scan of the
// line, or hides the command carrying it from the allowlist.
var hiddenWriteCorpus = []string{
	// Plain forms, as the control group.
	`echo hi > .env`,
	`echo hi > src/calc.go`,
	`echo hi >> .env`,
	`echo hi 2> .env`,
	`echo hi &> .env`,
	`echo hi >&2 > .env`,
	`echo hi > ./.env`,
	`echo hi > src/../.env`,
	`echo hi > .ENV`,
	`echo hi > src/.env`,
	`echo hi > ".env"`,
	`echo hi > '.env'`,
	`echo hi > .en\v`,
	// noclobber override: a redirection operator the tail-stripper did not know.
	`echo hi >| .env`,
	// Redirections inside a substitution the shell executes.
	`echo $(echo hi > .env)`,
	`echo $(echo $(echo hi > .env))`,
	`echo "$(echo hi > .env)"`,
	`echo ${x:-$(echo hi > .env)}`,
	"echo `echo hi > .env`",
	"echo `echo hi > src/calc.go`",
	// Commands the allowlist does not name, carrying a redirection. Each is a
	// Soft refusal, and a Soft refusal is demotable — which is what made these
	// writes reachable under the shipped posture.
	`(echo hi > .env)`,
	`{ echo hi > .env; }`,
	`(echo hi) > .env`,
	`if true; then echo hi > .env; fi`,
	`for i in 1; do echo hi > .env; done`,
	`while false; do :; done > .env`,
	`case x in x) echo hi > .env;; esac`,
	`exec > .env; echo hi`,
	`false || echo hi > .env`,
	`true && echo hi > .env`,
	`true; echo hi > .env`,
	`echo hi | cat > .env`,
	`echo hi > .env &`,
	// Re-parsed code: the redirection is not a redirection until sh expands it.
	`eval "echo hi > .env"`,
	`eval 'echo hi > .env'`,
	`FOO="a b" eval "echo hi > .env"`,
	`2>/dev/null eval "echo hi > .env"`,
	`command eval "echo hi > .env"`,
	`\eval "echo hi > .env"`,
	// Escaping the repository entirely. Found by the generated differential in
	// redirect_generative_test.go, not by anyone writing it down: hiding the
	// redirection in a substitution defeated the outside-root rung exactly as
	// it defeated the secret rung, and that is an arbitrary write anywhere the
	// process can reach rather than a write to one known-sensitive name.
	`echo $(printf x >> ../escaped.txt)`,
	"echo `printf x > ../escaped.txt`",
	`echo "$(printf x >> ../../escaped.txt)"`,
	// Targets only the shell can resolve.
	`echo hi > $(echo .env)`,
	`echo hi > "$(printf .env)"`,
}

// TestCommandVerdictIsNeverLooserThanTheWritesItPerforms is the invariant.
//
// A command line has two verdicts — one about the command, one about each file
// it redirects into — and the gate must never answer the first more
// permissively than it would answer the second. Stated that way the property
// holds in every posture without the test having to know which writes a
// posture permits: it asks the gate itself about each file that appeared, in
// the same posture, and only fails when the command was allowed and one of its
// writes would not have been.
func TestCommandVerdictIsNeverLooserThanTheWritesItPerforms(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Fatalf("sh is not on PATH; this check would examine nothing: %v", err)
	}
	for _, posture := range []string{flags.PostureStrict, flags.PostureDev} {
		for _, command := range hiddenWriteCorpus {
			t.Run(posture+" "+command, func(t *testing.T) {
				g := newGate(t, map[string]string{flags.HarnessPosture: posture})
				decision, err := g.EvaluateCommand(command, redirectTask())
				if err != nil {
					t.Fatalf("EvaluateCommand(%q): %v", command, err)
				}

				written := runInThrowawayTree(t, command)
				if decision.Blocked() {
					// The command does not run, so what it would have written
					// is not the question. Recorded rather than skipped so a
					// corpus entry that stops writing anything at all — and
					// therefore stops testing anything — is visible.
					t.Logf("refused by %s; the shell would have written %v", decision.Rule, written)
					return
				}
				for _, path := range written {
					// A fresh gate per path: EvaluateWrite records into the
					// run report, and reusing the command's gate would let one
					// question contaminate another's summary.
					wg := newGate(t, map[string]string{flags.HarnessPosture: posture})
					w, err := wg.EvaluateWrite(path, redirectTask(), dc.OpWrite)
					if err != nil {
						t.Fatalf("EvaluateWrite(%q): %v", path, err)
					}
					if w.Blocked() {
						t.Errorf("posture %s: %q was allowed, but it wrote %q, which the write gate refuses (%s: %s)",
							posture, command, path, w.Rule, w.Reason)
					}
				}
			})
		}
	}
}

// TestHiddenWritesAreRefusedOutrightUnderEveryPosture is the sharper half.
//
// The invariant above is satisfied by a gate that allows the command and would
// also allow the write, which is the right answer for a file a posture has
// deliberately stopped enforcing. It is not the right answer for .env: the
// secret rung is Hard, no posture demotes it, and a command that writes .env
// must be refused whichever way it spells the redirection.
func TestHiddenWritesAreRefusedOutrightUnderEveryPosture(t *testing.T) {
	for _, posture := range []string{flags.PostureStrict, flags.PostureDev} {
		for _, command := range hiddenWriteCorpus {
			written := runInThrowawayTree(t, command)
			if !touches(written, ".env") {
				continue
			}
			g := newGate(t, map[string]string{flags.HarnessPosture: posture})
			d, err := g.EvaluateCommand(command, redirectTask())
			if err != nil {
				t.Fatalf("EvaluateCommand(%q): %v", command, err)
			}
			if !d.Blocked() {
				t.Errorf("posture %s: %q writes .env and was allowed (rule %q)", posture, command, d.Rule)
				continue
			}
			if d.Severity != policy.Hard {
				t.Errorf("posture %s: %q writes .env and was refused only softly (%s/%s); "+
					"a Soft refusal is demotable, so the credential rung is reachable through it",
					posture, command, d.Rule, d.Severity)
			}
		}
	}
}

// The same demand as TestHiddenWritesAreRefusedOutrightUnderEveryPosture, for
// the rung one step wider than the secret one. A path outside the project is
// refused by a Hard rule, so no posture may reach it however the redirection is
// spelled — and unlike .env this is not a write to one known name, it is a
// write to anywhere the process can reach.
func TestEscapingWritesAreRefusedOutrightUnderEveryPosture(t *testing.T) {
	escaping := []string{
		`echo $(printf x >> ../escaped.txt)`,
		"echo `printf x > ../escaped.txt`",
		`echo "$(printf x >> ../../escaped.txt)"`,
		`printf x > ../escaped.txt`,
		`(printf x > ../escaped.txt)`,
		`false || printf x > ../escaped.txt`,
		`printf x >| ../escaped.txt`,
	}
	for _, posture := range []string{flags.PostureStrict, flags.PostureDev} {
		for _, command := range escaping {
			g := newGate(t, map[string]string{flags.HarnessPosture: posture})
			d, err := g.EvaluateCommand(command, redirectTask())
			if err != nil {
				t.Fatalf("EvaluateCommand(%q): %v", command, err)
			}
			if !d.Blocked() {
				t.Errorf("posture %s: %q writes outside the project root and was allowed (rule %q)",
					posture, command, d.Rule)
				continue
			}
			if d.Severity != policy.Hard {
				t.Errorf("posture %s: %q escapes the root and was refused only softly (%s/%s); "+
					"a Soft refusal is demotable", posture, command, d.Rule, d.Severity)
			}
		}
	}
}

func touches(written []string, name string) bool {
	for _, w := range written {
		if filepath.Base(w) == name {
			return true
		}
	}
	return false
}

// runInThrowawayTree executes command under `sh -c` in a temporary directory
// and returns every path, relative to it, that appeared or changed.
func runInThrowawayTree(t *testing.T, command string) []string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := treeSnapshot(root)
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = root
	// A corpus entry that hangs would wedge the suite; nothing here reads
	// stdin, and closing it turns any that starts to into an immediate EOF.
	cmd.Stdin = nil
	_ = cmd.Run()
	after := treeSnapshot(root)

	var changed []string
	for path, content := range after {
		if prev, existed := before[path]; !existed || prev != content {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	return changed
}

func treeSnapshot(root string) map[string]string {
	out := map[string]string{}
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		body, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		out[rel] = string(body)
		return nil
	})
	return out
}
