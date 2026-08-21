package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAnExplicitPathIsNeverSecondGuessed. An operator who named a path meant
// it, including when it does not exist — silently substituting something else
// would run a different binary than the one they asked for.
func TestAnExplicitPathIsNeverSecondGuessed(t *testing.T) {
	t.Setenv("MANVI_TEST_BINARY", "/nowhere/in/particular/dcstore")
	if got := toolBinary("MANVI_TEST_BINARY", "dcstore"); got != "/nowhere/in/particular/dcstore" {
		t.Fatalf("toolBinary = %q; an explicit path must win outright", got)
	}
}

// TestAMissingBinaryIsReturnedByBareName so the resulting exec error names what
// was looked for. Returning an empty string would produce "exec: no command",
// which says nothing about which tool is missing.
func TestAMissingBinaryIsReturnedByBareName(t *testing.T) {
	if got := toolBinary("MANVI_TEST_ABSENT", "definitely-not-installed-xyz"); got != "definitely-not-installed-xyz" {
		t.Fatalf("toolBinary = %q, want the bare name", got)
	}
}

// TestASiblingBuildIsFoundWhenPATHHasNothing is the failure this exists for: the
// binary was built, it was sitting in crates/target, and every store-backed tool
// still reported it missing because nobody had copied it onto PATH.
func TestASiblingBuildIsFoundWhenPATHHasNothing(t *testing.T) {
	root := t.TempDir()
	release := filepath.Join(root, "crates", "target", "release")
	if err := os.MkdirAll(release, 0o755); err != nil {
		t.Fatal(err)
	}
	built := filepath.Join(release, "dcstore-testonly")
	if err := os.WriteFile(built, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Stand inside the workspace, which is the case the fallback serves.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}

	got := toolBinary("MANVI_TEST_UNSET", "dcstore-testonly")
	// EvalSymlinks because a temp dir on macOS is under a symlinked /var.
	wantResolved, _ := filepath.EvalSymlinks(built)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != wantResolved {
		t.Fatalf("toolBinary = %q, want the sibling build at %q", got, built)
	}
}

// TestADirectoryNamedLikeTheBinaryIsNotACandidate. Returning one would turn a
// clear "not found" into a "permission denied" at exec time, much later and
// much less legibly.
func TestADirectoryNamedLikeTheBinaryIsNotACandidate(t *testing.T) {
	root := t.TempDir()
	release := filepath.Join(root, "crates", "target", "release")
	if err := os.MkdirAll(filepath.Join(release, "dcstore-dirname"), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}

	if got := toolBinary("MANVI_TEST_UNSET2", "dcstore-dirname"); got != "dcstore-dirname" {
		t.Fatalf("toolBinary = %q; a directory is not an executable", got)
	}
}

// TestANonExecutableFileIsNotACandidate, for the same reason.
func TestANonExecutableFileIsNotACandidate(t *testing.T) {
	root := t.TempDir()
	release := filepath.Join(root, "crates", "target", "release")
	if err := os.MkdirAll(release, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "dcstore-noexec"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}

	if got := toolBinary("MANVI_TEST_UNSET3", "dcstore-noexec"); got != "dcstore-noexec" {
		t.Fatalf("toolBinary = %q; a non-executable file is not a candidate", got)
	}
}
