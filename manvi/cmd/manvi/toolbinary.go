package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"

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
	return trustedLookPathLaunchable(name, nil)
}

// trustedLookPathLaunchable is trustedLookPath with one more condition: the
// resolved image must satisfy launchable (nil accepts any). It is how a managed
// coding agent passes over a shell wrapper on a trusted PATH entry and takes the
// real binary behind it, without a second PATH walk or a second trust rule.
func trustedLookPathLaunchable(name string, launchable func(string) error) (string, bool) {
	return lookPathWhere(name, func(p string) (string, bool) {
		return trustedLaunchable(p, launchable)
	})
}

// trustedLaunchable is trustedResolved plus the launch-shape check, run on the
// resolved path so the file that was judged is the file that was inspected.
func trustedLaunchable(candidate string, launchable func(string) error) (string, bool) {
	resolved, ok := trustedResolved(candidate)
	if !ok {
		return "", false
	}
	if launchable != nil && launchable(resolved) != nil {
		return "", false
	}
	return resolved, true
}

// lookPathWhere walks the PATH entries that are absolute directories, in order,
// and returns what accept makes of the first executable named name that it
// accepts. A relative or empty entry is skipped outright: it would be anchored
// at the working directory, which the agent controls.
func lookPathWhere(name string, accept func(path string) (string, bool)) (string, bool) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if !filepath.IsAbs(dir) {
			continue
		}
		p, err := exec.LookPath(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if got, ok := accept(p); ok {
			return got, true
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
// The managed coding agents (claude, agy) follow the same rule with two
// additions: a candidate must also be a direct launch rather than a shell
// wrapper (see managedAgentSpec), and the native install directories are tried
// after PATH, each through the same trustedResolved check. A developer build
// skips a wrapper on PATH but, finding no launchable image, returns the
// wrapper so the adapter can refuse with the variable to set.
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
	launchable, installs := managedAgentSpec(name)
	if releaseBuild() {
		if p, ok := releaseSearch(name, installs, func(p string) (string, bool) { return trustedLaunchable(p, launchable) }); ok {
			return p
		}
		return unresolvedTool(name)
	}
	if launchable != nil {
		return developerManagedAgent(name, launchable, installs)
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

// releaseSearch is the one list of places a release build looks for a program,
// in order: the directory holding this binary, each absolute PATH entry, then
// the native install directories. accept judges each executable candidate and
// returns what to use. toolBinary and managedAgentBinary both search through it,
// with different accept rules, so their sources and order cannot drift.
func releaseSearch(name string, installs func() []string, accept func(path string) (string, bool)) (string, bool) {
	if dir := ownDir(); dir != "" {
		if candidate := filepath.Join(dir, name); isExecutableFile(candidate) {
			if got, ok := accept(candidate); ok {
				return got, true
			}
		}
	}
	if got, ok := lookPathWhere(name, accept); ok {
		return got, true
	}
	for _, candidate := range installs() {
		if filepath.IsAbs(candidate) && isExecutableFile(candidate) {
			if got, ok := accept(candidate); ok {
				return got, true
			}
		}
	}
	return "", false
}

// managedAgentBinary is toolBinary for a managed coding agent, with one
// addition for a release build that found nothing launchable. When PATH or an
// install directory holds a trusted (outside the agent's reach) program that is
// only a wrapper, it is returned instead of unresolvedTool, so the adapter's
// pre-spawn check refuses it with the typed wrapper error that names the
// variable to set (MANVI_CLAUDE_BINARY, MANVI_AGY_BINARY) and not a bare "no
// such file". Nothing is run: the adapter reads the file's head and refuses
// before any spawn, exactly as a developer build's wrapper fallback does.
func managedAgentBinary(envKey, name string) string {
	got := toolBinary(envKey, name)
	if got != unresolvedTool(name) || !releaseBuild() {
		return got
	}
	_, installs := managedAgentSpec(name)
	if p, ok := releaseSearch(name, installs, trustedResolved); ok {
		return p
	}
	return got
}

// managedAgentSpec says how a managed coding agent differs from the analysis
// tools: its image must be a direct launch (never a shell wrapper, whose child
// would fail Claude's process-identity check and, for agy, which has no such
// check, would leave the recorded pid describing the wrapper), and a native install directory
// is a further source when PATH has only a wrapper. Any other tool gets a nil
// predicate and no install directories, which leaves its resolution exactly as
// it was.
func managedAgentSpec(name string) (launchable func(string) error, installs func() []string) {
	switch name {
	case "claude":
		return codingagent.CheckLaunchableClaude, claudeInstallCandidates
	case "agy":
		return codingagent.CheckLaunchableAgy, agyInstallCandidates
	}
	return nil, func() []string { return nil }
}

// developerManagedAgent is the developer-build resolution for a managed agent:
// the first launchable image on PATH, then a native install directory, as the
// operator's own checkout has no agent-writable filter. When nothing launchable
// exists the first thing PATH names is still returned, wrapper and all, so the
// adapter reports its typed wrapper refusal naming the variable to set instead
// of a misleading "not found".
func developerManagedAgent(name string, launchable func(string) error, installs func() []string) string {
	if p, ok := lookPathWhere(name, func(p string) (string, bool) { return p, launchable(p) == nil }); ok {
		return p
	}
	for _, candidate := range installs() {
		if filepath.IsAbs(candidate) && launchable(candidate) == nil {
			return candidate
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
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

// maxVersionCandidates bounds how many installed versions are tried, newest first.
const maxVersionCandidates = 256

// semverCandidate is an installed version file; versions order by semverCompare
// (SemVer 2.0.0 section 11 precedence, prereleases included).
type semverCandidate struct {
	ver  semver
	path string
}

// systemClaudeInstalls and systemAgyInstalls are the fixed system locations a
// package manager puts the agents in. Variables so a test can stand a host that
// has them, or has none, without depending on the machine it runs on.
var (
	systemClaudeInstalls = []string{"/opt/homebrew/bin/claude", "/usr/local/bin/claude"}
	systemAgyInstalls    = []string{
		"/opt/homebrew/bin/agy",
		"/usr/local/bin/agy",
		"/Applications/Antigravity.app/Contents/Resources/app/bin/agy",
	}
)

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
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			ver, ok := parseSemver(entry.Name())
			if !ok {
				continue
			}
			found = append(found, semverCandidate{ver: ver, path: filepath.Join(dir, entry.Name())})
		}
	}
	sort.Slice(found, func(i, j int) bool { return semverCompare(found[i].ver, found[j].ver) > 0 })
	// Bound the work after the sort, never before it: capping the directory
	// listing first would drop the newest version whenever it sorts late.
	if len(found) > maxVersionCandidates {
		found = found[:maxVersionCandidates]
	}
	out := make([]string, 0, len(found)+3)
	for _, c := range found {
		out = append(out, c.path)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && filepath.IsAbs(home) {
		out = append(out, filepath.Join(home, ".local", "bin", "claude.real"))
	}
	if runtime.GOOS != "windows" {
		out = append(out, systemClaudeInstalls...)
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
		out = append(out, systemAgyInstalls...)
	}
	return out
}
