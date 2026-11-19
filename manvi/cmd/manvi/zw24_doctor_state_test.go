package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Doctor is a diagnostic: a state directory that is a link out of the tree is a
// finding it reports, and nothing is created or written through the link.
func TestDoctorReportsALinkedStateDirInsteadOfAborting(t *testing.T) {
	root, outside := stateTree(t)
	defer setProjectRootForTest(root)()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	t.Setenv("MANVI_STATE_DIR", "link/state")

	var out, notes bytes.Buffer
	if err := run(&out, &notes, []string{"doctor"}); err != nil {
		t.Fatalf("doctor aborted instead of reporting: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "state dir       REFUSED") {
		t.Fatalf("doctor does not report the refusal:\n%s", text)
	}
	for _, row := range []string{"build ", "posture ", "config ", "verifier "} {
		if !strings.Contains(text, row) {
			t.Errorf("doctor stopped before diagnosing: no %q row:\n%s", row, text)
		}
	}
	if strings.Contains(text, "-> ") && strings.Contains(text, "store           ") {
		t.Errorf("doctor probed the store through the refused directory:\n%s", text)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("doctor created or wrote %v outside the project", entries)
	}
	if notes.Len() != 0 {
		t.Errorf("scaffolding ran against the refused directory:\n%s", notes.String())
	}
}

// Every other command that keeps state is still refused by name, before it runs.
func TestOtherCommandsAreStillRefusedOnALinkedStateDir(t *testing.T) {
	root, outside := stateTree(t)
	defer setProjectRootForTest(root)()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	t.Setenv("MANVI_STATE_DIR", "link/state")
	for _, args := range [][]string{{"lease", "list"}, {"reflect"}} {
		if err := run(io.Discard, io.Discard, args); err == nil || !strings.Contains(err.Error(), "outside") {
			t.Errorf("run(%v) = %v, want the state-directory refusal", args, err)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("something was created outside the project: %v", entries)
	}
}
