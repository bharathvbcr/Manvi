package devcouncil

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"manvi/internal/testsupport"
)

// FuzzVerifierAgreesWithGitAboutWhatADiffTouches is a differential oracle for
// the verifier's diff parser, against the program that wrote the diff.
//
// Everything the rigor gates decide rests on parse_unified. The secret scanner
// only ever sees files it reported; the scope classifier splits those files
// into in_scope and orphans; the coverage gate counts added lines it numbered.
// A file the parser silently drops is therefore not a file that is judged and
// passes — it is a file nobody looked at, reported under the same ok:true as
// one that was scanned clean. That is the failure this repository is organised
// around, sitting under its own credential check.
//
// It had unit tests and no oracle. The tests are good ones, but every case in
// them is a diff someone wrote down, and the parser's input in production is a
// diff *git* wrote — including the shapes nobody thinks to type: a rename, a
// deletion, a binary file, a file with no trailing newline, several of those in
// one diff. So this generates real changes in a real repository, has git render
// them, and holds the verifier to git's own account of which files that diff
// touches.
//
// The invariants:
//
//   - The verifier never disagrees with git about the *set* of files a diff
//     touches. Not a subset, which would mean an unscanned write; not a
//     superset, which would mean a finding attributed to a file the change
//     never touched.
//   - Its own `files` count matches that set, so the number it reports and the
//     list it reports cannot drift apart.
//   - A diff git produced always parses. An error here is the verifier
//     refusing real input, which the caller turns into a named degradation and
//     an operator reads as a broken pipeline.
func FuzzVerifierAgreesWithGitAboutWhatADiffTouches(f *testing.F) {
	verifier := testsupport.DCVerify(f)
	gitBin := testsupport.Tool(f, "git")

	repo := f.TempDir()
	git := func(t testing.TB, args ...string) string {
		t.Helper()
		// Identity and config are passed per invocation rather than written
		// into the repository, so the test cannot be changed by whatever the
		// machine running it has in ~/.gitconfig.
		full := append([]string{
			"-c", "user.email=fuzz@example.invalid",
			"-c", "user.name=fuzz",
			"-c", "init.defaultBranch=main",
			"-c", "commit.gpgsign=false",
			"-C", repo,
		}, args...)
		cmd := exec.Command(gitBin, full...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, bytes.TrimSpace(out))
		}
		return string(out)
	}

	// The files the generated changes act on. Fixed rather than fuzzed: a
	// fuzzed path is an argument-handling test, which lives elsewhere, and one
	// that escaped the repository would be testing the machine. The set spans
	// the shapes that make diffs differ — a nested path, an extensionless file,
	// and one the planned-scope glob will not match.
	names := []string{"a.go", "pkg/b.go", "deep/nested/c.rs", "notes.txt", "Makefile"}

	// A baseline commit, so every generated change is a diff against something.
	for _, name := range names {
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			f.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("base\nline two\nline three\n"), 0o644); err != nil {
			f.Fatal(err)
		}
	}
	git(f, "init", "-q", ".")
	git(f, "add", "-A")
	git(f, "commit", "-qm", "base")

	for _, seed := range []struct {
		plan    string
		content string
	}{
		{"", ""},
		{"\x00", "hello"},
		{"\x01", "one line no newline"},
		{"\x00\x01\x02", "several\nlines\nhere\n"},
		{"\x08", ""},                      // a delete
		{"\x10", ""},                      // an append
		{"\x00\x08\x11\x02", "mixed ops"}, // create, delete, append, write
		{"\x00\x01\x02\x03\x04", "every file at once"}, // touch all five
		{"\x00", "sk-ant-api03-" + strings.Repeat("A", 28)},
		{"\x00", "\x00\x01\x02binary\xff"},
		{"\x00", strings.Repeat("long line ", 500)},
		{"\x00\x00\x00", "repeated file"},
		{"\x04", "Makefile\ttab\tseparated"},
	} {
		f.Add(seed.plan, seed.content)
	}

	f.Fuzz(func(t *testing.T, plan, content string) {
		// Back to the baseline commit, so each case is judged on its own
		// changes rather than on everything the corpus did before it.
		git(t, "reset", "-q", "--hard")
		git(t, "clean", "-fdq")

		applied := false
		for i := 0; i < len(plan); i++ {
			b := plan[i]
			name := names[int(b)%len(names)]
			path := filepath.Join(repo, name)
			switch (b >> 3) % 3 {
			case 0: // rewrite
				body := fmt.Sprintf("%s\n%s\n", content, name)
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			case 1: // append, which produces a diff with no removed lines
				fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
				if err != nil {
					t.Fatal(err)
				}
				fmt.Fprintf(fh, "appended %d %s\n", i, content)
				fh.Close()
			case 2: // delete
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			applied = true
		}
		if !applied {
			return // no plan, no diff, nothing to reconcile
		}

		git(t, "add", "-A")
		diff := git(t, "diff", "--cached")
		if strings.TrimSpace(diff) == "" {
			return // the changes cancelled out; git sees nothing and neither should the verifier
		}

		// git's own account of which files this diff touches. -z because a
		// generated name could in principle need quoting, and a quoted name
		// compared against an unquoted one is a difference in the test rather
		// than in the thing under test.
		wantList := strings.Split(strings.TrimRight(git(t, "diff", "--cached", "--name-only", "-z"), "\x00"), "\x00")
		want := map[string]bool{}
		for _, n := range wantList {
			if n != "" {
				want[n] = true
			}
		}

		out, err := runVerifier(t, verifier, diff)
		if err != nil {
			t.Fatalf("a diff git produced did not parse: %v\n--- diff ---\n%s", err, diff)
		}

		got := map[string]bool{}
		for _, p := range append(append([]string{}, out.InScope...), out.Orphans...) {
			got[p] = true
		}
		if !sameSet(want, got) {
			t.Fatalf("the verifier and git disagree about which files this diff touches\n"+
				"  git:      %v\n  verifier: %v\n"+
				"a file only git names is a write nobody scanned; one only the verifier names is a\n"+
				"finding attributed to a file the change never touched\n--- diff ---\n%s",
				keys(want), keys(got), diff)
		}
		if out.Files != len(got) {
			t.Fatalf("the verifier reported files=%d over a list of %d (%v); the number and the list must not drift",
				out.Files, len(got), keys(got))
		}
	})
}

// runVerifier drives the real binary the way rigorClient does, and returns the
// decoded result. It goes through rigorClient rather than around it so the
// target exercises the client the harness actually uses.
func runVerifier(t *testing.T, binary, diff string) (*rigorResult, error) {
	t.Helper()
	c := rigorClient{Binary: binary, Timeout: 60 * time.Second}
	// Everything in scope: this target is about which files the parser found,
	// not about how they are classified, and `**` puts every one of them on the
	// in_scope side without the glob becoming a second thing under test.
	return c.run(t.Context(), diff, []string{"**"}, "")
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
