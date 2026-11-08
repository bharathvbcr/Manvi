package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The managed coding agents (claude, agy) run on the host, outside the gate,
// exactly as dcverify does, so a release build must resolve them by the same
// trust rule: every candidate is accepted only through trustedLookPath or
// trustedResolved, and nothing trusted means unresolvedTool(name), never a bare
// name that os/exec would search through PATH again.

// managedTrustWorkspace makes a project root the agent can write, makes it the
// working directory, stands the harness in as a release binary installed
// elsewhere, and gives the test an empty HOME outside the workspace.
func managedTrustWorkspace(t *testing.T) (root, home string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shebang and symlink fixtures")
	}
	skipIfSystemAgentInstalled(t)
	root = t.TempDir()
	advChdir(t, root)
	t.Setenv("MANVI_PROJECT_ROOT", root)
	releaseStandIn(t, filepath.Join(t.TempDir(), "install"))
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("MANVI_CLAUDE_BINARY", "")
	t.Setenv("MANVI_AGY_BINARY", "")
	return root, home
}

// skipIfSystemAgentInstalled keeps the "nothing trusted" assertions honest on a
// machine that really has an agent in one of the fixed system locations.
func skipIfSystemAgentInstalled(t *testing.T) {
	t.Helper()
	for _, p := range []string{"/opt/homebrew/bin/claude", "/usr/local/bin/claude", "/opt/homebrew/bin/agy", "/usr/local/bin/agy"} {
		if _, err := os.Stat(p); err == nil {
			t.Skipf("%s exists on this machine and is a legitimate trusted candidate", p)
		}
	}
}

type managedAgent struct {
	env, name string
	// installDir returns the directory under home that the resolver searches.
	installDir func(home string) string
}

func managedAgents() []managedAgent {
	return []managedAgent{
		{"MANVI_CLAUDE_BINARY", "claude", func(home string) string { return filepath.Join(home, ".local", "share", "claude", "versions") }},
		{"MANVI_AGY_BINARY", "agy", func(home string) string { return filepath.Join(home, ".local", "bin") }},
	}
}

func installFile(a managedAgent, home string) string {
	if a.name == "claude" {
		return filepath.Join(a.installDir(home), "2.1.319")
	}
	return filepath.Join(a.installDir(home), a.name)
}

func TestAReleaseBuildNeverResolvesAManagedAgentFromAnAgentWritablePATHEntry(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			root, _ := managedTrustWorkspace(t)
			inRepo := filepath.Join(root, "bin")
			writeExecFile(t, filepath.Join(inRepo, a.name), fakeMachO())
			t.Setenv("PATH", inRepo)
			if got := toolBinary(a.env, a.name); got != unresolvedTool(a.name) {
				t.Fatalf("toolBinary(%s) = %q from a PATH entry inside the workspace, want %q", a.name, got, unresolvedTool(a.name))
			}
		})
	}
}

func TestAReleaseBuildNeverReturnsAManagedAgentWrapperOnATrustedPATH(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			managedTrustWorkspace(t)
			trusted := t.TempDir()
			writeExecFile(t, filepath.Join(trusted, a.name), []byte("#!/bin/sh\nexec real \"$@\"\n"))
			t.Setenv("PATH", trusted)
			if got := toolBinary(a.env, a.name); got != unresolvedTool(a.name) {
				t.Fatalf("toolBinary(%s) = %q, a shell wrapper; want %q", a.name, got, unresolvedTool(a.name))
			}
		})
	}
}

func TestAReleaseBuildNeverResolvesAManagedAgentFromAnInstallDirInsideTheWorkspace(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			root, _ := managedTrustWorkspace(t)
			// HOME inside the repository: the install directories the resolver
			// derives from it are agent-writable.
			home := filepath.Join(root, "home")
			t.Setenv("HOME", home)
			writeExecFile(t, installFile(a, home), fakeMachO())
			t.Setenv("PATH", t.TempDir())
			if got := toolBinary(a.env, a.name); got != unresolvedTool(a.name) {
				t.Fatalf("toolBinary(%s) = %q from an install dir inside the workspace, want %q", a.name, got, unresolvedTool(a.name))
			}
		})
	}
}

