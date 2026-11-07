package mcp

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Adversarial tests for PathIsInside after the w9-pathcase change (case-variant
// spelling resolved by os.SameFile on the nearest existing ancestor).

func advBase(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return base
}

// A ".." that follows a symlink component names the symlink target's parent,
// but resolveExisting runs filepath.Abs (a lexical Clean) before it resolves
// anything, so "out/link/../cfg.yaml" is collapsed to "out/cfg.yaml" and read as
// outside while the kernel opens repo/cfg.yaml. Not touched by the w9 change
// (same on w9-base), but it is a false "outside" for a path that is inside. The
// two in-tree callers Clean their path first (trustFilePath, userConfigPath), so
// only a direct caller of the exported function is exposed.
func TestZAdvW9DotDotAfterSymlinkIsResolvedByTheKernelNotLexically(t *testing.T) {
	base := advBase(t)
	repo := filepath.Join(base, "repo")
	out := filepath.Join(base, "out")
	for _, d := range []string{filepath.Join(repo, "sub"), out} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(repo, "cfg.yaml")
	if err := os.WriteFile(cfg, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repo, "sub"), filepath.Join(out, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	candidate := out + string(filepath.Separator) + "link" + string(filepath.Separator) + ".." + string(filepath.Separator) + "cfg.yaml"
	if _, err := os.ReadFile(candidate); err != nil { // #nosec G304 -- a file under this test's own t.TempDir
		t.Fatalf("precondition: the kernel does not open the repository file through %q: %v", candidate, err)
	}
	mustInside(t, repo, candidate, true)
}

// A dangling symlink outside the repository whose target is a not-yet-existing
// path inside it: EvalSymlinks fails on it, resolveExisting reads that as "does
// not exist", drops the link's name and compares its parent. The path is inside
// the moment the target is created.
func TestZAdvW9DanglingSymlinkIntoRepoIsInside(t *testing.T) {
	base := advBase(t)
	repo := filepath.Join(base, "repo")
	out := filepath.Join(base, "out")
	for _, d := range []string{repo, out} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(out, "cfg.yaml")
	if err := os.Symlink(filepath.Join(repo, "later.yaml"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	mustInside(t, repo, link, true)
}

// macOS firmlinks: /System/Volumes/Data/Users/... and /Users/... are one tree
// that EvalSymlinks does not unify. The ancestor walk should see them as one.
func TestZAdvW9FirmlinkSpellingIsInside(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("firmlinks are a darwin feature")
	}
	base := advBase(t)
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	data := "/System/Volumes/Data"
	if _, err := os.Stat(data + base); err != nil {
		t.Skipf("no firmlink spelling of %s: %v", base, err)
	}
	mustInside(t, repo, data+filepath.Join(base, "repo", "sub", "f.yaml"), true)
	mustInside(t, repo, data+filepath.Join(base, "repo", "sub", "gone", "f.yaml"), true)
	mustInside(t, data+repo, filepath.Join(repo, "sub", "f.yaml"), true)
	mustInside(t, repo, data+filepath.Join(base, "elsewhere", "f.yaml"), false)
}

// Normalization: é as U+00E9 versus e+U+0301. APFS is normalization-insensitive,
// so these name one directory. Probed at runtime.
func TestZAdvW9UnicodeNormalizationVariantIsInside(t *testing.T) {
	base := advBase(t)
	nfc := "café"
	nfd := "café"
	repo := filepath.Join(base, nfc)
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, nfd)); err != nil {
		t.Skipf("volume at %s distinguishes NFC from NFD spellings", base)
	}
	mustInside(t, repo, filepath.Join(base, nfd, "sub", "f.yaml"), true)
	mustInside(t, repo, filepath.Join(base, nfd, "sub", "new", "f.yaml"), true)
	mustInside(t, filepath.Join(base, nfd), filepath.Join(repo, "sub", "f.yaml"), true)
	// A sibling that only shares the prefix, in the other normalization form.
	if err := os.MkdirAll(filepath.Join(base, nfc+"2"), 0o750); err != nil {
		t.Fatal(err)
	}
	mustInside(t, repo, filepath.Join(base, nfd+"2", "f.yaml"), false)
	mustInside(t, repo, filepath.Join(base, nfd+"-evil", "f.yaml"), false)
}

// A case-variant spelling of a symlink in the ancestry, for a file that does not
// exist yet, and a symlink whose target is spelled with other case than the root.
func TestZAdvW9CaseVariantAcrossSymlinks(t *testing.T) {
	base := caseInsensitiveBase(t)
	repo := filepath.Join(base, "repo")
	out := filepath.Join(base, "out")
	for _, d := range []string{filepath.Join(repo, "sub"), out} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	// out/link -> REPO (target spelled with the other case).
	if err := os.Symlink(filepath.Join(base, "REPO"), filepath.Join(out, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	mustInside(t, repo, filepath.Join(out, "link", "sub", "f.yaml"), true)
	mustInside(t, repo, filepath.Join(out, "LINK", "sub", "new", "f.yaml"), true)
	mustInside(t, repo, filepath.Join(base, "OUT", "link", "SUB", "new", "f.yaml"), true)

	// A link inside the repo that leads out stays outside however it is cased.
	if err := os.Symlink(out, filepath.Join(repo, "leave")); err != nil {
		t.Fatal(err)
	}
	mustInside(t, repo, filepath.Join(base, "REPO", "LEAVE", "other", "f.yaml"), false)
}

// Paths the file system cannot take must not be answered "outside": an error or
// "inside" is fine, (false, nil) is the original bug class.
func TestZAdvW9PathologicalPathsAreNeverOutside(t *testing.T) {
	base := advBase(t)
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "afile"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"nul":              filepath.Join(repo, "a\x00b", "f.yaml"),
		"path-max":         filepath.Join(repo, strings.Repeat("d/", 600), "f.yaml"),
		"name-max":         filepath.Join(repo, strings.Repeat("n", 300), "f.yaml"),
		"through-a-file":   filepath.Join(repo, "afile", "child", "f.yaml"),
		"trailing-slash":   repo + "/sub/",
		"dot-components":   repo + "/./sub/./f.yaml",
		"dotdot-roundtrip": filepath.Join(base, "REPO") + "/../repo/sub/f.yaml",
	}
	for name, p := range cases {
		inside, err := PathIsInside(repo, p)
		if err == nil && !inside {
			t.Errorf("%s: PathIsInside(%q) = false with no error for a path in the repository", name, p)
		}
	}
}

// Root swapped under the walk: an alias symlink is atomically replaced between
// two spellings of the same repository. At both ends the path is inside, so the
// answer must always be (true, nil).
func TestZAdvW9AtomicAliasSwapNeverReadsOutside(t *testing.T) {
	base := advBase(t)
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	targets := []string{repo, filepath.Join(base, "REPO"), filepath.Join(repo, ".")}
	if err := os.Symlink(targets[0], alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tmp := filepath.Join(base, "alias.tmp")
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Remove(tmp)
			if os.Symlink(targets[i%len(targets)], tmp) == nil {
				_ = os.Rename(tmp, alias)
			}
		}
	}()
	candidate := filepath.Join(alias, "sub", "new", "f.yaml")
	deadline := time.Now().Add(3 * time.Second)
	bad := 0
	var first string
	for time.Now().Before(deadline) {
		inside, err := PathIsInside(repo, candidate)
		if err != nil || !inside {
			bad++
			if first == "" {
				first = "inside=" + map[bool]string{true: "true", false: "false"}[inside] + " err=" + errString(err)
			}
		}
	}
	close(stop)
	wg.Wait()
	if bad > 0 {
		t.Errorf("%d answers other than (true, nil) for a path inside at both ends; first: %s", bad, first)
	}
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// A deleted root, and an unrelated path that shares nothing with it, stay
// outside without an error that would brick a startup.
func TestZAdvW9AbsentRootIsOutsideNotAnError(t *testing.T) {
	base := advBase(t)
	repo := filepath.Join(base, "gone-repo")
	other := filepath.Join(base, "cfg", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(other), 0o750); err != nil {
		t.Fatal(err)
	}
	mustInside(t, repo, other, false)
	mustInside(t, repo, filepath.Join(base, "GONE-REPO", "x"), false)
}

// A root that is a prefix-alias of an unrelated config directory: the config
// directory sibling of the repo must not read as inside however it is cased.
func TestZAdvW9CaseVariantSiblingPrefixesStayOutside(t *testing.T) {
	base := caseInsensitiveBase(t)
	repo := filepath.Join(base, "repo")
	for _, d := range []string{"repo", "repo2", "repo-evil", "REPO3"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"REPO2", "Repo-Evil", "repo3"} {
		mustInside(t, repo, filepath.Join(base, d, "f.yaml"), false)
		mustInside(t, repo, filepath.Join(base, d, "new", "f.yaml"), false)
	}
	// The parent of the repo is not inside it.
	mustInside(t, repo, base, false)
	mustInside(t, repo, strings.ToUpper(base), false)
}
