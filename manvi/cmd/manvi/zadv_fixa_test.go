package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
)

// Adversarial tests for fix-a: agentWritable delegating to mcp.PathIsInside,
// trustedLookPath, and the durableAdvice / userConfigHint pair.

// advRealBase is a symlink-free temp directory.
func advRealBase(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return base
}

func advMkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
}

func advSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

// advRequireCaseInsensitive skips unless base's volume treats Probe and probe
// as one name. Detected at runtime, never assumed from GOOS.
func advRequireCaseInsensitive(t *testing.T, base string) {
	t.Helper()
	advMkdirs(t, filepath.Join(base, "CaseProbe"))
	if _, err := os.Stat(filepath.Join(base, "caseprobe")); err != nil {
		t.Skipf("volume at %s is case-sensitive", base)
	}
}

// advRelease puts the process in a release build whose executable lives in
// installDir, with an isolated project root and cwd.
func advRelease(t *testing.T, installDir, root, cwd string) {
	t.Helper()
	releaseStandIn(t, installDir)
	t.Setenv("MANVI_PROJECT_ROOT", root)
	advChdir(t, cwd)
}

// 1. Case-variant spellings.

// A case difference in a MIDDLE component of the root, tool spelled with the
// other case.
func TestAdvFixaCaseVariantMiddleComponent(t *testing.T) {
	base := advRealBase(t)
	advRequireCaseInsensitive(t, base)
	realRepo := filepath.Join(base, "Work", "Repo")
	bin := filepath.Join(realRepo, "bin")
	elsewhere := filepath.Join(base, "elsewhere")
	advMkdirs(t, bin, elsewhere)
	tool := filepath.Join(bin, "dcverify")
	writeExecutable(t, tool, "x")
	advChdir(t, elsewhere)

	t.Setenv("MANVI_PROJECT_ROOT", filepath.Join(base, "wORK", "Repo"))
	if !agentWritable(tool) {
		t.Error("root with a case-variant middle component did not contain the tool")
	}
	t.Setenv("MANVI_PROJECT_ROOT", filepath.Join(base, "Work", "Repo"))
	variantTool := filepath.Join(base, "wORK", "Repo", "bin", "dcverify")
	if !agentWritable(variantTool) {
		t.Error("tool spelled with a case-variant middle component was called outside the root")
	}
	t.Setenv("PATH", filepath.Join(base, "wORK", "rEPO", "bin"))
	if got, ok := trustedLookPath("dcverify"); ok {
		t.Errorf("trustedLookPath accepted %q", got)
	}
}

// The LAST component differs in case (root names the repo, tool dir spelled
// with a different last component), through the cwd root rather than the
// project root.
func TestAdvFixaCaseVariantLastComponentViaCwd(t *testing.T) {
	base := advRealBase(t)
	advRequireCaseInsensitive(t, base)
	repo := filepath.Join(base, "Repo")
	other := filepath.Join(base, "Other")
	advMkdirs(t, filepath.Join(repo, "bin"), other)
	tool := filepath.Join(repo, "bin", "dcverify")
	writeExecutable(t, tool, "x")
	t.Setenv("MANVI_PROJECT_ROOT", other)
	advChdir(t, filepath.Join(base, "rEPO"))
	if !agentWritable(tool) {
		t.Error("cwd spelled with a case-variant last component did not contain the tool")
	}
}

// A symlink that points at a case-variant spelling of the repo.
func TestAdvFixaSymlinkToCaseVariantPath(t *testing.T) {
	base := advRealBase(t)
	advRequireCaseInsensitive(t, base)
	repo := filepath.Join(base, "Repo")
	other := filepath.Join(base, "Other")
	advMkdirs(t, filepath.Join(repo, "bin"), other)
	writeExecutable(t, filepath.Join(repo, "bin", "dcverify"), "x")
	link := filepath.Join(other, "link")
	advSymlink(t, filepath.Join(base, "rEPO", "bin"), link)
	advChdir(t, other)
	t.Setenv("MANVI_PROJECT_ROOT", repo)

	if !agentWritable(filepath.Join(link, "dcverify")) {
		t.Error("a symlink to a case-variant spelling of the repo hid a tool inside it")
	}
	t.Setenv("PATH", link)
	if got, ok := trustedLookPath("dcverify"); ok {
		t.Errorf("trustedLookPath accepted %q via a case-variant symlink", got)
	}
}