func TestAReleaseBuildNeverFollowsAManagedAgentSymlinkIntoTheWorkspace(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			root, home := managedTrustWorkspace(t)
			planted := filepath.Join(root, "planted-"+a.name)
			writeExecFile(t, planted, fakeMachO())
			// A symlink in an operator-trusted directory and one in the install
			// dir: both lead into the agent's tree.
			trusted := t.TempDir()
			if err := os.Symlink(planted, filepath.Join(trusted, a.name)); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(a.installDir(home), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(planted, installFile(a, home)); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", trusted)
			if got := toolBinary(a.env, a.name); got != unresolvedTool(a.name) {
				t.Fatalf("toolBinary(%s) = %q, a path leading into the workspace; want %q", a.name, got, unresolvedTool(a.name))
			}
		})
	}
}

func TestAReleaseBuildSkipsAPATHWrapperAndReturnsTheTrustedResolvedAgent(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			managedTrustWorkspace(t)
			wrapperDir, realDir := t.TempDir(), t.TempDir()
			writeExecFile(t, filepath.Join(wrapperDir, a.name), []byte("#!/bin/sh\nexec real \"$@\"\n"))
			real := filepath.Join(realDir, a.name)
			writeExecFile(t, real, fakeMachO())
			// A symlink is the normal shape of an install; the resolved path is
			// what is judged and what is returned.
			link := filepath.Join(t.TempDir(), a.name)
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+filepath.Dir(link))
			if got := toolBinary(a.env, a.name); got != resolvedPath(t, real) {
				t.Fatalf("toolBinary(%s) = %q, want the resolved trusted image %q", a.name, got, resolvedPath(t, real))
			}
		})
	}
}

func TestAReleaseBuildFindsAManagedAgentInATrustedInstallDirPastAPATHWrapper(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			_, home := managedTrustWorkspace(t)
			wrapperDir := t.TempDir()
			writeExecFile(t, filepath.Join(wrapperDir, a.name), []byte("#!/bin/sh\nexec real \"$@\"\n"))
			want := installFile(a, home)
			writeExecFile(t, want, fakeMachO())
			t.Setenv("PATH", wrapperDir)
			if got := toolBinary(a.env, a.name); got != resolvedPath(t, want) {
				t.Fatalf("toolBinary(%s) = %q, want the trusted install %q", a.name, got, resolvedPath(t, want))
			}
		})
	}
}

func TestAReleaseBuildIgnoresARelativePATHEntryForAManagedAgent(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			root, _ := managedTrustWorkspace(t)
			writeExecFile(t, filepath.Join(root, "rel", a.name), fakeMachO())
			t.Setenv("PATH", "rel")
			if got := toolBinary(a.env, a.name); got != unresolvedTool(a.name) {
				t.Fatalf("toolBinary(%s) = %q through a relative PATH entry, want %q", a.name, got, unresolvedTool(a.name))
			}
		})
	}
}

func TestADeveloperBuildStillSkipsAWrapperAndIgnoresTheAgentWritableFilter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shebang fixtures")
	}
	skipIfSystemAgentInstalled(t)
	root := t.TempDir()
	advChdir(t, root)
	t.Setenv("MANVI_PROJECT_ROOT", root)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("MANVI_CLAUDE_BINARY", "")
	buildTreeBinary(t, t.TempDir())
	wrapperDir, inRepo := t.TempDir(), filepath.Join(root, "bin")
	writeExecFile(t, filepath.Join(wrapperDir, "claude"), []byte("#!/bin/sh\nexec real \"$@\"\n"))
	writeExecFile(t, filepath.Join(inRepo, "claude"), fakeMachO())
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+inRepo)
	if got := toolBinary("MANVI_CLAUDE_BINARY", "claude"); got != filepath.Join(inRepo, "claude") {
		t.Fatalf("developer build chose %q, want the launchable in-repo claude past the wrapper", got)
	}
}
