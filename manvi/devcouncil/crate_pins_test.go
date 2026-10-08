package devcouncil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	cmd := exec.Command(cargo, "metadata", "--manifest-path", manifest, "--no-deps", "--format-version", "1", "--offline")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return cargoMetadata{}, fmt.Errorf("start cargo metadata for %s: %w", manifest, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return cargoMetadata{}, fmt.Errorf("cargo metadata for %s: %w\n%s", manifest, err, stderr.String())
		}
	case <-time.After(cargoMetadataTimeout):
		_ = cmd.Process.Kill()
		<-done
		return cargoMetadata{}, fmt.Errorf("cargo metadata for %s did not finish within %s", manifest, cargoMetadataTimeout)
	}
	var md cargoMetadata
	if err := json.Unmarshal(stdout.Bytes(), &md); err != nil {
		return cargoMetadata{}, fmt.Errorf("decode cargo metadata for %s: %w", manifest, err)
	}
	return md, nil
}

// compareCratePins checks that every member of the workspace at manviCrates is
// a DevCouncil crate reached through a symlink, and that each one resolves the
// same dependency specs here as in DevCouncil's own workspace at
// devcouncilRust. It returns every difference, not the first.
func compareCratePins(manviCrates, devcouncilRust string) ([]string, error) {
	devcouncilReal, err := filepath.EvalSymlinks(devcouncilRust)
	if err != nil {
		return nil, fmt.Errorf("DevCouncil rust/ is not at %s (CI places it with scripts/fetch-modules.sh): %w", devcouncilRust, err)
	}
	manvi, err := readCargoMetadata(filepath.Join(manviCrates, "Cargo.toml"))
	if err != nil {
		return nil, err
	}
	upstream, err := readCargoMetadata(filepath.Join(devcouncilReal, "Cargo.toml"))
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
		if !strings.HasPrefix(real+string(filepath.Separator), devcouncilReal+string(filepath.Separator)) {
			diffs = append(diffs, fmt.Sprintf("%s: %s is not a symlink into DevCouncil rust/ (%s); the dc-* sources have one owner", p.Name, real, devcouncilReal))
			continue
		}
		up, ok := byName[p.Name]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("%s: not a member of DevCouncil's workspace at %s", p.Name, devcouncilReal))
			continue
		}
		diffs = append(diffs, compareDeps(p.Name, p.Dependencies, up.Dependencies)...)
	}
	return diffs, nil
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
	diffs, err := compareCratePins(filepath.Join(root, "crates"), filepath.Join(root, "..", "DevCouncil", "rust"))
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
	devcouncilRust := filepath.Join(root, "..", "DevCouncil", "rust")

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

	diffs, err := compareCratePins(ws, devcouncilRust)
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
