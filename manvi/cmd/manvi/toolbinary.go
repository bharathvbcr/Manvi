package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
	"github.com/bharathvbcr/Manvi/manvi/mcp"
)

// toolExecutable names the running binary. It is a variable so a test can stand
// the harness somewhere other than the test binary's temp directory.
var toolExecutable = os.Executable

// buildInfo reads the binary's build info, as currentBuild does. A variable so a
// test can present the main-module version `go install module@vX` records.
var buildInfo = debug.ReadBuildInfo

// releaseBuild reports whether this binary is a release: one with the
// -ldflags "-X main.stampedVersion=..." stamp that scripts/release-build.sh
// sets, or one `go install module@vX` built, whose main module carries a real
// version. It asks currentBuild, the source `manvi version` reports from, so the
// two cannot disagree about what a release is. A build with neither, such as
// scripts/.dev-build.sh's, is a developer's own.
func releaseBuild() bool {
	info, ok := buildInfo()
	return readBuildIdentity(info, ok, stampedVersion).Version != ""
}

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

// agentWritable reports whether path lies inside the project root or the
// working directory: the two places the agent can write, by `cargo build` or by
// plain file writes. Containment is mcp.PathIsInside, the repository's one rule
// for "could a checked-out tree have written this file": it resolves symlinks,
// compares by path rather than string prefix, and asks the file system when the
// spelling differs only in case on a case-insensitive volume. A path or root it
// cannot examine is treated as agent-writable, failing closed.
//
// A containing directory that is the filesystem root is ignored: a daemon
// started with cwd "/" would otherwise distrust every installed binary.
func agentWritable(path string) bool {
	roots := []string{projectRoot()}
	if cwd, err := os.Getwd(); err == nil {
		roots = append(roots, cwd)
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		if clean := filepath.Clean(root); clean == filepath.Dir(clean) {
			continue
		}
		inside, err := mcp.PathIsInside(root, path)
		if err != nil || inside {
			return true
		}
	}
	return false
}

// trustedResolved returns candidate with every symlink resolved, and whether
// that resolved path is outside the agent's reach. The path returned is the one
// whose containment was judged, so what is executed is what was checked: a
// symlink the agent can retarget between the check and the exec is not left in
// the path. A candidate that cannot be resolved is not trusted.
func trustedResolved(candidate string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil || agentWritable(resolved) {
		return "", false
	}
	return resolved, true
}

// trustedLookPath is exec.LookPath for a release build: it takes the first
// PATH entry that is an absolute directory holding the program and not
// agent-writable, and returns the program's fully resolved path (see
// trustedResolved). A relative or empty entry is skipped outright.
func trustedLookPath(name string) (string, bool) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if !filepath.IsAbs(dir) {
			continue
		}
		p, err := exec.LookPath(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if resolved, ok := trustedResolved(p); ok {
			return resolved, true
		}
	}
	return "", false
}

// unresolvedTool is what a release build returns when no trusted candidate
// exists: an absolute path that cannot exist, so os/exec reports "no such file"
// for it instead of searching PATH, and that still names the program looked
// for (and the missing "manvi-no-trusted-" directory says why).
func unresolvedTool(name string) string {
	return filepath.Join(string(filepath.Separator), "manvi-no-trusted-"+name, name)
}

// untrustedNote explains an unresolved release-build result for an operator,
// as doctor prints it: when the program does exist but only in a directory the
// agent can write (beside the binary or on PATH), it says where. It returns ""
// for a resolved value and for a program found nowhere.
func untrustedNote(name, resolved string) string {
	if resolved != unresolvedTool(name) {
		return ""
	}
	var candidates []string
	if dir := ownDir(); dir != "" {
		candidates = append(candidates, filepath.Join(dir, name))
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.IsAbs(dir) {
			candidates = append(candidates, filepath.Join(dir, name))
		}
	}
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil && agentWritable(p) {
			return "found only in an agent-writable directory: " + p
		}
	}
	return "not found beside this binary or on PATH"
}

// toolBinary resolves one of the analysis-plane executables this harness execs.
// These run on the host, outside the gate, so where they come from is a trust
// decision: a directory the agent can write (its working directory, anything
// under the project) must never be a source of an executable.
//
// A release build resolves, in order:
//
//  1. the environment variable, because an operator who named a path meant it.
//     A relative value is made absolute against the working directory now: that
//     is what the operator meant when they launched manvi, and a client that
//     later runs the program with its own Dir (devmap and dcgrep run in the
//     project root) must not re-anchor it under the agent's tree;
//  2. the directory holding this binary, which is where an installer puts the
//     companions;
//  3. PATH.
//
// Candidates 2 and 3 are accepted only when their resolved path lies outside
// the project root and the working directory. A release binary run from
// ./dist inside the repository has a directory the agent can write, and an
// operator PATH can name a directory in the repository (./bin, a GOBIN inside
// it); both are agent-writable no matter who configured them, so such a
// candidate is skipped and the next is tried. The agent's shell cannot change
// the PATH of this already-running process, and relative PATH entries are
// ignored, so a PATH entry outside the tree is the operator's.
//
// A developer build, one that is not a release, tries PATH, then a cargo build
// tree found by walking up from the binary's own directory (crates/target,
// rust/target, DevCouncil/rust/target). The walk starts at the executable,
// never the working directory or the project root: `cargo build` in the
// agent's shell can write the latter, and a planted dcverify there would run on
// the host. That fallback exists because a failure wasted a working setup: the
// Rust binaries were built, sitting in crates/target/release, and every
// store-backed tool still said `exec: "dcstore": executable file not found in
// $PATH`. A developer build is the operator's own checkout, so it does not
// apply the agent-writable filter.
//
// An unfound binary is returned by bare name so the resulting error names what
// was looked for, except in a release build, which returns unresolvedTool(name):
// a bare name is searched through PATH again by os/exec at exec time, and that
// search would find the agent-writable PATH entry the filter above just skipped.
func toolBinary(envKey, name string) string {
	if v := os.Getenv(envKey); v != "" {
		if abs, err := filepath.Abs(v); err == nil {
			return abs
		}
		return v
	}
	switch name {
	case "claude":
		return resolveManagedAgentBinary(name, codingagent.CheckLaunchableClaude, claudeInstallCandidates)
	case "agy":
		return resolveManagedAgentBinary(name, codingagent.CheckLaunchableAgy, agyInstallCandidates)
	}
	if releaseBuild() {
		if dir := ownDir(); dir != "" {
			if candidate := filepath.Join(dir, name); isExecutableFile(candidate) {
				if resolved, ok := trustedResolved(candidate); ok {
					return resolved
				}
			}
		}
		if p, ok := trustedLookPath(name); ok {
			return p
		}
		return unresolvedTool(name)
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
