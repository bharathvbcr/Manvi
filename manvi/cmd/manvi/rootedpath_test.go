package main

import (
	"runtime"
	"testing"
)

// A target that names a root or a drive without being absolute is handed to the
// gate as written. On Windows "/etc/passwd" is that, and resolving it as
// "relative to the project" put it inside the repository: the permissive answer
// for a path the kernel opens elsewhere. It fails against the previous
// resolveCLIPath on Windows only, because a leading slash is absolute elsewhere.
func TestACLIPathRootedAtADriveIsNeverResolvedUnderTheProject(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	targets := []string{"/etc/passwd", "/tmp/x"}
	if runtime.GOOS == "windows" {
		targets = append(targets, `\etc\passwd`, "C:foo", "D:bar/baz")
	}
	for _, target := range targets {
		if got := resolveCLIPath(root, cwd, target); got != target {
			t.Errorf("resolveCLIPath(%q) = %q; want the target unchanged for the gate to refuse", target, got)
		}
	}
}
