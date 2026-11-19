//go:build unix

package main

// Adversarial tests for the w23 containment change (lane w23-containment-adv).
// A test whose name ends in Defect documents a real defect and is expected to
// FAIL on the w23-containment tip.

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/artifacts"
)

func zadvTree(t *testing.T) (root, outside string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "repo")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.Mkdir(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	return root, outside
}

// Every spelling of a legitimate relative or existing absolute state directory
// is accepted by the start-up check and then usable by the store, including when
// the project root is reached through a link (a temporary directory on macOS).
func TestZadvLegitimateStateDirSpellingsAreNotRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state func(root string) string
		via   bool
	}{
		{"default", func(string) string { return "" }, false},
		{"dot", func(string) string { return "." }, false},
		{"dot-slash", func(string) string { return "./" }, false},
		{"trailing-slash", func(string) string { return "state/" }, false},
		{"relative", func(string) string { return "state" }, false},
		{"nested", func(string) string { return "a/b/c" }, false},
		{"cleaned-parent", func(string) string { return "a/../state" }, false},
		{"absolute-existing", func(root string) string { return filepath.Join(filepath.Dir(root), "abs-state") }, false},
		{"absolute-existing-trailing-slash", func(root string) string { return filepath.Join(filepath.Dir(root), "abs-state") + "/" }, false},
		{"default-via-link", func(string) string { return "" }, true},
		{"relative-via-link", func(string) string { return "state" }, true},
		{"dot-via-link", func(string) string { return "." }, true},
		{"absolute-existing-via-link", func(root string) string { return filepath.Join(filepath.Dir(root), "abs-state") }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := zadvTree(t)
			if err := os.MkdirAll(filepath.Join(filepath.Dir(root), "abs-state"), 0o750); err != nil {
				t.Fatal(err)
			}
			projectRoot := root
			if tc.via {
				projectRoot = filepath.Join(t.TempDir(), "via")
				if err := os.Symlink(root, projectRoot); err != nil {
					t.Skip(err)
				}
			}
			defer setProjectRootForTest(projectRoot)()
			t.Setenv("MANVI_STATE_DIR", tc.state(root))
			if err := verifyStateDir(); err != nil {
				t.Fatalf("verifyStateDir refused a legitimate layout: %v", err)
			}
			store, err := newArtifactStore()
			if err != nil {
				t.Fatalf("newArtifactStore: %v", err)
			}
			if _, err := store.Create("probe.md", "x", artifacts.Metadata{}); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if got, err := store.Get("probe.md"); err != nil || got.Content != "x" {
				t.Fatalf("Get = %v, %v", got, err)
			}
		})
	}
}

// An absolute MANVI_STATE_DIR that does not exist yet (a fresh machine, or
// harness init switched off so the scaffolding never made it). The previous
// store created the missing directories on first write; the anchor now has to
// exist, so the first artifact write is refused with ENOENT. Defect (regression).
func TestZadvAbsoluteStateDirNotYetCreatedStillWorksDefect(t *testing.T) {
	root, _ := zadvTree(t)
	defer setProjectRootForTest(root)()
	t.Setenv("MANVI_STATE_DIR", filepath.Join(t.TempDir(), "not-yet", "state"))
	store, err := newArtifactStore()
	if err != nil {
		t.Fatalf("newArtifactStore: %v", err)
	}
	if _, err := store.Create("probe.md", "x", artifacts.Metadata{}); err != nil {
		t.Fatalf("Create under a state directory that does not exist yet: %v", err)
	}
}

// Ways of climbing out are refused, and nothing is created outside.
func TestZadvStateDirClimbsAreRefused(t *testing.T) {
	root, outside := zadvTree(t)
	defer setProjectRootForTest(root)()
	for _, state := range []string{"..", "../outside", "a/../../outside", "../repo/../outside"} {
		t.Setenv("MANVI_STATE_DIR", state)
		if err := verifyStateDir(); err == nil {
			t.Errorf("verifyStateDir(%q) = nil, want a refusal", state)
		}
	}
	if es, _ := os.ReadDir(outside); len(es) != 0 {
		t.Fatalf("outside was touched: %v", es)
	}
}