// 2. Symlinks.

func TestAdvFixaSymlinkedPathEntryIntoRepo(t *testing.T) {
	base := advRealBase(t)
	repo := filepath.Join(base, "repo")
	good := filepath.Join(base, "good")
	other := filepath.Join(base, "other")
	advMkdirs(t, filepath.Join(repo, "bin"), good, other)
	writeExecutable(t, filepath.Join(repo, "bin", "dcverify"), "planted")
	writeExecutable(t, filepath.Join(good, "dcverify"), "trusted")
	link := filepath.Join(other, "tools")
	advSymlink(t, filepath.Join(repo, "bin"), link)
	advRelease(t, filepath.Join(base, "install"), repo, other)
	t.Setenv("PATH", link+string(os.PathListSeparator)+good)

	got, ok := trustedLookPath("dcverify")
	if !ok || got != filepath.Join(good, "dcverify") {
		t.Errorf("trustedLookPath = %q, %v; want the trusted binary in %s", got, ok, good)
	}
}

func TestAdvFixaRepoReachedThroughSymlink(t *testing.T) {
	base := advRealBase(t)
	repo := filepath.Join(base, "realrepo")
	other := filepath.Join(base, "other")
	advMkdirs(t, filepath.Join(repo, "bin"), other)
	tool := filepath.Join(repo, "bin", "dcverify")
	writeExecutable(t, tool, "x")
	alias := filepath.Join(other, "alias")
	advSymlink(t, repo, alias)
	advChdir(t, other)

	// The project root override is not resolved by projectRoot's test hook, so
	// both an alias root with a real tool and a real root with an alias tool
	// must agree.
	t.Setenv("MANVI_PROJECT_ROOT", alias)
	if !agentWritable(tool) {
		t.Error("a project root reached through a symlink did not contain the real tool path")
	}
	t.Setenv("MANVI_PROJECT_ROOT", repo)
	if !agentWritable(filepath.Join(alias, "bin", "dcverify")) {
		t.Error("a tool reached through a symlink to the repo was called outside it")
	}
	// cwd reached through the symlink.
	t.Setenv("MANVI_PROJECT_ROOT", other)
	advChdir(t, alias)
	if !agentWritable(tool) {
		t.Error("a cwd reached through a symlink did not contain the real tool path")
	}
}

// A symlink inside the repo that points OUT: the agent can retarget it, so the
// path the harness execs must not be the repo-side spelling. Recorded as an
// observation: it asserts the safe property and fails if the exec path runs
// through a link the agent owns.
func TestAdvFixaPathEntryIsAnOutwardSymlinkInsideRepo(t *testing.T) {
	base := advRealBase(t)
	repo := filepath.Join(base, "repo")
	outside := filepath.Join(base, "outside")
	cwd := filepath.Join(base, "cwd")
	advMkdirs(t, repo, outside, cwd)
	writeExecutable(t, filepath.Join(outside, "dcverify"), "x")
	link := filepath.Join(repo, "tools")
	advSymlink(t, outside, link)
	advRelease(t, filepath.Join(base, "install"), repo, cwd)
	t.Setenv("PATH", link)

	got, ok := trustedLookPath("dcverify")
	if ok && strings.HasPrefix(got, repo+string(filepath.Separator)) {
		t.Errorf("trustedLookPath returned %q, a path through a symlink the agent can retarget", got)
	}
}

// 3. Spelling variants of a path that is inside.

func TestAdvFixaDotDotAndTrailingSlashSpellings(t *testing.T) {
	base := advRealBase(t)
	repo := filepath.Join(base, "repo")
	other := filepath.Join(base, "other")
	advMkdirs(t, filepath.Join(repo, "bin"), other)
	writeExecutable(t, filepath.Join(repo, "bin", "dcverify"), "x")
	advRelease(t, filepath.Join(base, "install"), repo, other)

	for name, entry := range map[string]string{
		"dotdot":         filepath.Join(other, "..", "repo", "bin"),
		"dotdot-twice":   other + "/../other/../repo/bin",
		"trailing-slash": filepath.Join(repo, "bin") + "/",
		"double-slash":   repo + "//bin//",
		"dot-segment":    repo + "/./bin",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PATH", entry)
			if got, ok := trustedLookPath("dcverify"); ok {
				t.Errorf("PATH %q: trustedLookPath accepted %q", entry, got)
			}
		})
	}
	// A dotdot that escapes the repo is outside, not inside.
	t.Setenv("PATH", filepath.Join(repo, "bin", "..", "..", "other"))
	writeExecutable(t, filepath.Join(other, "dcverify"), "x")
	if _, ok := trustedLookPath("dcverify"); ok {
		t.Error("cwd is other; a binary there must be agent-writable")
	}
}

