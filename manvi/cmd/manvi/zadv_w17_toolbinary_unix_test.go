//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

func advW17Versions(t *testing.T, names ...string) (versionsDir string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	withoutSystemAgentInstalls(t)
	versionsDir = filepath.Join(home, ".local", "share", "claude", "versions")
	for _, n := range names {
		writeExecFile(t, filepath.Join(versionsDir, n), fakeMachO())
	}
	return versionsDir
}

func advW17Order(t *testing.T, dir string) []string {
	t.Helper()
	var got []string
	for _, p := range claudeInstallCandidates() {
		if filepath.Dir(p) == dir {
			got = append(got, filepath.Base(p))
		}
	}
	return got
}

// Install order is SemVer 2.0.0 precedence, newest first, prereleases included,
// and names that are not SemVer never appear.
func TestAdvW17InstallOrderIsSemverPrecedence(t *testing.T) {
	dir := advW17Versions(t, "2.1.9", "2.1.10", "2.1.10-rc.2", "2.1.10-rc.10", "2.1.10-rc.1", "2.1.10-beta", "2.1.10-alpha.1",
		"2.1.10-1", "3.0.0", "3.0.0-rc.1", "v2.5.0", "10.0.0", "9.99.99", "junk", "2.01.0", "1.0.0-a..b", "1.0.0-", "2.1.10.tmp")
	want := []string{"10.0.0", "9.99.99", "3.0.0", "3.0.0-rc.1", "v2.5.0", "2.1.10", "2.1.10-rc.10", "2.1.10-rc.2", "2.1.10-rc.1", "2.1.10-beta", "2.1.10-alpha.1", "2.1.10-1", "2.1.9"}
	if got := advW17Order(t, dir); !slices.Equal(got, want) {
		t.Fatalf("order\n got %v\nwant %v", got, want)
	}
}

// The cap keeps the newest 256 even when they sort last in the directory listing,
// and a version name past the cap is dropped, not the newest.
func TestAdvW17InstallCapKeepsTheNewest(t *testing.T) {
	var names []string
	for i := 1; i <= 300; i++ {
		names = append(names, fmt.Sprintf("0.0.%d", i))
	}
	dir := advW17Versions(t, names...)
	got := advW17Order(t, dir)
	if len(got) != maxVersionCandidates || got[0] != "0.0.300" || got[len(got)-1] != "0.0.45" {
		t.Fatalf("kept %d, first %q last %q", len(got), got[0], got[len(got)-1])
	}
}

// Equal precedence (build metadata differs) must come out in the same order on
// every call, or the chosen binary changes between runs.
func TestAdvW17EqualPrecedenceOrderIsStable(t *testing.T) {
	var names []string
	for i := 0; i < 40; i++ {
		names = append(names, fmt.Sprintf("1.0.0+b%02d", i))
	}
	dir := advW17Versions(t, names...)
	first := advW17Order(t, dir)
	for i := 0; i < 50; i++ {
		if got := advW17Order(t, dir); !slices.Equal(got, first) {
			t.Fatalf("BREAK: order of equal-precedence versions changed between calls:\n%v\n%v", first, got)
		}
	}
}

var advW17Wrapper = []byte("#!/bin/sh\nexec real \"$@\"\n")

// A release build with only a wrapper on a trusted PATH returns that wrapper, so
// the adapter refuses it with the typed error naming the variable to set; nothing
// is run to find that out.
func TestAdvW17ReleaseWrapperOnTrustedPATHGivesTheTypedRefusal(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			managedTrustWorkspace(t)
			trusted := t.TempDir()
			writeExecFile(t, filepath.Join(trusted, a.name), advW17Wrapper)
			t.Setenv("PATH", trusted)
			got := managedAgentBinary(a.env, a.name)
			if got == unresolvedTool(a.name) {
				t.Fatalf("BREAK: managedAgentBinary = %q, want the trusted wrapper", got)
			}
			err := advW17Check(a.name, got)
			if err == nil || !strings.Contains(err.Error(), a.env) {
				t.Fatalf("launch check = %v, want a refusal naming %s", err, a.env)
			}
			var cw *codingagent.ClaudeWrapperError
			var aw *codingagent.AgyWrapperError
			if !errors.As(err, &cw) && !errors.As(err, &aw) {
				t.Fatalf("refusal %T is not the typed wrapper error", err)
			}
		})
	}
}

func advW17Check(name, program string) error {
	if name == "claude" {
		return codingagent.CheckLaunchableClaude(program)
	}
	return codingagent.CheckLaunchableAgy(program)
}

