package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// toolBinary resolves one of the executables this harness execs.
//
// The order is explicit-beats-PATH-beats-sibling:
//
//  1. the environment variable, because an operator who named a path meant it;
//  2. PATH, preferring a direct launchable image over a shell wrapper for
//     managed coding agents whose PID identity is verified by the host;
//  3. known native installation directories (for example
//     ~/.local/share/claude/versions/<highest-semver>) or workspace crates/target.
//
// An explicitly named path is never second-guessed, and when only a shell
// wrapper exists on PATH it is still returned so the adapter produces its typed
// wrapper refusal naming the environment variable to set.
func toolBinary(envKey, name string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	switch name {
	case "claude":
		return resolveManagedAgentBinary(name, codingagent.CheckLaunchableClaude, claudeInstallCandidates)
	case "agy":
		return resolveManagedAgentBinary(name, codingagent.CheckLaunchableAgy, agyInstallCandidates)
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

func resolveManagedAgentBinary(name string, check func(string) error, fallbackCandidates func() []string) string {
	firstFound, lookErr := exec.LookPath(name)
	if lookErr == nil && check(firstFound) == nil {
		return firstFound
	}
	exeNames := []string{name}
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(name), ".exe") {
		exeNames = []string{name + ".exe", name}
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		for _, exe := range exeNames {
			candidate := filepath.Join(dir, exe)
			if filepath.IsAbs(candidate) && check(candidate) == nil {
				return candidate
			}
		}
	}
	for _, candidate := range fallbackCandidates() {
		if filepath.IsAbs(candidate) && check(candidate) == nil {
			return candidate
		}
	}
	if lookErr == nil {
		return firstFound
	}
	return name
}

type semverCandidate struct {
	major, minor, patch int
	prerelease          string
	path                string
}

func parseSemverName(name string) (major, minor, patch int, prerelease string, ok bool) {
	core := name
	if idx := strings.IndexByte(name, '-'); idx >= 0 {
		core = name[:idx]
		prerelease = name[idx+1:]
		if prerelease == "" {
			return 0, 0, 0, "", false
		}
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return 0, 0, 0, "", false
	}
	var err error
	if major, err = strconv.Atoi(parts[0]); err != nil || major < 0 {
		return 0, 0, 0, "", false
	}
	if minor, err = strconv.Atoi(parts[1]); err != nil || minor < 0 {
		return 0, 0, 0, "", false
	}
	if patch, err = strconv.Atoi(parts[2]); err != nil || patch < 0 {
		return 0, 0, 0, "", false
	}
	return major, minor, patch, prerelease, true
}

func claudeInstallCandidates() []string {
	var dirs []string
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" && filepath.IsAbs(xdg) {
		dirs = append(dirs, filepath.Join(xdg, "claude", "versions"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && filepath.IsAbs(home) {
		dirs = append(dirs, filepath.Join(home, ".local", "share", "claude", "versions"))
	}
	var found []semverCandidate
	seenDirs := map[string]bool{}
	for _, dir := range dirs {
		if seenDirs[dir] {
			continue
		}
		seenDirs[dir] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		if len(entries) > 256 {
			entries = entries[:256]
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			maj, min, pat, pre, ok := parseSemverName(entry.Name())
			if !ok {
				continue
			}
			found = append(found, semverCandidate{
				major:      maj,
				minor:      min,
				patch:      pat,
				prerelease: pre,
				path:       filepath.Join(dir, entry.Name()),
			})
		}
	}
	sort.Slice(found, func(i, j int) bool {
		a, b := found[i], found[j]
		if a.major != b.major {
			return a.major > b.major
		}
		if a.minor != b.minor {
			return a.minor > b.minor
		}
		if a.patch != b.patch {
			return a.patch > b.patch
		}
		if (a.prerelease == "") != (b.prerelease == "") {
			return a.prerelease == ""
		}
		return a.prerelease > b.prerelease
	})
	out := make([]string, 0, len(found)+3)
	for _, c := range found {
		out = append(out, c.path)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && filepath.IsAbs(home) {
		out = append(out, filepath.Join(home, ".local", "bin", "claude.real"))
	}
	if runtime.GOOS != "windows" {
		out = append(out, "/opt/homebrew/bin/claude", "/usr/local/bin/claude")
	}
	return out
}

func agyInstallCandidates() []string {
	exe := "agy"
	if runtime.GOOS == "windows" {
		exe = "agy.exe"
	}
	var out []string
	if home, err := os.UserHomeDir(); err == nil && home != "" && filepath.IsAbs(home) {
		out = append(out,
			filepath.Join(home, ".local", "bin", exe),
			filepath.Join(home, ".antigravity", "antigravity", "bin", exe),
			filepath.Join(home, "Applications", "Antigravity.app", "Contents", "Resources", "app", "bin", exe),
		)
	}
	if runtime.GOOS != "windows" {
		out = append(out,
			"/opt/homebrew/bin/agy",
			"/usr/local/bin/agy",
			"/Applications/Antigravity.app/Contents/Resources/app/bin/agy",
		)
	}
	return out
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
