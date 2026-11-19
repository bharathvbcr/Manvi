//go:build unix

package bootstrap

// Adversarial tests for the w23 containment change (lane w23-containment-adv).
// A test whose name ends in Defect documents a real defect and is expected to
// FAIL on the w23-containment tip.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func zadvWithin(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s blocked for 30s", what)
	}
}

// .gitignore as a link (to a file, dangling), a FIFO, a directory, a sparse
// 1 TiB file, a mode-000 file: Ensure neither hangs nor reads unbounded, leaves
// the entry as found, never touches what a link names, and reports a failure.
func TestZadvGitignoreHostileEntries(t *testing.T) {
	for _, how := range []string{"link", "dangling", "fifo", "dir", "huge", "mode000"} {
		t.Run(how, func(t *testing.T) {
			if how == "mode000" && os.Geteuid() == 0 {
				t.Skip("root reads mode-000 files")
			}
			root := gitRepo(t)
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, ".gitignore")
			switch how {
			case "link":
				if err := os.Symlink(outside, path); err != nil {
					t.Skip(err)
				}
			case "dangling":
				if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), path); err != nil {
					t.Skip(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
			case "dir":
				if err := os.Mkdir(path, 0o750); err != nil {
					t.Fatal(err)
				}
			case "huge":
				f, err := os.Create(path) // #nosec G304 -- a path under this test's own t.TempDir
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Truncate(1 << 40); err != nil {
					t.Skip(err)
				}
				_ = f.Close()
			case "mode000":
				if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(path, 0o600) }()
			}
			before, _ := os.Lstat(path)
			var report Report
			zadvWithin(t, "Ensure", func() { report = Ensure(root, options()) })
			if len(report.Failures) == 0 || len(report.AddedRules) != 0 {
				t.Fatalf("report = %+v, want a failure and no added rules", report)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatalf(".gitignore entry was replaced: %v", err)
			}
			if got, _ := os.ReadFile(outside); how == "link" && string(got) != "outside\n" { // #nosec G304 -- a path under this test's own t.TempDir
				t.Fatalf("the link target was written: %q", got)
			}
			if es, _ := os.ReadDir(root); true {
				for _, e := range es {
					if strings.HasSuffix(e.Name(), ".tmp") {
						t.Errorf("stage file left behind: %s", e.Name())
					}
				}
			}
		})
	}
}

// Byte fidelity: non-UTF-8 bytes, CRLF line endings, no trailing newline, a
// leading BOM. The operator's bytes are a strict prefix of the result, the rules
// start on a fresh line, a second Ensure adds nothing, and the mode survives.
func TestZadvGitignoreByteFidelity(t *testing.T) {
	cases := map[string]string{
		"non-utf8":   "# caf\xe9\n*.\xff\xfe\n",
		"crlf":       "node_modules/\r\ndist/\r\n",
		"no-newline": "build",
		"bom":        "\xef\xbb\xbf*.log\n",
		"empty":      "",
		"only-nl":    "\n\n\n",
		"nul":        "a\x00b\n",
	}
	for name, original := range cases {
		t.Run(name, func(t *testing.T) {
			root := gitRepo(t)
			path := filepath.Join(root, ".gitignore")
			if err := os.WriteFile(path, []byte(original), 0o640); err != nil { // #nosec G306 -- a .gitignore the test builds under t.TempDir; 0640 is the mode whose preservation is being tested
				t.Fatal(err)
			}
			report := Ensure(root, options())
			if len(report.Failures) != 0 {
				t.Fatalf("failures: %v", report.Failures)
			}
			got, err := os.ReadFile(path) // #nosec G304 -- a path under this test's own t.TempDir
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(got), original) {
				t.Fatalf("the operator's bytes were altered:\n%q\nfrom %q", got, original)
			}
			tail := strings.TrimPrefix(string(got), original)
			if original != "" && !strings.HasSuffix(original, "\n") && !strings.HasPrefix(tail, "\n") {
				t.Fatalf("the first rule was glued to the operator's last line: %q", tail)
			}
			info, _ := os.Stat(path)
			if info.Mode().Perm() != 0o640 {
				t.Errorf("mode = %v, want 0640", info.Mode().Perm())
			}
			again := Ensure(root, options())
			if len(again.AddedRules) != 0 || len(again.Failures) != 0 {
				t.Fatalf("second Ensure = %+v, want a no-op", again)
			}
		})
	}
}

