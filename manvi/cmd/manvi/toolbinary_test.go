package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
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
			writeExecutable(t, built, "#!/bin/sh\nexit 0\n")

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
	writeExecutable(t, planted, "#!/bin/sh\nexit 0\n")
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
	if got != unresolvedTool("dcverify-planted") {
		t.Fatalf("toolBinary = %q, want the unresolvable value when nothing trusted has it", got)
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
	writeExecutable(t, companion, "#!/bin/sh\nexit 0\n")
	releaseStandIn(t, install)
	if got := toolBinary("MANVI_TEST_UNSET_SIB", "dcstore-sibling"); got != resolvedPath(t, companion) {
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
	writeExecutable(t, onPath, "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", pathDir)
	if got := toolBinary("MANVI_TEST_UNSET_PATH", "dcstore-onpath"); got != resolvedPath(t, onPath) {
		t.Fatalf("PATH lookup = %q, want %q", got, onPath)
	}
}

// TestAReleaseBuildSkipsAPATHEntryInsideTheWorkspace: an operator PATH can name
// a directory in the repository, which the agent can write.
func TestAReleaseBuildSkipsAPATHEntryInsideTheWorkspace(t *testing.T) {
	root := t.TempDir()
	inRepo := filepath.Join(root, "bin")
	planted := filepath.Join(inRepo, "dcverify-pathin")
	advWriteScript(t, planted, filepath.Join(root, "marker"))
	trusted := t.TempDir()
	good := filepath.Join(trusted, "dcverify-pathin")
	advWriteScript(t, good, filepath.Join(root, "marker2"))
	advChdir(t, root)
	releaseStandIn(t, filepath.Join(t.TempDir(), "install"))
	t.Setenv("PATH", inRepo+string(os.PathListSeparator)+trusted)
	if got := toolBinary("MANVI_TEST_PATHIN", "dcverify-pathin"); got != resolvedPath(t, good) {
		t.Fatalf("toolBinary = %q, want the PATH entry outside the workspace %q", got, good)
	}
	t.Setenv("PATH", inRepo)
	if got := toolBinary("MANVI_TEST_PATHIN", "dcverify-pathin"); got != unresolvedTool("dcverify-pathin") {
		t.Fatalf("toolBinary = %q from a PATH inside the workspace", got)
	}
}

// TestAgentWritableIsByPathNotStringPrefix: a sibling that shares a prefix is
// outside, and a symlink inside that points out is outside too, while a
// symlink outside that points in is inside.
func TestAgentWritableIsByPathNotStringPrefix(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	evil := filepath.Join(base, "repo-evil")
	for _, d := range []string{root, evil} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	lookalike := filepath.Join(evil, "tool")
	inside := filepath.Join(root, "tool")
	for _, f := range []string{lookalike, inside} {
		writeExecutable(t, f, "x")
	}
	linkIn := filepath.Join(base, "link-to-inside")
	if err := os.Symlink(inside, linkIn); err != nil {
		t.Fatal(err)
	}
	advChdir(t, root)
	t.Setenv("MANVI_PROJECT_ROOT", root)
	if agentWritable(lookalike) {
		t.Errorf("%s is outside %s", lookalike, root)
	}
	if !agentWritable(inside) {
		t.Errorf("%s is inside %s", inside, root)
	}
	if !agentWritable(linkIn) {
		t.Errorf("a symlink resolving into %s must count as inside", root)
	}
	if !agentWritable(filepath.Join(root, "missing")) {
		t.Error("an unresolvable path must fail closed")
	}
}

// TestAGoInstalledBuildIsARelease: `go install module@vX` records a real
// main-module version, which is the one `manvi version` reports.
func TestAGoInstalledBuildIsARelease(t *testing.T) {
	prevInfo, prevStamp := buildInfo, stampedVersion
	t.Cleanup(func() { buildInfo, stampedVersion = prevInfo, prevStamp })
	stampedVersion = ""
	for version, want := range map[string]bool{"v1.2.3": true, "(devel)": false, "": false} {
		buildInfo = func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Main: debug.Module{Path: "m", Version: version}}, true
		}
		if got := releaseBuild(); got != want {
			t.Errorf("main module version %q: releaseBuild = %v, want %v", version, got, want)
		}
	}
	buildInfo = func() (*debug.BuildInfo, bool) { return nil, false }
	if releaseBuild() {
		t.Error("no build info and no stamp counted as a release")
	}
}

// writeExecutable writes an executable stand-in for a tool binary.
// resolvedPath is p with symlinks resolved: toolBinary returns the path whose
// containment it judged, so on a platform whose temp directory is itself a
// symlink (macOS /var -> /private/var) the same file has a different spelling.
func resolvedPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // the file must be executable to stand in for a tool; it lives under t.TempDir
		t.Fatal(err)
	}
}

// TestAReleaseBuildNeverFallsBackToExecTimePATHSearch: with nothing trusted,
// the value returned must not be a bare name, which os/exec would search
// through PATH again and resolve to the agent-writable entry just filtered out.
func TestAReleaseBuildNeverFallsBackToExecTimePATHSearch(t *testing.T) {
	root := t.TempDir()
	inRepo := filepath.Join(root, "bin")
	marker := filepath.Join(root, "marker")
	advWriteScript(t, filepath.Join(inRepo, "dcverify"), marker)
	advChdir(t, root)
	t.Setenv("MANVI_PROJECT_ROOT", root)
	releaseStandIn(t, filepath.Join(t.TempDir(), "install"))
	t.Setenv("PATH", inRepo)

	got := toolBinary("MANVI_TEST_NOFALLBACK", "dcverify")
	if !strings.Contains(got, "dcverify") {
		t.Fatalf("toolBinary = %q, want a value naming dcverify", got)
	}
	if err := exec.CommandContext(t.Context(), got).Run(); err == nil { //nolint:gosec // proves the resolver's answer does not execute the planted script
		t.Fatal("exec of the unresolved value succeeded")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("planted dcverify in an agent-writable PATH entry ran via %q", got)
	}
	if note := untrustedNote("dcverify", got); !strings.Contains(note, filepath.Join(inRepo, "dcverify")) || !strings.Contains(note, "agent-writable") {
		t.Fatalf("untrustedNote = %q, want it to name the untrusted candidate", note)
	}
	t.Setenv("PATH", t.TempDir())
	if note := untrustedNote("dcverify", toolBinary("MANVI_TEST_NOFALLBACK", "dcverify")); !strings.Contains(note, "not found") {
		t.Fatalf("untrustedNote for an absent program = %q", note)
	}
	if note := untrustedNote("dcverify", "/some/path/dcverify"); note != "" {
		t.Fatalf("untrustedNote for a resolved value = %q, want empty", note)
	}
}
