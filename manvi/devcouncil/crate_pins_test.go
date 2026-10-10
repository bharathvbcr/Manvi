package devcouncil

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// crates/dc-* are symlinks into DevCouncil's rust/, so their sources cannot
// drift: there is one tree (DevCouncil rust/STATUS.md §6). What can drift is
// crates/Cargo.toml. A symlinked member's `foo.workspace = true` is resolved
// against *this* workspace's [workspace.dependencies], not DevCouncil's. A key
// missing here fails the workspace load and names it. A key present with a
// different version or feature set builds without a word, against a dependency
// DevCouncil's suite never ran. These tests make that second case a failure.
//
// DevCouncil's side is read at the commit scripts/module-pins.txt pins, not
// from whatever the sibling checkout has open: the release builds the pin, so a
// comparison against a newer or edited checkout would pass on one machine and
// fail in CI, or the reverse. This file is the one owner of the comparison;
// the hand-rolled TOML check that used to live in scripts/check-release.mjs
// was folded into it.
//
// The comparison is over what Cargo resolved, not over TOML text: `cargo
// metadata --no-deps` reports each member's dependencies with the inherited
// spec already substituted, so a pin that only differs in how it is written
// (inline table versus dotted key) compares equal, and one that differs in
// what it asks for does not.

// cargoMetadataTimeout bounds one `cargo metadata --no-deps --offline`, which
// reads manifests and nothing else; it answers in well under a second.
const cargoMetadataTimeout = 60 * time.Second

type cargoMetadata struct {
	Packages []cargoPackage `json:"packages"`
}

type cargoPackage struct {
	Name         string     `json:"name"`
	Version      string     `json:"version"`
	License      *string    `json:"license"`
	Edition      string     `json:"edition"`
	ManifestPath string     `json:"manifest_path"`
	Dependencies []cargoDep `json:"dependencies"`
}

// cargoDep is the part of a dependency that says what is asked for. `path` is
// left out on purpose: a path dependency between two members names a location
// under whichever workspace was asked, and that differs by construction.
type cargoDep struct {
	Name                string   `json:"name"`
	Req                 string   `json:"req"`
	Kind                *string  `json:"kind"`
	Optional            bool     `json:"optional"`
	UsesDefaultFeatures bool     `json:"uses_default_features"`
	Features            []string `json:"features"`
	Target              *string  `json:"target"`
	Rename              *string  `json:"rename"`
	Registry            *string  `json:"registry"`
}

func (d cargoDep) key() string {
	return fmt.Sprintf("%s kind=%s target=%s", d.Name, strOr(d.Kind, "normal"), strOr(d.Target, "any"))
}

func (d cargoDep) spec() string {
	features := append([]string(nil), d.Features...)
	sort.Strings(features)
	return fmt.Sprintf("req=%q features=%v default-features=%t optional=%t rename=%s registry=%s",
		d.Req, features, d.UsesDefaultFeatures, d.Optional, strOr(d.Rename, "-"), strOr(d.Registry, "-"))
}

func strOr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}

func readCargoMetadata(manifest string) (cargoMetadata, error) {
	cargo, err := exec.LookPath("cargo")
	if err != nil {
		return cargoMetadata{}, fmt.Errorf("cargo is not on PATH, so the crate pins could not be compared: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cargoMetadataTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cargo, "metadata", "--manifest-path", manifest, "--no-deps", "--format-version", "1", "--offline")
	// cargo may leave a helper holding the pipes after it is killed; this
	// bounds how long Wait waits on them.
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return cargoMetadata{}, fmt.Errorf("cargo metadata for %s did not finish within %s", manifest, cargoMetadataTimeout)
		}
		return cargoMetadata{}, fmt.Errorf("cargo metadata for %s: %w\n%s", manifest, err, stderr.String())
	}
	var md cargoMetadata
	if err := json.Unmarshal(stdout.Bytes(), &md); err != nil {
		return cargoMetadata{}, fmt.Errorf("decode cargo metadata for %s: %w", manifest, err)
	}
	return md, nil
}

