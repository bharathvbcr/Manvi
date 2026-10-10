package devcouncil

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// An untracked file that was listed but could not be rendered is not in the
// diff, so the secret scanner, the stub detector and the scope check never saw
// it. It has to be named, with the reason, rather than dropped.
func TestAnUnreadableUntrackedFileIsNamedInTheDiffNotes(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits do not stop a read here")
	}
	root := newGitRepo(t)
	write(t, root, "ok.txt", "fine\n")
	write(t, root, "leak.txt", "secret\n")
	if err := os.Chmod(filepath.Join(root, "leak.txt"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "leak.txt"), 0o600) })

	diff, notes, err := gitDiff(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "ok.txt") {
		t.Fatalf("the readable file is missing from the diff:\n%s", diff)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "leak.txt") || !strings.Contains(joined, "could not be rendered") {
		t.Fatalf("the unrendered file is not named in the notes: %q", notes)
	}
	if !strings.Contains(joined, "Permission denied") {
		t.Errorf("the note does not say why: %q", notes)
	}
}

// The whole fan-out sits under one deadline. When it expires part-way, every
// file still waiting is unrendered, and the note must carry how many and which.
func TestAnExpiredDeadlineMidFanOutIsCountedInTheNotes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the git shim is a shell script")
	}
	root := newGitRepo(t)
	const files = 12
	for i := 0; i < files; i++ {
		write(t, root, fmt.Sprintf("new%02d.txt", i), "x\n")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not found: %v", err)
	}
	shim := t.TempDir()
	// Only the per-file render hangs; listing and the tracked diff are real.
	script := "#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = \"--no-index\" ]; then exec sleep 60; fi; done\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, notes, err := gitDiff(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, fmt.Sprintf("rendered 0 of %d untracked file(s); %d could not be rendered", files, files)) {
		t.Fatalf("the notes do not count the %d unrendered files: %q", files, notes)
	}
	if !strings.Contains(joined, "new00.txt") || !strings.Contains(joined, "timed out") {
		t.Fatalf("the notes do not name a file and the reason: %q", notes)
	}
}

// An incomplete diff is a check that could not see everything: the verdict is
// degraded when the gates are otherwise clean, never passed, and never failed
// on that account alone. A real failure stays failed.
func TestAnIncompleteDiffIsDegradedAndARealFailureStaysFailed(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits do not stop a read here")
	}
	for _, tc := range []struct {
		name    string
		orphan  bool
		want    string
		blocked bool
	}{
		{"clean gates", false, VerdictDegraded, false},
		{"a failing gate", true, VerdictFailed, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})
			if err := os.MkdirAll(filepath.Join(f.root, "src"), 0o750); err != nil {
				t.Fatal(err)
			}
			write(t, f.root, "src/calc.go", "package calc\n")
			if tc.orphan {
				write(t, f.root, "unplanned.txt", "x\n")
			}
			leak := filepath.Join(f.root, "leak.txt")
			write(t, f.root, "leak.txt", "secret\n")
			if err := os.Chmod(leak, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(leak, 0o600) })

			report, err := f.reg.report(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !report.diffIncomplete {
				t.Fatalf("the shortfall was not recorded: %+v", report.Degraded)
			}
			if report.Verdict != tc.want || report.Passed {
				t.Fatalf("verdict = %q passed = %v, want %q and not passed; gaps %+v",
					report.Verdict, report.Passed, tc.want, report.Gaps)
			}
			if (report.BlockingCount() > 0) != tc.blocked {
				t.Fatalf("blocking = %d, want blocked=%v: %+v", report.BlockingCount(), tc.blocked, report.Gaps)
			}
			for _, g := range report.Gaps {
				if g.Type == "diff_incomplete" {
					t.Fatalf("an incomplete diff became a gap: %+v", g)
				}
			}
			joined := strings.Join(report.Degraded, "\n")
			if !strings.Contains(joined, "rendered ") || !strings.Contains(joined, " of ") {
				t.Fatalf("the reason does not carry rendered N of M: %q", report.Degraded)
			}
		})
	}
}