// With an absolute MANVI_STATE_DIR the store must live there and never fall back
// to the default location below the project root, even when that default is a
// committed link out of the tree.
func TestZadvAbsoluteStateDirNeverFallsBackToTheDefault(t *testing.T) {
	root, outside := zadvTree(t)
	defer setProjectRootForTest(root)()
	if err := os.Symlink(outside, filepath.Join(root, ".devcouncil")); err != nil {
		t.Skip(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Mkdir(elsewhere, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MANVI_STATE_DIR", elsewhere)
	store, err := newArtifactStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("a.md", "x", artifacts.Metadata{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "artifacts", "a.md")); err != nil {
		t.Fatalf("the artifact is not under MANVI_STATE_DIR: %v", err)
	}
	if es, _ := os.ReadDir(outside); len(es) != 0 {
		t.Fatalf("the default location's link target was written: %v", es)
	}
}

// DEVCOUNCIL_ROOT names the project root. A root reached through a link is a
// legitimate layout; a .devcouncil link out of it is refused, and the refusal
// follows the env root, not the working directory.
func TestZadvDevcouncilRootInterplay(t *testing.T) {
	root, outside := zadvTree(t)
	via := filepath.Join(t.TempDir(), "via")
	if err := os.Symlink(root, via); err != nil {
		t.Skip(err)
	}
	t.Setenv("MANVI_PROJECT_ROOT", "")
	t.Setenv("MANVI_ROOT", "")
	t.Setenv("MANVI_STATE_DIR", "")
	t.Setenv("DEVCOUNCIL_ROOT", via)
	if err := verifyStateDir(); err != nil {
		t.Fatalf("a DEVCOUNCIL_ROOT reached through a link was refused: %v", err)
	}
	store, err := newArtifactStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("a.md", "x", artifacts.Metadata{}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, ".devcouncil")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".devcouncil")); err != nil {
		t.Fatal(err)
	}
	if err := verifyStateDir(); !errors.Is(err, errStateDirOutside) {
		t.Fatalf("verifyStateDir with a linked .devcouncil under DEVCOUNCIL_ROOT = %v", err)
	}
}

// A state directory that is a link to another place INSIDE the project: the
// start-up check passes it, so the store must either work or the check must say
// so; today the check passes and every artifact write is then refused later.
func TestZadvInTreeStateDirLinkIsJudgedTheSameByCheckAndStoreDefect(t *testing.T) {
	root, _ := zadvTree(t)
	defer setProjectRootForTest(root)()
	t.Setenv("MANVI_STATE_DIR", "")
	if err := os.MkdirAll(filepath.Join(root, "tools", "state"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("tools", "state"), filepath.Join(root, ".devcouncil")); err != nil {
		t.Skip(err)
	}
	checkErr := verifyStateDir()
	store, err := newArtifactStore()
	var createErr error
	if err == nil {
		_, createErr = store.Create("a.md", "x", artifacts.Metadata{})
	} else {
		createErr = err
	}
	if (checkErr == nil) != (createErr == nil) {
		t.Fatalf("start-up check says %v but the store says %v", checkErr, createErr)
	}
}

// Commands outside preparesRepository still use the state directory. With the
// default state directory a committed link out of the tree, none of them may
// create anything outside.
func TestZadvNonPreparingCommandsDoNotWriteThroughALinkedStateDir(t *testing.T) {
	for _, args := range [][]string{{"budget"}, {"tools"}, {"check"}, {"providers"}, {"agents"}, {"tiers"}, {"flags"}, {"lease", "status"}, {"map", "status"}} {
		root, outside := zadvTree(t)
		restore := setProjectRootForTest(root)
		t.Setenv("MANVI_STATE_DIR", "")
		if err := os.Symlink(outside, filepath.Join(root, ".devcouncil")); err != nil {
			restore()
			t.Skip(err)
		}
		_ = run(io.Discard, io.Discard, args)
		restore()
		if es, _ := os.ReadDir(outside); len(es) != 0 {
			names := make([]string, 0, len(es))
			for _, e := range es {
				names = append(names, e.Name())
			}
			t.Errorf("manvi %v created %v through the linked state directory", args, names)
		}
	}
}