// exportPinnedRust writes DevCouncil's rust/ tree as the commit sha has it,
// read through git in the sibling checkout, and returns the directory. Tracked
// files only, so an edit or an untracked file in the sibling cannot reach it.
func exportPinnedRust(t testing.TB, sibling, sha string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*cargoMetadataTimeout)
	defer cancel()
	dir := t.TempDir()
	archive := exec.CommandContext(ctx, "git", "-C", sibling, "archive", "--format=tar", sha, "rust")
	extract := exec.CommandContext(ctx, "tar", "-x", "-C", dir)
	archive.WaitDelay, extract.WaitDelay = 5*time.Second, 5*time.Second
	var archiveErr, extractErr bytes.Buffer
	archive.Stderr, extract.Stderr = &archiveErr, &extractErr
	pipe, err := archive.StdoutPipe()
	if err != nil {
		return "", err
	}
	extract.Stdin = pipe
	if err := extract.Start(); err != nil {
		return "", fmt.Errorf("tar: %w", err)
	}
	if err := archive.Run(); err != nil {
		_ = extract.Process.Kill()
		_ = extract.Wait()
		return "", fmt.Errorf("git archive %s rust in %s: %w\n%s\nrun scripts/fetch-modules.sh to put DevCouncil at the pin", sha, sibling, err, archiveErr.String())
	}
	if err := extract.Wait(); err != nil {
		return "", fmt.Errorf("extract pinned rust/: %w\n%s", err, extractErr.String())
	}
	return filepath.Join(dir, "rust"), nil
}

var modulePinLine = regexp.MustCompile(`^DevCouncil\t[^\t]+\t([0-9a-f]{40})\s*$`)

// pinnedDevCouncilSHA reads the DevCouncil commit out of scripts/module-pins.txt.
func pinnedDevCouncilSHA(root string) (string, error) {
	body, err := os.ReadFile(filepath.Join(root, "scripts", "module-pins.txt"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		if m := modulePinLine.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			return m[1], nil
		}
	}
	return "", errors.New("scripts/module-pins.txt does not pin DevCouncil with a full commit, so there is nothing to compare against")
}

// workspaceLints returns the normalized lines of every [workspace.lints*]
// table, which cargo metadata does not report. Comments and blank lines are
// dropped and spaces around `=` do not matter.
func workspaceLints(manifest string) []string {
	var out []string
	table := ""
	for _, raw := range strings.Split(manifest, "\n") {
		line := strings.TrimSpace(raw)
		if i := strings.Index(line, " #"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			table = strings.Trim(line, "[] ")
			continue
		}
		if strings.HasPrefix(table, "workspace.lints") {
			k, v, _ := strings.Cut(line, "=")
			out = append(out, table+": "+strings.TrimSpace(k)+" = "+strings.TrimSpace(v))
		}
	}
	sort.Strings(out)
	return out
}

func lintsOf(rustDir string) ([]string, error) {
	body, err := os.ReadFile(filepath.Join(rustDir, "Cargo.toml"))
	if err != nil {
		return nil, err
	}
	return workspaceLints(string(body)), nil
}

