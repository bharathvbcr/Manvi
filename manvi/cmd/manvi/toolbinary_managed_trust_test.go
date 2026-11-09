package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
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
// machine that really has an agent in one of the fixed system locations: it
// stands those locations in as empty for the test, so the outcome does not
// depend on the host. (The name is kept for its callers; nothing is skipped.)
func skipIfSystemAgentInstalled(t *testing.T) {
	t.Helper()
	claude, agy := systemClaudeInstalls, systemAgyInstalls
	systemClaudeInstalls, systemAgyInstalls = nil, nil
	t.Cleanup(func() { systemClaudeInstalls, systemAgyInstalls = claude, agy })
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

// A fixed system install is a candidate only through the injectable list: a test
// that points it at a trusted launchable finds it, and with the list empty the
// same machine finds nothing.
func TestAReleaseBuildFindsAManagedAgentInAnInjectedSystemInstall(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			managedTrustWorkspace(t)
			system := filepath.Join(t.TempDir(), a.name)
			writeExecFile(t, system, fakeMachO())
			t.Setenv("PATH", t.TempDir())
			if got := toolBinary(a.env, a.name); got != unresolvedTool(a.name) {
				t.Fatalf("toolBinary(%s) = %q with no system install, want %q", a.name, got, unresolvedTool(a.name))
			}
			if a.name == "claude" {
				systemClaudeInstalls = []string{system}
			} else {
				systemAgyInstalls = []string{system}
			}
			if got := toolBinary(a.env, a.name); got != resolvedPath(t, system) {
				t.Fatalf("toolBinary(%s) = %q, want the injected system install %q", a.name, got, resolvedPath(t, system))
			}
		})
	}
}

// In a release build whose PATH holds only a wrapper, the managed agent's error
// is the typed wrapper refusal that names the variable to set, not "not found".
func TestAReleaseBuildWithOnlyAWrapperOnPATHNamesTheVariableToSet(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			managedTrustWorkspace(t)
			trusted := t.TempDir()
			wrapper := filepath.Join(trusted, a.name)
			writeExecFile(t, wrapper, []byte("#!/bin/sh\nexec real \"$@\"\n"))
			t.Setenv("PATH", trusted)
			if got := toolBinary(a.env, a.name); got != unresolvedTool(a.name) {
				t.Fatalf("toolBinary(%s) = %q, want %q (the resolver itself still trusts no wrapper)", a.name, got, unresolvedTool(a.name))
			}
			program := managedAgentBinary(a.env, a.name)
			if program != resolvedPath(t, wrapper) {
				t.Fatalf("managedAgentBinary(%s) = %q, want the wrapper %q so the adapter can refuse it by name", a.name, program, resolvedPath(t, wrapper))
			}
			var err error
			var typedClaude *codingagent.ClaudeWrapperError
			var typedAgy *codingagent.AgyWrapperError
			if a.name == "claude" {
				err = codingagent.CheckLaunchableClaude(program)
				if !errors.As(err, &typedClaude) {
					t.Fatalf("want a ClaudeWrapperError, got %v", err)
				}
			} else {
				err = codingagent.CheckLaunchableAgy(program)
				if !errors.As(err, &typedAgy) {
					t.Fatalf("want an AgyWrapperError, got %v", err)
				}
			}
			if !strings.Contains(err.Error(), a.env) {
				t.Fatalf("the refusal does not name %s: %v", a.env, err)
			}
		})
	}
}

// A wrapper inside the agent's reach is still never returned, by name or not.
func TestAReleaseBuildNeverReturnsAnAgentWritableWrapperForTheTypedRefusal(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			root, _ := managedTrustWorkspace(t)
			inRepo := filepath.Join(root, "bin")
			writeExecFile(t, filepath.Join(inRepo, a.name), []byte("#!/bin/sh\nexec real \"$@\"\n"))
			t.Setenv("PATH", inRepo)
			if got := managedAgentBinary(a.env, a.name); got != unresolvedTool(a.name) {
				t.Fatalf("managedAgentBinary(%s) = %q from an agent-writable directory, want %q", a.name, got, unresolvedTool(a.name))
			}
		})
	}
}

// SemVer 2.0.0 section 11: installed versions sort newest first by precedence,
// so a release outranks its prereleases and prerelease identifiers compare
// numerically when numeric and below alphanumeric ones.
func TestInstalledClaudeVersionsOrderBySemverPrecedence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	skipIfSystemAgentInstalled(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	ascending := []string{
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
		"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1-rc.1", "1.2.0", "2.0.0-9", "2.0.0-10", "2.0.0-x",
	}
	dir := filepath.Join(home, ".local", "share", "claude", "versions")
	for i := len(ascending) - 1; i >= 0; i-- { // written in the wrong order on purpose
		writeExecFile(t, filepath.Join(dir, ascending[i]), fakeMachO())
	}
	writeExecFile(t, filepath.Join(dir, "not-a-version"), fakeMachO())
	var got []string
	for _, p := range claudeInstallCandidates() {
		if filepath.Dir(p) == dir {
			got = append(got, filepath.Base(p))
		}
	}
	for i, j := 0, len(got)-1; i < j; i, j = i+1, j-1 {
		got[i], got[j] = got[j], got[i]
	}
	if strings.Join(got, " ") != strings.Join(ascending, " ") {
		t.Fatalf("newest-first order reversed =\n%v\nwant\n%v", got, ascending)
	}
}