// A wrapper that sits beside the manvi binary (the installer's own directory,
// trusted, and the first place toolBinary looks) and is the only candidate: it is
// as trusted as one on PATH, so the typed refusal should name the variable too.
// PARTIAL: managedAgentBinary's fallback walks PATH and the install dirs but not
// the binary's own directory, so this reports a bare "no such file".
func TestAdvW17ReleaseWrapperBesideTheBinaryGivesTheTypedRefusal(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			managedTrustWorkspace(t)
			writeExecFile(t, filepath.Join(ownDir(), a.name), advW17Wrapper)
			t.Setenv("PATH", t.TempDir())
			if got := managedAgentBinary(a.env, a.name); got == unresolvedTool(a.name) {
				t.Fatalf("BREAK (PARTIAL): a trusted wrapper beside the manvi binary is passed over: got %q, want a typed wrapper refusal", got)
			}
		})
	}
}

// A wrapper that can be reached only through the workspace is never returned, by
// any route: PATH entry, symlink in a trusted directory, install directory under
// a workspace HOME, a workspace file behind a trusted wrapper-named symlink.
func TestAdvW17ReleaseNeverReturnsAWorkspaceWrapper(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			root, _ := managedTrustWorkspace(t)
			planted := filepath.Join(root, "planted-"+a.name)
			writeExecFile(t, planted, advW17Wrapper)
			writeExecFile(t, filepath.Join(root, "bin", a.name), advW17Wrapper)
			trusted := t.TempDir()
			if err := os.Symlink(planted, filepath.Join(trusted, a.name)); err != nil {
				t.Fatal(err)
			}
			// A chain: trusted link -> trusted link -> workspace file.
			hop := t.TempDir()
			if err := os.Symlink(filepath.Join(trusted, a.name), filepath.Join(hop, a.name)); err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(root, "home")
			t.Setenv("HOME", home)
			writeExecFile(t, installFile(a, home), advW17Wrapper)
			t.Setenv("PATH", strings.Join([]string{filepath.Join(root, "bin"), hop, trusted}, string(os.PathListSeparator)))
			if got := managedAgentBinary(a.env, a.name); got != unresolvedTool(a.name) {
				t.Fatalf("BREAK: managedAgentBinary = %q, want %q", got, unresolvedTool(a.name))
			}
		})
	}
}

// The cwd and project root can differ from each other and from the workspace the
// wrapper is in; each is a root the filter must apply.
func TestAdvW17ReleaseWrapperUnderTheWorkingDirectoryAloneIsNotReturned(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			managedTrustWorkspace(t)
			t.Setenv("MANVI_PROJECT_ROOT", t.TempDir()) // the project root is elsewhere
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			writeExecFile(t, filepath.Join(cwd, "bin", a.name), advW17Wrapper)
			t.Setenv("PATH", filepath.Join(cwd, "bin"))
			if got := managedAgentBinary(a.env, a.name); got != unresolvedTool(a.name) {
				t.Fatalf("BREAK: managedAgentBinary = %q for a wrapper under the cwd, want %q", got, unresolvedTool(a.name))
			}
		})
	}
}

// An operator's variable wins unchanged in both builds, even when it names a
// file that is not executable; the adapter, not the resolver, refuses it.
func TestAdvW17EnvVarNamingANonExecutableIsPassedThroughAndRefusedByTheAdapter(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			managedTrustWorkspace(t)
			f := filepath.Join(t.TempDir(), "not-exec")
			if err := os.WriteFile(f, fakeMachO(), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(a.env, f)
			if got := managedAgentBinary(a.env, a.name); got != f {
				t.Fatalf("managedAgentBinary = %q, want the operator's %q", got, f)
			}
			if err := advW17Check(a.name, f); err == nil {
				t.Fatalf("a non-executable file passed the launch check")
			}
			// A directory too.
			t.Setenv(a.env, t.TempDir())
			if err := advW17Check(a.name, os.Getenv(a.env)); err == nil {
				t.Fatalf("a directory passed the launch check")
			}
		})
	}
}

// A developer build is unchanged by managedAgentBinary.
func TestAdvW17DeveloperBuildManagedAgentBinaryEqualsToolBinary(t *testing.T) {
	for _, a := range managedAgents() {
		t.Run(a.name, func(t *testing.T) {
			withoutSystemAgentInstalls(t)
			if releaseBuild() {
				t.Skip("test binary reports itself as a release build")
			}
			trusted := t.TempDir()
			writeExecFile(t, filepath.Join(trusted, a.name), advW17Wrapper)
			t.Setenv("PATH", trusted)
			t.Setenv(a.env, "")
			t.Setenv("HOME", t.TempDir())
			if got, want := managedAgentBinary(a.env, a.name), toolBinary(a.env, a.name); got != want {
				t.Fatalf("managedAgentBinary = %q, toolBinary = %q", got, want)
			}
		})
	}
}