// compareCratePins checks that every member of the workspace at manviCrates is
// a DevCouncil crate reached through a symlink into siblingRust (the checkout
// the build resolves), and that each one resolves the same package fields,
// dependency specs and workspace lints here as in DevCouncil's own workspace at
// pinnedRust (the pinned commit). It returns every difference, not the first.
func compareCratePins(manviCrates, siblingRust, pinnedRust string) ([]string, error) {
	siblingReal, err := filepath.EvalSymlinks(siblingRust)
	if err != nil {
		return nil, fmt.Errorf("DevCouncil rust/ is not at %s (CI places it with scripts/fetch-modules.sh): %w", siblingRust, err)
	}
	manvi, err := readCargoMetadata(filepath.Join(manviCrates, "Cargo.toml"))
	if err != nil {
		return nil, err
	}
	upstream, err := readCargoMetadata(filepath.Join(pinnedRust, "Cargo.toml"))
	if err != nil {
		return nil, err
	}
	if len(manvi.Packages) == 0 {
		return nil, fmt.Errorf("cargo metadata for %s listed no members, so nothing was compared", manviCrates)
	}

	byName := make(map[string]cargoPackage, len(upstream.Packages))
	for _, p := range upstream.Packages {
		byName[p.Name] = p
	}

	var diffs []string
	for _, p := range manvi.Packages {
		real, err := filepath.EvalSymlinks(filepath.Dir(p.ManifestPath))
		if err != nil {
			return nil, fmt.Errorf("resolve member %s at %s: %w", p.Name, p.ManifestPath, err)
		}
		if !strings.HasPrefix(real+string(filepath.Separator), siblingReal+string(filepath.Separator)) {
			diffs = append(diffs, fmt.Sprintf("%s: %s is not a symlink into DevCouncil rust/ (%s); the dc-* sources have one owner", p.Name, real, siblingReal))
			continue
		}
		up, ok := byName[p.Name]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("%s: not a member of DevCouncil's workspace at the pin", p.Name))
			continue
		}
		if p.Version != up.Version {
			diffs = append(diffs, fmt.Sprintf("%s: version %q here but %q in DevCouncil", p.Name, p.Version, up.Version))
		}
		if strOr(p.License, "-") != strOr(up.License, "-") {
			diffs = append(diffs, fmt.Sprintf("%s: license %q here but %q in DevCouncil", p.Name, strOr(p.License, "-"), strOr(up.License, "-")))
		}
		if p.Edition != up.Edition {
			diffs = append(diffs, fmt.Sprintf("%s: edition %q here but %q in DevCouncil", p.Name, p.Edition, up.Edition))
		}
		diffs = append(diffs, compareDeps(p.Name, p.Dependencies, up.Dependencies)...)
	}
	here, err := lintsOf(manviCrates)
	if err != nil {
		return nil, err
	}
	there, err := lintsOf(pinnedRust)
	if err != nil {
		return nil, err
	}
	if strings.Join(here, "\n") != strings.Join(there, "\n") {
		diffs = append(diffs, fmt.Sprintf("[workspace.lints] here is %v but %v in DevCouncil", here, there))
	}
	return diffs, nil
}

// checkCratePinsAtPin compares root's crates/ workspace with DevCouncil at the
// commit pinned in root's scripts/module-pins.txt, read from the checkout beside
// root.
func checkCratePinsAtPin(t testing.TB, root string) ([]string, error) {
	t.Helper()
	sha, err := pinnedDevCouncilSHA(root)
	if err != nil {
		return nil, err
	}
	sibling := filepath.Join(root, "..", "DevCouncil")
	pinned, err := exportPinnedRust(t, sibling, sha)
	if err != nil {
		return nil, err
	}
	return compareCratePins(filepath.Join(root, "crates"), filepath.Join(sibling, "rust"), pinned)
}

