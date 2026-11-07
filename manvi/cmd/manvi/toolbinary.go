package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// toolExecutable names the running binary. It is a variable so a test can stand
// the harness somewhere other than the test binary's temp directory.
var toolExecutable = os.Executable

// releaseBuild reports whether this binary carries a release stamp, the same
// -ldflags "-X main.stampedVersion=..." that scripts/release-build.sh sets and
// `manvi version` reports. Every other build is a developer's own.
func releaseBuild() bool { return strings.TrimSpace(stampedVersion) != "" }

// ownDir is the directory holding the running binary, symlinks resolved, or ""
// when the platform cannot say.
func ownDir() string {
	exe, err := toolExecutable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}

// toolBinary resolves one of the analysis-plane executables this harness execs.
// These run on the host, outside the gate, so where they come from is a trust
// decision: a directory the agent can write (its working directory, anything
// under the project) must never be a source of an executable.
//
// A release-stamped binary resolves, in order:
//
//  1. the environment variable, because an operator who named a path meant it;
//  2. the directory holding this binary, which is where an installer puts the
//     companions and is not agent-writable;
//  3. PATH. The agent's shell cannot change the PATH of this already-running
//     process, so PATH is operator-controlled; os/exec also refuses a relative
//     PATH entry that resolves to a program in the current directory.
//
// A developer build, one with no release stamp, tries PATH before the
// environment's fallback of last resort: a cargo build tree found by walking up
// from the binary's own directory (crates/target, rust/target,
// DevCouncil/rust/target). The walk starts at the executable, never the working
// directory or the project root: `cargo build` in the agent's shell can write
// the latter, and a planted dcverify there would run on the host. That
// fallback exists because a failure wasted a working setup: the Rust binaries
// were built, sitting in crates/target/release, and every store-backed tool
// still said `exec: "dcstore": executable file not found in $PATH`.
//
// An explicitly named path is never second-guessed, and an unfound binary is
// returned by bare name so the resulting error names what was looked for.
func toolBinary(envKey, name string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	released := releaseBuild()
	if released {
		if dir := ownDir(); dir != "" {
			if candidate := filepath.Join(dir, name); isExecutableFile(candidate) {
				return candidate
			}
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	if released {
		return name
	}
	for _, dir := range workspaceBuildDirs() {
		candidate := filepath.Join(dir, name)
		if isExecutableFile(candidate) {
			return candidate
		}
	}
	return name
}

// workspaceBuildDirs lists the cargo output directories that could hold a
// sibling build, nearest first, release before debug, for a developer build.
//
// Release is preferred because a debug build of the verifier is slow enough to
// change how a turn feels, and an operator who has both almost certainly means
// the optimised one. Only the executable's own directory is walked up from,
// which covers a binary run from a build tree. The working directory and the
// project root are deliberately not roots: the agent controls both.
func workspaceBuildDirs() []string {
	var roots []string
	if dir := ownDir(); dir != "" {
		roots = append(roots, dir)
	}

	seen := map[string]bool{}
	var out []string
	for _, root := range roots {
		dir := root
		// Bounded rather than "until /" as a matter of habit: an unbounded walk
		// up a symlinked or deeply nested path is a loop waiting for an odd
		// filesystem, and no real checkout puts crates/ more than a handful of
		// levels above the binary.
		for i := 0; i < 8; i++ {
			for _, profile := range []string{"release", "debug"} {
				for _, rel := range []string{
					filepath.Join("crates", "target", profile),
					filepath.Join("rust", "target", profile),
					filepath.Join("DevCouncil", "rust", "target", profile),
				} {
					candidate := filepath.Join(dir, rel)
					if !seen[candidate] {
						seen[candidate] = true
						out = append(out, candidate)
					}
				}
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return out
}

// isExecutableFile reports whether a path is a regular file this process could
// exec. A directory named like the binary, or a non-executable file, is not a
// candidate — returning either would turn a clear "not found" into a confusing
// "permission denied" much later.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}