func TestAdvFixaRelativePathEntriesSkipped(t *testing.T) {
	base := advRealBase(t)
	repo := filepath.Join(base, "repo")
	good := filepath.Join(base, "good")
	advMkdirs(t, filepath.Join(repo, "bin"), good)
	marker := filepath.Join(repo, "marker")
	advWriteScript(t, filepath.Join(repo, "bin", "dcverify"), marker)
	advWriteScript(t, filepath.Join(repo, "dcverify"), marker)
	writeExecutable(t, filepath.Join(good, "dcverify"), "x")
	advRelease(t, filepath.Join(base, "install"), repo, repo)

	for _, entry := range []string{"bin", ".", "", "./bin", "..", "../repo/bin"} {
		t.Run("entry="+entry, func(t *testing.T) {
			t.Setenv("PATH", entry+string(os.PathListSeparator)+good)
			got, ok := trustedLookPath("dcverify")
			if !ok || got != filepath.Join(good, "dcverify") {
				t.Errorf("PATH entry %q: got %q, %v; want %s", entry, got, ok, filepath.Join(good, "dcverify"))
			}
			if bin := toolBinary("MANVI_ADV_FIXA_UNSET", "dcverify"); strings.HasPrefix(bin, repo) {
				t.Errorf("toolBinary returned %q from the repo", bin)
			}
		})
	}
}

// NFC vs NFD spelling of an accented directory on a volume that treats them as
// one name (APFS).
func TestAdvFixaUnicodeNormalisationNFCvsNFD(t *testing.T) {
	const nfc = "café"  // é precomposed
	const nfd = "café" // e + combining acute
	base := advRealBase(t)
	repo := filepath.Join(base, nfc)
	other := filepath.Join(base, "other")
	advMkdirs(t, filepath.Join(repo, "bin"), other)
	if _, err := os.Stat(filepath.Join(base, nfd)); err != nil {
		t.Skipf("volume at %s distinguishes NFC from NFD", base)
	}
	tool := filepath.Join(repo, "bin", "dcverify")
	writeExecutable(t, tool, "x")
	advChdir(t, other)

	t.Setenv("MANVI_PROJECT_ROOT", filepath.Join(base, nfd))
	if !agentWritable(tool) {
		t.Error("NFD root did not contain an NFC-spelled tool")
	}
	t.Setenv("MANVI_PROJECT_ROOT", repo)
	if !agentWritable(filepath.Join(base, nfd, "bin", "dcverify")) {
		t.Error("NFD-spelled tool was called outside an NFC root")
	}
}

// 4. PathIsInside error paths.

func advDenied(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) }) //nolint:gosec // a directory must be searchable again so t.TempDir can remove it
}

func TestAdvFixaUnexaminablePathFailsClosedAndResolutionContinues(t *testing.T) {
	base := advRealBase(t)
	repo := filepath.Join(base, "repo")
	good := filepath.Join(base, "good")
	denied := filepath.Join(base, "denied")
	advMkdirs(t, repo, good, filepath.Join(denied, "bin"))
	writeExecutable(t, filepath.Join(good, "dcverify"), "x")
	advDenied(t, denied)
	advRelease(t, filepath.Join(base, "install"), repo, repo)

	if !agentWritable(filepath.Join(denied, "bin", "dcverify")) {
		t.Error("a permission-denied component was read as outside the agent's tree")
	}
	missing := filepath.Join(base, "no-such-dir")
	t.Setenv("PATH", strings.Join([]string{missing, filepath.Join(denied, "bin"), good}, string(os.PathListSeparator)))
	got, ok := trustedLookPath("dcverify")
	if !ok || got != filepath.Join(good, "dcverify") {
		t.Errorf("got %q, %v; want %s", got, ok, filepath.Join(good, "dcverify"))
	}
}

