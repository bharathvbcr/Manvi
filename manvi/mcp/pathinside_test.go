package mcp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// caseInsensitiveBase returns a resolved temp directory on a volume that treats
// names differing only in case as one, or skips: a case-variant spelling is
// only the same directory on such a volume, and on a case-sensitive one
// (case-sensitive APFS, most Linux file systems) it is a different, absent one.
// The volume is probed, not assumed from GOOS, because both kinds exist on
// darwin.
func caseInsensitiveBase(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(base, "caseprobe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "CASEPROBE")); err != nil {
		t.Skipf("%s volume at %s is case-sensitive; a case-variant spelling names another path", runtime.GOOS, base)
	}
	return base
}

func mustInside(t *testing.T, root, path string, want bool) {
	t.Helper()
	got, err := PathIsInside(root, path)
	if err != nil {
		t.Fatalf("PathIsInside(%q, %q): %v", root, path, err)
	}
	if got != want {
		t.Errorf("PathIsInside(%q, %q) = %v, want %v", root, path, got, want)
	}
}

// A path inside the repository spelled with different case is still inside it
// on a case-insensitive volume, whether or not the file exists yet.
func TestPathIsInsideCaseVariantSpelling(t *testing.T) {
	base := caseInsensitiveBase(t)
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(repo, "sub", "evil.yaml")
	if err := os.WriteFile(existing, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	upper := filepath.Join(base, "REPO")

	mustInside(t, repo, filepath.Join(upper, "sub", "evil.yaml"), true)
	mustInside(t, repo, filepath.Join(upper, "sub", "EVIL.YAML"), true)
	mustInside(t, repo, filepath.Join(upper, "sub", "not-yet", "new.yaml"), true)
	mustInside(t, upper, filepath.Join(repo, "sub", "evil.yaml"), true)
	mustInside(t, filepath.Join(upper, "SUB"), filepath.Join(repo, "sub", "evil.yaml"), true)
	mustInside(t, repo, upper, true)
}

// Prefix tricks and siblings stay outside, identity or not.
func TestPathIsInsideSiblingsAndPrefixTricks(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "repo")
	for _, d := range []string{"repo", "repo2", "repo-evil", "other"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustInside(t, repo, filepath.Join(base, "repo2", "f"), false)
	mustInside(t, repo, filepath.Join(base, "repo-evil", "f"), false)
	mustInside(t, repo, filepath.Join(base, "other", "f"), false)
	mustInside(t, repo, filepath.Join(base, "REPO2", "f"), false)
	mustInside(t, repo, filepath.Join(base, "repo", "f"), true)
	mustInside(t, repo, filepath.Join(base, "repo", "missing", "f"), true)
}

// Symlinks are followed before the comparison: a link into the root is inside,
// a link out of it is outside.
func TestPathIsInsideFollowsSymlinks(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "repo")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{repo, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	intoRepo := filepath.Join(outside, "link-in")
	outOfRepo := filepath.Join(repo, "link-out")
	if err := os.Symlink(repo, intoRepo); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, outOfRepo); err != nil {
		t.Fatal(err)
	}
	mustInside(t, repo, filepath.Join(intoRepo, "f"), true)
	mustInside(t, repo, filepath.Join(outOfRepo, "f"), false)
}

// A candidate that cannot be examined is an error, never "outside". The walk up
// to an existing ancestor must not read a permission failure as absence.
func TestPathIsInsideUnreadableAncestorIsAnError(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not deny this user")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "repo")
	locked := filepath.Join(base, "locked")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	if inside, err := PathIsInside(repo, filepath.Join(locked, "inner", "f")); err == nil {
		t.Fatalf("an unreadable ancestor was answered %v with no error", inside)
	}
}

// The MCP trust file of the same shape as the user-config case: a trust file
// reached through a case-variant spelling of the repository is inside it, so
// it is refused and a repository cannot authorize itself that way.
func TestATrustFileThroughACaseVariantOfTheRepositoryIsNotHonoured(t *testing.T) {
	base := caseInsensitiveBase(t)
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(base, "REPO", "mcp-trust.json")
	t.Setenv(TrustFileEnv, inside)
	t.Setenv(TrustListEnv, "")

	cfg := stubConfig(t, "selfsigned", "", "")
	writeRootConfig(t, root, "mcp.json", "selfsigned", cfg)

	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeTrust(t, inside, Fingerprint(mustConfig(t, m, "selfsigned")))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := m.Client(ctx, "selfsigned")
	if err == nil {
		t.Fatal("a repository authorized its own MCP server through a case-variant spelling of itself")
	}
	if !strings.Contains(err.Error(), "inside the repository") {
		t.Errorf("the refusal does not say why the file was ignored: %v", err)
	}
}

func BenchmarkPathIsInside(b *testing.B) {
	base, err := filepath.EvalSymlinks(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	repo := filepath.Join(base, "repo")
	deep := filepath.Join(repo, "a", "b", "c")
	other := filepath.Join(base, "elsewhere", "a", "b", "c")
	for _, d := range []string{deep, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			b.Fatal(err)
		}
	}
	cases := []struct{ name, path string }{
		{"inside", filepath.Join(deep, "f.yaml")},
		{"outside", filepath.Join(other, "f.yaml")},
		{"case-variant", filepath.Join(base, "REPO", "a", "b", "c", "f.yaml")},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := PathIsInside(repo, c.path); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
