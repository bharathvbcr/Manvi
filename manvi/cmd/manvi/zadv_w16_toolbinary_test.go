package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Adversarial regression tests for lane w16-agy, criterion A (release-build
// trust of the managed agents). They reuse managedTrustWorkspace and friends
// from toolbinary_managed_trust_test.go.

func advWrapperBody() []byte { return []byte("#!/bin/sh\nexec real \"$@\"\n") }

func advRequireRelease(t *testing.T, a managedAgent, got string) {
	t.Helper()
	if got != unresolvedTool(a.name) {
		t.Fatalf("toolBinary(%s) = %q, want %q", a.name, got, unresolvedTool(a.name))
	}
}

// A PATH entry that spells the repository with different letter case names the
// same directory on a case-insensitive volume; the filter must still see it.
func TestAdvW16ReleaseCaseVariantOfRepoPATHEntryIsStillAgentWritable(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			root, _ := managedTrustWorkspace(t)
			bin := filepath.Join(root, "bin")
			writeExecFile(t, filepath.Join(bin, a.name), fakeMachO())
			variant := strings.ToUpper(bin)
			if variant == bin {
				t.Skip("path has no letters to flip")
			}
			if _, err := os.Stat(variant); err != nil {
				t.Skip("volume is case-sensitive; no case variant of the repository exists")
			}
			t.Setenv("PATH", variant)
			advRequireRelease(t, a, toolBinary(a.env, a.name))
		})
	}
}

// Same, with the install directory reached through a case-flipped HOME.
func TestAdvW16ReleaseCaseVariantHomeInsideRepoIsStillAgentWritable(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			root, _ := managedTrustWorkspace(t)
			home := filepath.Join(root, "home")
			writeExecFile(t, installFile(a, home), fakeMachO())
			variant := strings.ToUpper(home)
			if _, err := os.Stat(variant); err != nil {
				t.Skip("volume is case-sensitive")
			}
			t.Setenv("HOME", variant)
			t.Setenv("PATH", t.TempDir())
			advRequireRelease(t, a, toolBinary(a.env, a.name))
		})
	}
}

// A PATH directory that is itself a symlink into the repository.
func TestAdvW16ReleasePATHDirSymlinkIntoRepoIsRejected(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			root, _ := managedTrustWorkspace(t)
			writeExecFile(t, filepath.Join(root, "bin", a.name), fakeMachO())
			link := filepath.Join(t.TempDir(), "trusted-looking")
			if err := os.Symlink(filepath.Join(root, "bin"), link); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", link)
			advRequireRelease(t, a, toolBinary(a.env, a.name))
		})
	}
}

// A chain of symlinks whose middle hop is in the repo and whose last hop is
// outside: what is returned must be the final resolved file, never a hop the
// agent can retarget between the check and the exec.
func TestAdvW16ReleaseReturnsFullyResolvedPathThroughAChainWithAnInRepoHop(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			root, _ := managedTrustWorkspace(t)
			real := filepath.Join(t.TempDir(), a.name)
			writeExecFile(t, real, fakeMachO())
			hop := filepath.Join(root, "hop")
			if err := os.Symlink(real, hop); err != nil {
				t.Fatal(err)
			}
			trusted := t.TempDir()
			if err := os.Symlink(hop, filepath.Join(trusted, a.name)); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", trusted)
			got := toolBinary(a.env, a.name)
			if got != resolvedPath(t, real) {
				t.Fatalf("toolBinary(%s) = %q, want the fully resolved %q", a.name, got, resolvedPath(t, real))
			}
			if r, err := filepath.EvalSymlinks(got); err != nil || r != got {
				t.Fatalf("returned path %q still contains a symlink (resolves to %q, %v)", got, r, err)
			}
		})
	}
}

// ".." in a PATH entry that climbs from a trusted directory into the repo.
func TestAdvW16ReleaseDotDotPATHEntryIntoRepoIsRejected(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			root, _ := managedTrustWorkspace(t)
			inRepo := filepath.Join(root, "bin")
			writeExecFile(t, filepath.Join(inRepo, a.name), fakeMachO())
			trusted := t.TempDir()
			rel, err := filepath.Rel(trusted, inRepo)
			if err != nil {
				t.Skip(err)
			}
			t.Setenv("PATH", trusted+string(filepath.Separator)+rel) // unclean on purpose: .../../..
			advRequireRelease(t, a, toolBinary(a.env, a.name))
			t.Setenv("PATH", root+"/sub/../bin")
			advRequireRelease(t, a, toolBinary(a.env, a.name))
		})
	}
}

// Empty, "." and ".." entries are anchored at the working directory.
func TestAdvW16ReleaseEmptyAndDotPATHEntriesAreIgnored(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			root, _ := managedTrustWorkspace(t)
			writeExecFile(t, filepath.Join(root, a.name), fakeMachO())
			sep := string(os.PathListSeparator)
			for _, p := range []string{"", sep, "." + sep, sep + ".", "./", "..", sep + sep + sep} {
				t.Setenv("PATH", p)
				advRequireRelease(t, a, toolBinary(a.env, a.name))
			}
		})
	}
}

