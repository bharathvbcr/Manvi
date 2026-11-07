package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Adversarial tests for card ft-6ff33aaf, criterion "shipped binaries never exec
// a tool binary from an agent-writable path". They reuse the stand-ins in
// toolbinary_test.go (releaseStandIn, buildTreeBinary, plantInCwd).

func advWriteScript(t *testing.T, path, marker string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\necho ran > '" + marker + "'\nexit 0\n"
	writeExecutable(t, path, body)
}

func advChdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

// A release binary run from inside the workspace (./dist/manvi-vX, which is
// where scripts/release-build.sh puts it, or GOBIN inside the repo) has an
// ownDir the agent can write. The agent drops dcverify next to it.
func TestAdvReleaseOwnDirInsideWorkspaceIsAgentWritable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	planted := filepath.Join(dist, "dcverify-adv")
	advWriteScript(t, planted, filepath.Join(root, "marker"))
	releaseStandIn(t, dist)
	advChdir(t, root)
	if got := toolBinary("MANVI_ADV_UNSET", "dcverify-adv"); got == planted {
		t.Fatalf("release build resolved %q from a directory under the agent's working directory", got)
	}
}

// ErrDot must hold end to end: a relative PATH entry (".", "bin", empty) with a
// planted binary in cwd must neither be returned by toolBinary nor executed by
// the bare-name exec that follows.
func TestAdvReleaseRelativePathEntriesNeverExecPlantedBinary(t *testing.T) {
	for _, entry := range []string{".", "bin", "", "./bin", "../" + "x"} {
		t.Run("entry="+entry, func(t *testing.T) {
			root := t.TempDir()
			marker := filepath.Join(root, "marker")
			dir := root
			switch entry {
			case "bin", "./bin":
				dir = filepath.Join(root, "bin")
			case "../x":
				dir = filepath.Join(filepath.Dir(root), "x")
				t.Cleanup(func() { _ = os.RemoveAll(dir) })
			}
			name := "dcverify-advdot"
			advWriteScript(t, filepath.Join(dir, name), marker)
			releaseStandIn(t, filepath.Join(t.TempDir(), "install"))
			advChdir(t, root)
			t.Setenv("PATH", entry)
			got := toolBinary("MANVI_ADV_UNSET_DOT", name)
			if filepath.IsAbs(got) || strings.ContainsRune(got, os.PathSeparator) {
				t.Fatalf("toolBinary returned %q for a relative PATH entry %q", got, entry)
			}
			cmd := exec.CommandContext(t.Context(), got) //nolint:gosec // runs the resolver's answer, a planted script under t.TempDir, to prove it does not execute
			cmd.Dir = root
			err := cmd.Run()
			if _, statErr := os.Stat(marker); statErr == nil {
				t.Fatalf("planted binary executed via PATH entry %q (err=%v)", entry, err)
			}
			if err == nil {
				t.Fatalf("exec of bare %q succeeded", got)
			}
		})
	}
}

// Same for a developer build: PATH="." with a planted binary must not run.
func TestAdvDeveloperBuildRelativePathEntryNeverExecs(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "marker")
	advWriteScript(t, filepath.Join(root, "dcverify-advdev"), marker)
	buildTreeBinary(t, t.TempDir())
	advChdir(t, root)
	t.Setenv("PATH", ".")
	got := toolBinary("MANVI_ADV_UNSET_DEVDOT", "dcverify-advdev")
	cmd := exec.CommandContext(t.Context(), got) //nolint:gosec // runs the resolver's answer, a planted script under t.TempDir, to prove it does not execute
	cmd.Dir = root
	_ = cmd.Run()
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("developer build executed the planted binary through PATH=. (resolved %q)", got)
	}
}

// An unrecognised stamp: whitespace must read as unstamped, consistently with
// `manvi version` (which also trims).
func TestAdvWhitespaceStampIsUnstamped(t *testing.T) {
	prev := stampedVersion
	t.Cleanup(func() { stampedVersion = prev })
	for _, s := range []string{" ", "\t\n", "  "} {
		stampedVersion = s
		if releaseBuild() {
			t.Errorf("stamp %q counted as a release", s)
		}
		if id := readBuildIdentity(nil, false, s); id.Version != "" {
			t.Errorf("version reports %q for stamp %q while releaseBuild is false", id.Version, s)
		}
	}
	stampedVersion = "v1"
	if !releaseBuild() {
		t.Error("v1 not a release")
	}
}

// An operator-named path is never second-guessed, but a RELATIVE one is
// resolved by exec against the child's cmd.Dir, which devmap's client sets to
// the project root (DevCouncil dc/devmap/devmap.go:1176). The agent controls
// that directory, so a relative value executes an agent-writable file.
func TestAdvRelativeOperatorPathIsNotLeftRelative(t *testing.T) {
	t.Setenv("MANVI_ADV_REL", "target/release/dcverify")
	got := toolBinary("MANVI_ADV_REL", "dcverify")
	if !filepath.IsAbs(got) && strings.ContainsRune(got, os.PathSeparator) {
		t.Fatalf("toolBinary = %q: relative path survives to exec", got)
	}
}