// A root that cannot be examined must not let a tool through.
func TestAdvFixaUnexaminableRootFailsClosed(t *testing.T) {
	base := advRealBase(t)
	good := filepath.Join(base, "good")
	denied := filepath.Join(base, "denied")
	advMkdirs(t, good, filepath.Join(denied, "repo"))
	tool := filepath.Join(good, "dcverify")
	writeExecutable(t, tool, "x")
	advDenied(t, denied)
	advRelease(t, filepath.Join(base, "install"), filepath.Join(denied, "repo"), good)
	t.Setenv("PATH", good)
	if got, ok := trustedLookPath("dcverify"); ok {
		t.Errorf("an unexaminable project root still trusted %q", got)
	}
}

// A path that does not exist, inside the root, is writable (a build can create
// it); outside the root it is not.
func TestAdvFixaNonexistentPathInsideRoot(t *testing.T) {
	base := advRealBase(t)
	repo := filepath.Join(base, "repo")
	other := filepath.Join(base, "other")
	advMkdirs(t, repo, other)
	advChdir(t, other)
	t.Setenv("MANVI_PROJECT_ROOT", repo)
	if !agentWritable(filepath.Join(repo, "not", "yet", "dcverify")) {
		t.Error("a not-yet-created file inside the root was called outside it")
	}
}

// 5. The executable's own directory.

func TestAdvFixaExeDirInsideRepoIsSkippedForPATH(t *testing.T) {
	base := advRealBase(t)
	repo := filepath.Join(base, "repo")
	good := filepath.Join(base, "good")
	other := filepath.Join(base, "other")
	advMkdirs(t, filepath.Join(repo, "dist"), good, other)
	writeExecutable(t, filepath.Join(repo, "dist", "dcverify"), "planted")
	writeExecutable(t, filepath.Join(good, "dcverify"), "trusted")
	advRelease(t, filepath.Join(repo, "dist"), repo, other)
	t.Setenv("PATH", good)
	if got := toolBinary("MANVI_ADV_FIXA_UNSET", "dcverify"); got != filepath.Join(good, "dcverify") {
		t.Errorf("toolBinary = %q; want the PATH binary %s", got, filepath.Join(good, "dcverify"))
	}
	t.Setenv("PATH", other)
	if got := toolBinary("MANVI_ADV_FIXA_UNSET", "dcverify"); got != unresolvedTool("dcverify") {
		t.Errorf("toolBinary = %q; want unresolved", got)
	}
	if note := untrustedNote("dcverify", unresolvedTool("dcverify")); !strings.Contains(note, "agent-writable") {
		t.Errorf("untrustedNote = %q", note)
	}
}

// The executable reached through a symlink outside the repo, whose real home is
// inside it (ownDir resolves symlinks); and the reverse, a companion that is a
// symlink into the repo placed in a trusted directory.
func TestAdvFixaExeSymlinkAndCompanionSymlink(t *testing.T) {
	base := advRealBase(t)
	repo := filepath.Join(base, "repo")
	install := filepath.Join(base, "install")
	other := filepath.Join(base, "other")
	advMkdirs(t, filepath.Join(repo, "dist"), install, other)
	planted := filepath.Join(repo, "dist", "dcverify")
	writeExecutable(t, planted, "planted")
	writeExecutable(t, filepath.Join(repo, "dist", "manvi"), "x")
	advSymlink(t, filepath.Join(repo, "dist", "manvi"), filepath.Join(install, "manvi"))
	advSymlink(t, planted, filepath.Join(install, "dcverify"))
	advRelease(t, install, repo, other)
	t.Setenv("PATH", other)

	// ownDir resolves install/manvi to repo/dist; the companion symlink in
	// install must also not be trusted.
	if got := toolBinary("MANVI_ADV_FIXA_UNSET", "dcverify"); got != unresolvedTool("dcverify") {
		t.Errorf("toolBinary = %q; want unresolved (exe resolves into the repo)", got)
	}
	// Exe really outside, companion a symlink into the repo.
	realExe := filepath.Join(base, "realinstall")
	advMkdirs(t, realExe)
	writeExecutable(t, filepath.Join(realExe, "manvi"), "x")
	advSymlink(t, planted, filepath.Join(realExe, "dcverify"))
	releaseStandIn(t, realExe)
	if got := toolBinary("MANVI_ADV_FIXA_UNSET", "dcverify"); got != unresolvedTool("dcverify") {
		t.Errorf("toolBinary = %q; a companion symlinked into the repo was trusted", got)
	}
}