// The working directory can differ from the project root; both are agent
// reachable, so an absolute PATH entry equal to the cwd alone must be refused.
func TestAdvW16ReleaseCwdDistinctFromProjectRootIsStillAgentWritable(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			managedTrustWorkspace(t)
			cwdDir := t.TempDir()
			advChdir(t, cwdDir)
			t.Setenv("MANVI_PROJECT_ROOT", t.TempDir())
			writeExecFile(t, filepath.Join(cwdDir, "tools", a.name), fakeMachO())
			t.Setenv("PATH", filepath.Join(cwdDir, "tools"))
			advRequireRelease(t, a, toolBinary(a.env, a.name))
		})
	}
}

// The install directory (not just the file) is a symlink into the repository.
func TestAdvW16ReleaseInstallDirSymlinkedIntoRepoIsRejected(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			root, home := managedTrustWorkspace(t)
			planted := filepath.Join(root, "planted-install")
			writeExecFile(t, filepath.Join(planted, filepath.Base(installFile(a, home))), fakeMachO())
			dir := a.installDir(home)
			if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(planted, dir); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", t.TempDir())
			advRequireRelease(t, a, toolBinary(a.env, a.name))
		})
	}
}

// XDG_DATA_HOME may be set inside the repository (an agent cannot change the
// host's environment, but an operator's direnv can); the versions dir under it is
// agent-writable and must not be trusted.
func TestAdvW16ReleaseXDGDataHomeInsideRepoIsRejectedForClaude(t *testing.T) {
	root, _ := managedTrustWorkspace(t)
	xdg := filepath.Join(root, "xdg")
	writeExecFile(t, filepath.Join(xdg, "claude", "versions", "9.9.9"), fakeMachO())
	t.Setenv("XDG_DATA_HOME", xdg)
	t.Setenv("PATH", t.TempDir())
	got := toolBinary("MANVI_CLAUDE_BINARY", "claude")
	if got != unresolvedTool("claude") {
		t.Fatalf("toolBinary(claude) = %q from XDG_DATA_HOME inside the repo", got)
	}
}

// A relative XDG_DATA_HOME is ignored (anchored at cwd otherwise).
func TestAdvW16ReleaseRelativeXDGDataHomeIsIgnored(t *testing.T) {
	root, _ := managedTrustWorkspace(t)
	writeExecFile(t, filepath.Join(root, "xdg", "claude", "versions", "9.9.9"), fakeMachO())
	t.Setenv("XDG_DATA_HOME", "xdg")
	t.Setenv("PATH", t.TempDir())
	if got := toolBinary("MANVI_CLAUDE_BINARY", "claude"); got != unresolvedTool("claude") {
		t.Fatalf("toolBinary(claude) = %q from a relative XDG_DATA_HOME", got)
	}
}

// A relative HOME must not produce install candidates anchored at the cwd.
func TestAdvW16ReleaseRelativeHomeIsIgnored(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			root, _ := managedTrustWorkspace(t)
			writeExecFile(t, installFile(a, "relhome"), fakeMachO())
			_ = root
			t.Setenv("HOME", "relhome")
			t.Setenv("PATH", t.TempDir())
			advRequireRelease(t, a, toolBinary(a.env, a.name))
		})
	}
}

// Hostile version names: overflow, signs, empty prerelease, a name that sorts
// above every real version but is a shell wrapper, and a prerelease with path
// punctuation. Only the genuine launchable image may come back.
func TestAdvW16ReleaseHostileVersionNamesNeverBeatTheRealImage(t *testing.T) {
	_, home := managedTrustWorkspace(t)
	versions := filepath.Join(home, ".local", "share", "claude", "versions")
	want := filepath.Join(versions, "2.1.7")
	writeExecFile(t, want, fakeMachO())
	for _, name := range []string{
		"99999999999999999999.0.0", "+9.+9.+9", "9.9.9-", "9.9.9-..", "9.9.9-a b", "09.09.09", "9.9", "9.9.9.9", "latest", ".hidden", "9.9.9-\x01",
	} {
		// Only names the file system accepts are planted; each is a Mach-O or a
		// wrapper, and neither may outrank 2.1.7 unless it is a legitimate semver
		// image, which none of these is.
		if err := os.WriteFile(filepath.Join(versions, name), []byte("#!/bin/sh\necho evil\n"), 0o755); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", t.TempDir())
	got := toolBinary("MANVI_CLAUDE_BINARY", "claude")
	if got != resolvedPath(t, want) {
		t.Fatalf("toolBinary(claude) = %q, want %q", got, resolvedPath(t, want))
	}
}

// The directory scan is capped; the cap must not hide the highest version.
func TestAdvW16ReleaseVersionScanCapDoesNotHideTheNewestVersion(t *testing.T) {
	_, home := managedTrustWorkspace(t)
	versions := filepath.Join(home, ".local", "share", "claude", "versions")
	for n := 0; n < 600; n++ {
		writeExecFile(t, filepath.Join(versions, fmt.Sprintf("1.0.%d", n)), fakeMachO())
	}
	t.Setenv("PATH", t.TempDir())
	want := filepath.Join(versions, "1.0.599")
	if got := toolBinary("MANVI_CLAUDE_BINARY", "claude"); got != resolvedPath(t, want) {
		t.Fatalf("toolBinary(claude) = %q, want the highest installed version %q (scan capped at 256 entries sorted by name)", got, want)
	}
}

// A release binary installed beside a wrapper `claude` must not return it.
func TestAdvW16ReleaseOwnDirWrapperIsSkipped(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			managedTrustWorkspace(t)
			install := t.TempDir()
			releaseStandIn(t, install)
			writeExecFile(t, filepath.Join(install, a.name), advWrapperBody())
			t.Setenv("PATH", t.TempDir())
			advRequireRelease(t, a, toolBinary(a.env, a.name))
		})
	}
}