// Every build script or workflow that compiles ./cmd/manvi for distribution
// must stamp main.stampedVersion, or the artifact is a "developer build" that
// walks up from its own directory (inside the repo, for dist/) to
// crates/target. scripts/.dev-build.sh is a developer build by name and is
// meant to stay unstamped, so it is the one exemption.
func TestAdvEveryManviBuildScriptStampsTheVersion(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	var unstamped []string
	scan := func(path string) {
		if filepath.Base(path) == ".dev-build.sh" {
			return
		}
		raw, err := os.ReadFile(path) //nolint:gosec // a repository script path the test enumerated
		if err != nil {
			return
		}
		text := string(raw)
		if strings.Contains(text, "cmd/manvi") && regexp.MustCompile(`go\s+(-C\s+\S+\s+)?build`).MatchString(text) &&
			!strings.Contains(text, "stampedVersion") {
			unstamped = append(unstamped, path)
		}
	}
	for _, glob := range []string{"scripts/*.sh", "scripts/.*.sh", ".github/workflows/*.yml"} {
		matches, _ := filepath.Glob(filepath.Join(root, glob))
		for _, m := range matches {
			scan(m)
		}
	}
	if len(unstamped) > 0 {
		t.Errorf("these build manvi without -X main.stampedVersion: %s", strings.Join(unstamped, ", "))
	}
}

var advResolverCall = regexp.MustCompile(`exec\.LookPath\b|"crates", "target"|"rust", "target"|crates/target|os\.Executable\b`)

// Class sweep ratchet: the only non-test sources under manvi/ that may turn a
// tool name into a path are the files audited in the w10-toolbin review. A new
// resolver is a new trust decision and must be added here on purpose.
func TestAdvToolResolverSweepRatchet(t *testing.T) {
	allowed := map[string]string{
		"cmd/manvi/toolbinary.go":       "the resolver",
		"cmd/manvi/main.go":             "usage text only",
		"cmd/manvi/serve.go":            "",
		"codingagent/claude.go":         "LookPath of toolBinary output, requires absolute",
		"codingagent/codex.go":          "LookPath of toolBinary output",
		"devcouncil/workspace.go":       "LookPath of the configured DevmapBinary",
		"devcouncil/devbridge.go":       "operator-pinned env only",
		"serve/verify_checks.go":        "LookPath of the configured VerifierBinary",
		"cmd/anthropic-account/main.go": "lookPath injection for the account tool",
		"internal/contract/env.go":      "comment",
	}
	var found []string
	err := filepath.WalkDir(filepath.Join("..", ".."), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		raw, rerr := os.ReadFile(path) //nolint:gosec // a source path found by walking this module
		if rerr != nil {
			return rerr
		}
		if advResolverCall.Match(raw) {
			rel, _ := filepath.Rel(filepath.Join("..", ".."), path)
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range found {
		if _, ok := allowed[f]; !ok {
			t.Errorf("%s resolves an executable (LookPath / build-tree walk / os.Executable) and was not audited", f)
		}
	}
	for f := range allowed {
		if f == "cmd/manvi/serve.go" || f == "cmd/manvi/main.go" || f == "internal/contract/env.go" {
			continue
		}
		seen := false
		for _, g := range found {
			seen = seen || g == f
		}
		if !seen {
			t.Errorf("audited resolver %s no longer matches; refresh the sweep", f)
		}
	}
}

// Every construction of a store/devmap/dcgrep client or a Verifier/Devmap
// binary field in cmd/manvi goes through toolBinary, never a bare literal.
func TestAdvEveryToolClientIsBuiltFromToolBinary(t *testing.T) {
	pat := regexp.MustCompile(`(store\.New|devmap\.New|dcgrep\.New|NewWarmDevmapPool)\(([^)]*)|(VerifierBinary|DevmapBinary):\s*([^,\n]+)`)
	files, _ := filepath.Glob("*.go")
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, _ := os.ReadFile(f) //nolint:gosec // a Go source file in this package directory
		for _, m := range pat.FindAllStringSubmatch(string(raw), -1) {
			arg := m[2] + m[4]
			if strings.TrimSpace(arg) == "" || strings.Contains(arg, "func(") {
				continue
			}
			seen++
			if !strings.Contains(arg, "toolBinary(") {
				t.Logf("%s: %s", f, m[0])
				if strings.Contains(arg, `"`) {
					t.Errorf("%s builds a tool client from a literal: %s", f, m[0])
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("pattern matched no constructions; the sweep is vacuous")
	}
}

// Nothing in the harness may rewrite its own PATH or relax ErrDot.
func TestAdvNoPathMutationOrErrDotOptOut(t *testing.T) {
	bad := regexp.MustCompile(`os\.Setenv\("PATH"|execerrdot|\.Err = nil|\.Err=nil`)
	err := filepath.WalkDir(filepath.Join("..", ".."), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		raw, rerr := os.ReadFile(path) //nolint:gosec // a source path found by walking this module
		if rerr != nil {
			return rerr
		}
		if bad.Match(raw) {
			t.Errorf("%s mutates PATH or relaxes ErrDot", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The developer-build walk starts at the executable only. A dev binary run from
// elsewhere must not find crates/target above a planted cwd or MANVI roots.
func TestAdvDeveloperWalkIsBoundedAndExecutableRooted(t *testing.T) {
	root := t.TempDir()
	buildTreeBinary(t, root)
	dirs := workspaceBuildDirs()
	if len(dirs) == 0 {
		t.Fatal("no candidate directories")
	}
	cwd, _ := os.Getwd()
	for _, d := range dirs {
		if strings.HasPrefix(d, cwd+string(os.PathSeparator)) && !strings.HasPrefix(cwd, root) {
			t.Errorf("candidate %q derived from the working directory", d)
		}
	}
	if len(dirs) > 8*2*3 {
		t.Errorf("walk unbounded: %d candidates", len(dirs))
	}
}
