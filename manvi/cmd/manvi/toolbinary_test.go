package main

import (
	"os"
	"path/filepath"
	"strings"
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
// binary was built, it was sitting in a workspace target directory, and every
// store-backed tool still reported it missing because nobody had copied it onto PATH.
func TestASiblingBuildIsFoundWhenPATHHasNothing(t *testing.T) {
	layouts := []string{
		filepath.Join("crates", "target", "release"),
		filepath.Join("rust", "target", "release"),
		filepath.Join("DevCouncil", "rust", "target", "release"),
	}
	for _, rel := range layouts {
		t.Run(rel, func(t *testing.T) {
			root := t.TempDir()
			release := filepath.Join(root, rel)
			if err := os.MkdirAll(release, 0o750); err != nil {
				t.Fatal(err)
			}
			built := filepath.Join(release, "dcstore-testonly")
			if err := os.WriteFile(built, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}

			buildTreeBinary(t, root)

			got := toolBinary("MANVI_TEST_UNSET", "dcstore-testonly")
			wantResolved, _ := filepath.EvalSymlinks(built)
			gotResolved, _ := filepath.EvalSymlinks(got)
			if gotResolved != wantResolved {
				t.Fatalf("toolBinary = %q, want the sibling build at %q", got, built)
			}
		})
	}
}

// TestADirectoryNamedLikeTheBinaryIsNotACandidate. Returning one would turn a
// clear "not found" into a "permission denied" at exec time, much later and
// much less legibly.
func TestADirectoryNamedLikeTheBinaryIsNotACandidate(t *testing.T) {
	root := t.TempDir()
	release := filepath.Join(root, "crates", "target", "release")
	if err := os.MkdirAll(filepath.Join(release, "dcstore-dirname"), 0o750); err != nil {
		t.Fatal(err)
	}
	buildTreeBinary(t, root)

	if got := toolBinary("MANVI_TEST_UNSET2", "dcstore-dirname"); got != "dcstore-dirname" {
		t.Fatalf("toolBinary = %q; a directory is not an executable", got)
	}
}

// TestANonExecutableFileIsNotACandidate, for the same reason.
func TestANonExecutableFileIsNotACandidate(t *testing.T) {
	root := t.TempDir()
	release := filepath.Join(root, "crates", "target", "release")
	if err := os.MkdirAll(release, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "dcstore-noexec"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	buildTreeBinary(t, root)

	if got := toolBinary("MANVI_TEST_UNSET3", "dcstore-noexec"); got != "dcstore-noexec" {
		t.Fatalf("toolBinary = %q; a non-executable file is not a candidate", got)
	}
}

// buildTreeBinary stands the harness inside root as a developer build: the
// running binary is root/bin/manvi, with no release stamp.
func buildTreeBinary(t *testing.T, root string) {
	t.Helper()
	exeDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(exeDir, 0o750); err != nil {
		t.Fatal(err)
	}
	prevExe, prevStamp := toolExecutable, stampedVersion
	toolExecutable = func() (string, error) { return filepath.Join(exeDir, "manvi"), nil }
	stampedVersion = ""
	t.Cleanup(func() { toolExecutable, stampedVersion = prevExe, prevStamp })
}

// plantInCwd writes an executable named name into root/crates/target/release
// and makes root the working directory, which is what an agent can arrange:
// its shell runs `cargo build`, or just writes the file.
func plantInCwd(t *testing.T, root, name string) string {
	t.Helper()
	release := filepath.Join(root, "crates", "target", "release")
	if err := os.MkdirAll(release, 0o750); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(release, name)
	if err := os.WriteFile(planted, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
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
	return planted
}

// releaseStandIn marks the running binary as release-stamped, installed in
// installDir. The stamp is the one version.go reads.
func releaseStandIn(t *testing.T, installDir string) {
	t.Helper()
	prevExe, prevStamp := toolExecutable, stampedVersion
	toolExecutable = func() (string, error) { return filepath.Join(installDir, "manvi"), nil }
	stampedVersion = "v9.9.9"
	t.Cleanup(func() { toolExecutable, stampedVersion = prevExe, prevStamp })
}

// TestAReleaseBuildNeverExecsAPlantedWorkspaceBinary is the trust boundary:
// the binary resolved here runs on the host outside the gate, and the working
// directory and project root are where the agent can put files.
func TestAReleaseBuildNeverExecsAPlantedWorkspaceBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	root := t.TempDir()
	planted := plantInCwd(t, root, "dcverify-planted")
	t.Setenv("MANVI_PROJECT_ROOT", root)
	releaseStandIn(t, filepath.Join(t.TempDir(), "install"))

	got := toolBinary("MANVI_TEST_UNSET_REL", "dcverify-planted")
	if got == planted || strings.HasPrefix(got, root) {
		t.Fatalf("release build resolved %q, the planted workspace binary", got)
	}
	if got != "dcverify-planted" {
		t.Fatalf("toolBinary = %q, want the bare name when nothing trusted has it", got)
	}
}

// TestADeveloperBuildNeverWalksFromTheWorkingDirectory: a build tree is found
// from the binary's own location, never from wherever the agent points cwd.
func TestADeveloperBuildNeverWalksFromTheWorkingDirectory(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	elsewhere := t.TempDir()
	buildTreeBinary(t, elsewhere)
	root := t.TempDir()
	plantInCwd(t, root, "dcverify-devplant")
	t.Setenv("MANVI_PROJECT_ROOT", root)

	if got := toolBinary("MANVI_TEST_UNSET_DEV", "dcverify-devplant"); got != "dcverify-devplant" {
		t.Fatalf("developer build resolved %q from the working directory", got)
	}
}

// TestAReleaseBuildFindsACompanionNextToItself: the directory holding the
// binary is a trusted source, because an installer puts the companions there.
func TestAReleaseBuildFindsACompanionNextToItself(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	install := t.TempDir()
	companion := filepath.Join(install, "dcstore-sibling")
	if err := os.WriteFile(companion, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	releaseStandIn(t, install)
	if got := toolBinary("MANVI_TEST_UNSET_SIB", "dcstore-sibling"); got != companion {
		t.Fatalf("toolBinary = %q, want the companion at %q", got, companion)
	}
}

func TestAReleaseBuildStillHonoursExplicitConfigurationAndPATH(t *testing.T) {
	releaseStandIn(t, t.TempDir())
	t.Setenv("MANVI_TEST_REL_EXPLICIT", "/operator/chose/dcverify")
	if got := toolBinary("MANVI_TEST_REL_EXPLICIT", "dcverify"); got != "/operator/chose/dcverify" {
		t.Fatalf("explicit path not honoured: %q", got)
	}
	pathDir := t.TempDir()
	onPath := filepath.Join(pathDir, "dcstore-onpath")
	if err := os.WriteFile(onPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)
	if got := toolBinary("MANVI_TEST_UNSET_PATH", "dcstore-onpath"); got != onPath {
		t.Fatalf("PATH lookup = %q, want %q", got, onPath)
	}
}