func compareDeps(member string, here, there []cargoDep) []string {
	index := func(deps []cargoDep) map[string]string {
		m := make(map[string]string, len(deps))
		for _, d := range deps {
			m[d.key()] = d.spec()
		}
		return m
	}
	h, u := index(here), index(there)
	keys := make([]string, 0, len(h)+len(u))
	for k := range h {
		keys = append(keys, k)
	}
	for k := range u {
		if _, ok := h[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var diffs []string
	for _, k := range keys {
		hs, inHere := h[k]
		us, inThere := u[k]
		switch {
		case !inThere:
			diffs = append(diffs, fmt.Sprintf("%s: %s only here: %s", member, k, hs))
		case !inHere:
			diffs = append(diffs, fmt.Sprintf("%s: %s only in DevCouncil: %s", member, k, us))
		case hs != us:
			diffs = append(diffs, fmt.Sprintf("%s: %s resolves to %s here but %s in DevCouncil", member, k, hs, us))
		}
	}
	return diffs
}

func TestCratePinsMatchDevCouncil(t *testing.T) {
	root := repoRoot(t)
	diffs, err := checkCratePinsAtPin(t, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) > 0 {
		t.Fatalf("crates/Cargo.toml [workspace.dependencies] drifted from DevCouncil rust/Cargo.toml; mirror DevCouncil's pins:\n  %s",
			strings.Join(diffs, "\n  "))
	}
}

// TestCratePinDriftIsReported is the red half: the same members, through the
// same symlinks, under a workspace file whose rusqlite pin differs only in its
// version. The check must name the member and the dependency.
func TestCratePinDriftIsReported(t *testing.T) {
	root := repoRoot(t)
	manviCrates := filepath.Join(root, "crates")
	sha, err := pinnedDevCouncilSHA(root)
	if err != nil {
		t.Fatal(err)
	}
	devcouncilRust := filepath.Join(root, "..", "DevCouncil", "rust")
	pinnedRust, err := exportPinnedRust(t, filepath.Join(root, "..", "DevCouncil"), sha)
	if err != nil {
		t.Fatal(err)
	}

	manifest := readFile(t, filepath.Join(manviCrates, "Cargo.toml"))
	const pin = `rusqlite = { version = "`
	start := strings.Index(manifest, pin)
	if start < 0 {
		t.Fatalf("crates/Cargo.toml has no %q pin to perturb", pin)
	}
	end := strings.Index(manifest[start+len(pin):], `"`)
	if end < 0 {
		t.Fatalf("crates/Cargo.toml rusqlite pin is not a quoted version")
	}
	drifted := manifest[:start+len(pin)] + "0.39.0" + manifest[start+len(pin)+end:]

	md, err := readCargoMetadata(filepath.Join(manviCrates, "Cargo.toml"))
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "Cargo.toml"), []byte(drifted), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range md.Packages {
		real, err := filepath.EvalSymlinks(filepath.Dir(p.ManifestPath))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(ws, filepath.Base(filepath.Dir(p.ManifestPath)))); err != nil {
			t.Fatal(err)
		}
	}

	diffs, err := compareCratePins(ws, devcouncilRust, pinnedRust)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diffs {
		if strings.HasPrefix(d, "dc-store: rusqlite ") && strings.Contains(d, `"^0.39.0"`) {
			return
		}
	}
	t.Fatalf("a rusqlite pin of 0.39.0 was not reported against dc-store; got:\n  %s", strings.Join(diffs, "\n  "))
}