// A release binary run from inside the repository: its own directory is
// agent-writable, so a managed agent beside it is refused.
func TestAdvW16ReleaseOwnDirInsideRepoIsRejected(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			root, _ := managedTrustWorkspace(t)
			dist := filepath.Join(root, "dist")
			writeExecFile(t, filepath.Join(dist, a.name), fakeMachO())
			releaseStandIn(t, dist)
			t.Setenv("PATH", t.TempDir())
			advRequireRelease(t, a, toolBinary(a.env, a.name))
		})
	}
}

// A dangling symlink and a directory named like the agent are not candidates.
func TestAdvW16ReleaseDanglingLinkAndDirectoryNamedLikeAgentAreSkipped(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			managedTrustWorkspace(t)
			d1, d2 := t.TempDir(), t.TempDir()
			if err := os.Symlink(filepath.Join(d1, "nowhere"), filepath.Join(d1, a.name)); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(d2, a.name), 0o750); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", d1+string(os.PathListSeparator)+d2)
			advRequireRelease(t, a, toolBinary(a.env, a.name))
		})
	}
}

// The developer build reaches a native install past a PATH that holds only a
// wrapper (ruling 1: developer semantics plus the wrapper skip).
func TestAdvW16DeveloperBuildResolvesNativeInstallPastAWrapperOnlyPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixtures")
	}
	withoutSystemAgentInstalls(t)
	root := t.TempDir()
	advChdir(t, root)
	t.Setenv("MANVI_PROJECT_ROOT", root)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("MANVI_CLAUDE_BINARY", "")
	buildTreeBinary(t, t.TempDir())
	wrapperDir := t.TempDir()
	writeExecFile(t, filepath.Join(wrapperDir, "claude"), advWrapperBody())
	want := filepath.Join(home, ".local", "share", "claude", "versions", "2.1.5")
	writeExecFile(t, want, fakeMachO())
	t.Setenv("PATH", wrapperDir)
	if got := toolBinary("MANVI_CLAUDE_BINARY", "claude"); got != want {
		t.Fatalf("developer build = %q, want the native install %q", got, want)
	}
}

// A developer build never returns a bare name when a wrapper is the only thing
// on PATH: it returns the wrapper so the adapter can refuse with the variable
// to set. (Documented behaviour; asserts the documented shape.)
func TestAdvW16DeveloperBuildWithOnlyAWrapperReturnsTheWrapperNotABareName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixtures")
	}
	withoutSystemAgentInstalls(t)
	root := t.TempDir()
	advChdir(t, root)
	t.Setenv("MANVI_PROJECT_ROOT", root)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("MANVI_CLAUDE_BINARY", "")
	buildTreeBinary(t, t.TempDir())
	wrapperDir := t.TempDir()
	writeExecFile(t, filepath.Join(wrapperDir, "claude"), advWrapperBody())
	t.Setenv("PATH", wrapperDir)
	got := toolBinary("MANVI_CLAUDE_BINARY", "claude")
	if !filepath.IsAbs(got) {
		t.Fatalf("developer build returned %q, not an absolute path", got)
	}
}

// Whatever the release build returns for a managed agent is absolute and
// resolved, including the env override, which is made absolute against cwd.
func TestAdvW16ReleaseEnvOverrideIsAlwaysAbsolute(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			managedTrustWorkspace(t)
			for _, v := range []string{"rel/" + a.name, "./" + a.name, "../" + a.name, a.name} {
				t.Setenv(a.env, v)
				if got := toolBinary(a.env, a.name); !filepath.IsAbs(got) {
					t.Fatalf("toolBinary with %s=%q returned %q, not absolute", a.env, v, got)
				}
			}
		})
	}
}