// The .gitignore is edited by the operator between Ensure's read and its
// publish. A tiny git shim on PATH makes that window deterministic: it appends a
// line the first time Ensure calls git, which happens between the read and the
// write. The edit must survive.
func TestZadvGitignoreEditedBetweenReadAndPublishIsKeptDefect(t *testing.T) {
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	root := gitRepo(t)
	path := filepath.Join(root, ".gitignore")
	if err := os.WriteFile(path, []byte("# mine\n"), 0o644); err != nil { // #nosec G306 -- an operator .gitignore under t.TempDir, written in the usual world-readable mode
		t.Fatal(err)
	}
	shim := t.TempDir()
	marker := filepath.Join(shim, "done")
	script := fmt.Sprintf("#!/bin/sh\nif [ ! -e %q ]; then : > %q; printf 'OPERATOR-EDIT\\n' >> %q; fi\nexec %q \"$@\"\n", marker, marker, path, real)
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil { // #nosec G306 -- a stand-in git executable under t.TempDir must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	report := Ensure(root, options())
	if len(report.Failures) != 0 {
		t.Fatalf("failures: %v", report.Failures)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Skip("the shim was never called, so the window was not exercised")
	}
	got, _ := os.ReadFile(path) // #nosec G304 -- a path under this test's own t.TempDir
	if !strings.Contains(string(got), "OPERATOR-EDIT") {
		t.Fatalf("an edit made between the read and the publish was overwritten:\n%s", got)
	}
}

const zadvEnv = "ZADV_W23_BOOTSTRAP_HELPER"

func TestZadvHelperProcess(t *testing.T) {
	root := os.Getenv(zadvEnv)
	if root == "" {
		t.Skip("helper process only")
	}
	for i := 0; i < 20; i++ {
		if r := Ensure(root, options()); len(r.Failures) != 0 {
			fmt.Fprintln(os.Stderr, r.Failures)
			os.Exit(3)
		}
	}
	os.Exit(0)
}

// Six real processes run Ensure on one repository at once: the file ends up
// valid (every managed rule exactly once, the operator's line first), with no
// stage left behind.
func TestZadvConcurrentEnsureAcrossProcesses(t *testing.T) {
	root := gitRepo(t)
	path := filepath.Join(root, ".gitignore")
	if err := os.WriteFile(path, []byte("# mine\nlocal.txt\n"), 0o644); err != nil { // #nosec G306 -- an operator .gitignore under t.TempDir, written in the usual world-readable mode
		t.Fatal(err)
	}
	var cmds []*exec.Cmd
	var outs []*strings.Builder
	for i := 0; i < 6; i++ {
		c := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestZadvHelperProcess$") // #nosec G204 G702 -- re-execs this test binary as its own helper; the environment selects the role
		c.Env = append(os.Environ(), zadvEnv+"="+root)
		var sb strings.Builder
		c.Stdout, c.Stderr = &sb, &sb
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		cmds, outs = append(cmds, c), append(outs, &sb)
	}
	for i, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Errorf("child %d: %v\n%s", i, err, outs[i])
		}
	}
	got, err := os.ReadFile(path) // #nosec G304 -- a path under this test's own t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "# mine\nlocal.txt\n") {
		t.Fatalf("the operator's lines were lost:\n%s", got)
	}
	seen := map[string]int{}
	for _, line := range strings.Split(string(got), "\n") {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "#") {
			seen[l]++
		}
	}
	for rule, n := range seen {
		if n != 1 {
			t.Errorf("rule %q appears %d times", rule, n)
		}
	}
	es, _ := os.ReadDir(root)
	for _, e := range es {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("stage file left behind: %s", e.Name())
		}
	}
}