// The docs say the working directory counts as well as the project root.
func TestAdvFixaExeDirInsideCwdOnly(t *testing.T) {
	base := advRealBase(t)
	root := filepath.Join(base, "root")
	cwd := filepath.Join(base, "cwd")
	advMkdirs(t, root, filepath.Join(cwd, "dist"))
	writeExecutable(t, filepath.Join(cwd, "dist", "dcverify"), "planted")
	advRelease(t, filepath.Join(cwd, "dist"), root, cwd)
	t.Setenv("PATH", t.TempDir())
	if got := toolBinary("MANVI_ADV_FIXA_UNSET", "dcverify"); got != unresolvedTool("dcverify") {
		t.Errorf("toolBinary = %q; the exe dir under the cwd was trusted", got)
	}
}

// 6. durableAdvice / userConfigHint.

func advAdvice(t *testing.T, key string) string {
	t.Helper()
	return durableAdvice(key, "x")
}

func TestAdvFixaAdviceWithRelativeUserConfig(t *testing.T) {
	t.Setenv(envUserConfig, "rel/user.yaml")
	for _, key := range []string{"llm.tier.fast.model", "llm.provider.default"} {
		advice := advAdvice(t, key)
		if strings.Contains(advice, "rel/user.yaml") {
			t.Errorf("%s: advice names the relative path manvi rejects: %q", key, advice)
		}
		if !strings.Contains(advice, envUserConfig) {
			t.Errorf("%s: advice should point at %s: %q", key, envUserConfig, advice)
		}
	}
}

// MANVI_USER_CONFIG inside the repository is refused at start by
// loadUserConfig; the advice must not send the operator there.
func TestAdvFixaAdviceNeverNamesAUserConfigInsideTheRepo(t *testing.T) {
	root := advRealBase(t)
	advMkdirs(t, filepath.Join(root, ".devcouncil"))
	t.Setenv("MANVI_PROJECT_ROOT", root)
	advChdir(t, root)
	inside := filepath.Join(root, ".devcouncil", "user.yaml")
	t.Setenv(envUserConfig, inside)

	reg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if loadUserConfig(reg, inside, root) == nil {
		t.Fatal("precondition: loadUserConfig should refuse a user config inside the repository")
	}
	for _, key := range []string{"llm.tier.fast.model", "llm.provider.default"} {
		if advice := advAdvice(t, key); strings.Contains(advice, inside) {
			t.Errorf("%s: advice %q names %s, which manvi refuses at start", key, advice, inside)
		}
	}
}

// The default location (no MANVI_USER_CONFIG) can also land in the repo when
// the platform config dir does (XDG_CONFIG_HOME on Linux, HOME-derived).
func TestAdvFixaAdviceDefaultLocationInsideTheRepo(t *testing.T) {
	root := advRealBase(t)
	t.Setenv("MANVI_PROJECT_ROOT", root)
	advChdir(t, root)
	t.Setenv(envUserConfig, "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "cfg"))
	t.Setenv("HOME", filepath.Join(root, "home"))
	path, err := userConfigPath()
	if err != nil {
		t.Skipf("no user config dir: %v", err)
	}
	reg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if loadUserConfig(reg, path, root) == nil {
		t.Skip("default location not inside the repo on this platform")
	}
	if advice := advAdvice(t, "llm.tier.fast.model"); strings.Contains(advice, path) {
		t.Errorf("advice %q names %s, which manvi refuses at start", advice, path)
	}
}

func TestAdvFixaAdviceWithUserConfigUnset(t *testing.T) {
	t.Setenv(envUserConfig, "")
	advMkdirs(t, filepath.Join(advRealBase(t)))
	advice := advAdvice(t, "llm.tier.fast.model")
	if !strings.Contains(advice, "config") {
		t.Errorf("advice %q names no config", advice)
	}
	if strings.Contains(advice, configPath()) {
		t.Errorf("advice %q names the repository config", advice)
	}
}
