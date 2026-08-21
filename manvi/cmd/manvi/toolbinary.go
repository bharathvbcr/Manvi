package main

import (
	"os"
	"os/exec"
	"path/filepath"
)

// toolBinary resolves one of the analysis-plane executables this harness execs.
//
// The order is explicit-beats-PATH-beats-sibling:
//
//  1. the environment variable, because an operator who named a path meant it;
//  2. PATH, which is the installed case;
//  3. a build sitting in this repository's own crates/target.
//
// Step three exists because of a failure that wasted a working setup. The Rust
// binaries were built — they were sitting in crates/target/release — and every
// store-backed tool still failed with `exec: "dcstore": executable file not
// found in $PATH`. Nothing was broken except that a compiled artifact twenty
// directories away from the process had not been copied onto PATH, and the
// error named the symptom rather than that. The harness knows where its own
// workspace is; it can look there before declaring the binary missing.
//
// It is a fallback, not a search: an explicitly named path is never
// second-guessed, and an unfound binary is still returned by bare name so the
// resulting error names what was actually looked for.
func toolBinary(envKey, name string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
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
// sibling build, nearest first, release before debug.
//
// Release is preferred because a debug build of the verifier is slow enough to
// change how a turn feels, and an operator who has both almost certainly means
// the optimised one. Both the executable's own directory and the working
// directory are walked up from: the first covers a binary run from a build
// tree, the second covers `go run` and a binary installed elsewhere while the
// operator stands in the repository.
func workspaceBuildDirs() []string {
	var roots []string
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		roots = append(roots, filepath.Dir(exe))
	}
	if cwd, err := os.Getwd(); err == nil {
		roots = append(roots, cwd)
	}
	if root := projectRoot(); root != "" {
		roots = append(roots, root)
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
				candidate := filepath.Join(dir, "crates", "target", profile)
				if !seen[candidate] {
					seen[candidate] = true
					out = append(out, candidate)
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