// pinFixture is a Manvi root whose crates/dc-a links into a sibling DevCouncil
// git repository with two commits: the pin, where dc-a depends on sha2 0.10.9
// under MIT, and a later HEAD, where sha2 is 0.11 and the licence is
// Apache-2.0. The sibling's working tree is at HEAD; the pin is the first.
type pinFixture struct {
	root, sibling, pin, head string
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFixtureFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func workspaceManifest(sha2, license string) string {
	return "[workspace]\nresolver = \"3\"\nmembers = [\"dc-a\"]\n\n[workspace.package]\nversion = \"1.0.0\"\nedition = \"2021\"\nlicense = \"" + license + "\"\n\n[workspace.lints.rust]\nunsafe_code = \"forbid\"\n\n[workspace.dependencies]\nsha2 = \"" + sha2 + "\"\n"
}

const fixtureMember = "[package]\nname = \"dc-a\"\nversion.workspace = true\nedition.workspace = true\nlicense.workspace = true\n\n[dependencies]\nsha2.workspace = true\n"

func newPinFixture(t *testing.T) pinFixture {
	t.Helper()
	top := t.TempDir()
	f := pinFixture{root: filepath.Join(top, "Manvi"), sibling: filepath.Join(top, "DevCouncil")}
	writeFixtureFile(t, filepath.Join(f.sibling, "rust", "Cargo.toml"), workspaceManifest("0.10.9", "MIT"))
	writeFixtureFile(t, filepath.Join(f.sibling, "rust", "dc-a", "Cargo.toml"), fixtureMember)
	writeFixtureFile(t, filepath.Join(f.sibling, "rust", "dc-a", "src", "lib.rs"), "")
	gitIn(t, f.sibling, "init", "--quiet")
	gitIn(t, f.sibling, "add", "-A")
	gitIn(t, f.sibling, "commit", "--quiet", "-m", "pin")
	f.pin = gitIn(t, f.sibling, "rev-parse", "HEAD")
	writeFixtureFile(t, filepath.Join(f.sibling, "rust", "Cargo.toml"), workspaceManifest("0.11.0", "Apache-2.0"))
	gitIn(t, f.sibling, "commit", "--quiet", "-am", "newer")
	f.head = gitIn(t, f.sibling, "rev-parse", "HEAD")

	writeFixtureFile(t, filepath.Join(f.root, "scripts", "module-pins.txt"), "# pins\nDevCouncil\thttps://github.com/example/DevCouncil.git\t"+f.pin+"\n")
	if err := os.MkdirAll(filepath.Join(f.root, "crates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "DevCouncil", "rust", "dc-a"), filepath.Join(f.root, "crates", "dc-a")); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f pinFixture) writeCrates(t *testing.T, sha2, license string) {
	t.Helper()
	writeFixtureFile(t, filepath.Join(f.root, "crates", "Cargo.toml"), workspaceManifest(sha2, license))
}

// The sibling's HEAD has moved past the pin and crates/Cargo.toml matches the
// pin: the check must hold. Compared against the working tree, as it once was,
// this reports a sha2 and a licence drift that the release never builds.
func TestCratePinsComparePinnedCommitNotSiblingWorkingTree(t *testing.T) {
	f := newPinFixture(t)
	f.writeCrates(t, "0.10.9", "MIT")
	diffs, err := checkCratePinsAtPin(t, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 0 {
		t.Fatalf("crates/ matches the pin but the sibling HEAD (%s) was compared:\n  %s", f.head[:12], strings.Join(diffs, "\n  "))
	}
}

// crates/ matches the sibling HEAD and not the pin: the check must fail and
// name each drifted value.
func TestCratePinsReportDriftFromPinnedCommit(t *testing.T) {
	f := newPinFixture(t)
	f.writeCrates(t, "0.11.0", "Apache-2.0")
	diffs, err := checkCratePinsAtPin(t, f.root)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(diffs, "\n")
	for _, want := range []string{`dc-a: sha2 kind=normal target=any resolves to req="^0.11.0"`, `license "Apache-2.0" here but "MIT" in DevCouncil`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestCratePinsReportLintDrift(t *testing.T) {
	f := newPinFixture(t)
	f.writeCrates(t, "0.10.9", "MIT")
	manifest := readFile(t, filepath.Join(f.root, "crates", "Cargo.toml"))
	writeFixtureFile(t, filepath.Join(f.root, "crates", "Cargo.toml"), strings.Replace(manifest, `unsafe_code = "forbid"`, `unsafe_code = "warn"`, 1))
	diffs, err := checkCratePinsAtPin(t, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || !strings.Contains(diffs[0], "[workspace.lints]") {
		t.Fatalf("a weakened unsafe_code lint was not reported: %v", diffs)
	}
}

// A pin the sibling does not have is an error, never an empty diff list.
func TestCratePinsFailWhenPinnedCommitIsAbsent(t *testing.T) {
	f := newPinFixture(t)
	f.writeCrates(t, "0.10.9", "MIT")
	writeFixtureFile(t, filepath.Join(f.root, "scripts", "module-pins.txt"), "DevCouncil\thttps://github.com/example/DevCouncil.git\t"+strings.Repeat("a", 40)+"\n")
	diffs, err := checkCratePinsAtPin(t, f.root)
	if err == nil {
		t.Fatalf("an unreadable pin passed with diffs %v", diffs)
	}
	if !strings.Contains(err.Error(), "fetch-modules.sh") {
		t.Fatalf("error does not say how to fix it: %v", err)
	}
}

func TestPinnedDevCouncilSHARejectsMissingOrShortPin(t *testing.T) {
	for name, body := range map[string]string{
		"empty":     "",
		"short":     "DevCouncil\thttps://github.com/x/y.git\tabc123\n",
		"othername": "gusset\thttps://github.com/x/y.git\t" + strings.Repeat("b", 40) + "\n",
	} {
		root := t.TempDir()
		writeFixtureFile(t, filepath.Join(root, "scripts", "module-pins.txt"), body)
		if sha, err := pinnedDevCouncilSHA(root); err == nil {
			t.Errorf("%s: accepted %q", name, sha)
		}
	}
}
