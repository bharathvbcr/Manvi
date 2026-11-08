package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAgentWritableSeesACaseVariantSpellingOfTheProjectRoot: on a volume whose
// names are not case-exact (APFS and NTFS by default) the project root spelled
// .../repo and a tool directory spelled .../Repo/bin are one place, so the tool
// is agent-writable. A spelling-only containment test calls it outside, and a
// release build then runs an agent-planted dcverify or dcstore outside the
// gate. agentWritable must give the answer mcp.PathIsInside gives.
func TestAgentWritableSeesACaseVariantSpellingOfTheProjectRoot(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(base, "Repo")
	binDir := filepath.Join(real, "bin")
	elsewhere := filepath.Join(base, "elsewhere")
	for _, d := range []string{binDir, elsewhere} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(base, strings.ToLower("Repo"))); err != nil {
		t.Skipf("%s volume at %s is case-sensitive; a case-variant spelling names another path", runtime.GOOS, base)
	}
	tool := filepath.Join(binDir, "dcverify")
	writeExecutable(t, tool, "x")

	// The cwd is elsewhere so only the project root can make the tool inside.
	advChdir(t, elsewhere)
	t.Setenv("MANVI_PROJECT_ROOT", filepath.Join(base, "repo"))
	if !agentWritable(tool) {
		t.Errorf("%s is inside the project root %s on a case-insensitive volume", tool, filepath.Join(base, "repo"))
	}

	t.Setenv("PATH", binDir)
	if got, ok := trustedLookPath("dcverify"); ok {
		t.Errorf("trustedLookPath accepted %q from a directory inside the project root", got)
	}
}
