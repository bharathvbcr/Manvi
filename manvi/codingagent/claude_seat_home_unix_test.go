//go:build unix

package codingagent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestClaudeSeatHomeOwnershipAndWritability(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode os.FileMode
		want string
	}{
		{"owned and private", 0o700, ""},
		{"owned and group-readable", 0o755, ""},
		{"world-writable", 0o777, homeWorldWritable},
		{"world-writable sticky (/tmp style)", 0o1777, homeWorldWritable},
		{"group-writable", 0o775, homeWorldWritable},
		{"group-writable only", 0o770, homeWorldWritable},
		{"other-writable only", 0o702, homeWorldWritable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "home")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, tc.mode); err != nil {
				t.Fatal(err)
			}
			_, problem := resolveClaudeHome(dir)
			if problem != tc.want {
				t.Fatalf("mode %o: problem = %q, want %q", tc.mode, problem, tc.want)
			}
			if usableClaudeHome(dir) != (tc.want == "") {
				t.Fatalf("mode %o: usableClaudeHome disagrees with resolveClaudeHome", tc.mode)
			}
		})
	}
}

func TestClaudeSeatHomeOwnedByAnotherUserIsRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: every directory is 'owned' by a different uid only for non-root callers")
	}
	for _, dir := range []string{"/usr", "/Users", "/home"} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		t.Run(dir, func(t *testing.T) {
			if _, problem := resolveClaudeHome(dir); problem == "" {
				t.Fatalf("%s is usable as HOME", dir)
			}
		})
	}
	if _, problem := resolveClaudeHome("/usr"); problem != homeNotOwned {
		if _, err := os.Stat("/usr"); err == nil {
			t.Fatalf("/usr: problem = %q, want %q", problem, homeNotOwned)
		}
	}
}

func TestClaudeSeatUnsafeHomeRefusesPinnedAndLeavesUnpinnedUnrouted(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o777); err != nil { // #nosec G302 -- the fixture needs a world-writable HOME
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	unpinned := t.TempDir()
	ov, err := claudeSeatOverrides(unpinned)
	if err != nil || len(ov.set) != 0 || len(ov.unsetKeys) != 2 {
		t.Fatalf("unpinned checkout under an unsafe HOME: want the unrouted launch, got %+v, %v", ov, err)
	}

	pinned := t.TempDir()
	writeSeatTestFile(t, filepath.Join(pinned, ".claude-account"), "chandra\n")
	_, err = claudeSeatOverrides(pinned)
	if err == nil || !strings.Contains(err.Error(), homeWorldWritable) {
		t.Fatalf("pinned checkout under a world-writable HOME: want %q, got %v", homeWorldWritable, err)
	}
	if strings.Contains(err.Error(), home) {
		t.Fatalf("the refusal names the HOME path: %v", err)
	}
}

// unsafeSeatHome makes a HOME with the given mode and points the process at it.
func unsafeSeatHome(t *testing.T, mode os.FileMode) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "h")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, mode); err != nil { // #nosec G302 -- the fixture needs an unsafe or a plain HOME
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// A marker in a folder between the checkout and HOME is a supported placement.
// An unusable HOME must not shrink the search to the checkout and its .git.
func TestClaudeSeatUnsafeHomeStillFindsMarkerBetweenCheckoutAndHome(t *testing.T) {
	home := unsafeSeatHome(t, 0o775)
	writeSeatTestFile(t, filepath.Join(home, "work", ".claude-account"), "chandra\n")
	repo := filepath.Join(home, "work", "repo")
	writeSeatTestFile(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
	sub := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{repo, sub} {
		_, err := claudeSeatOverrides(cwd)
		if err == nil || !strings.Contains(err.Error(), homeWorldWritable) {
			t.Fatalf("cwd %s: marker between checkout and an unsafe HOME: want a refusal naming %q, got %v", cwd, homeWorldWritable, err)
		}
	}
}

// With no HOME path at all there is no bound, so the search runs to the root.
func TestClaudeSeatNoHomePathSearchesToFilesystemRoot(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	base := t.TempDir()
	writeSeatTestFile(t, filepath.Join(base, "work", ".claude-account"), "chandra\n")
	cwd := filepath.Join(base, "work", "a", "b")
	if err := os.MkdirAll(cwd, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := claudeSeatOverrides(cwd); err == nil || !strings.Contains(err.Error(), homeUnusable) {
		t.Fatalf("marker above a checkout with no HOME: want a refusal naming %q, got %v", homeUnusable, err)
	}
}

// The main checkout of a linked worktree is searched the same way: a marker in
// a folder above the main checkout still pins the worktree.
func TestClaudeSeatUnsafeHomeLinkedWorktreeFindsMarkerAboveMainCheckout(t *testing.T) {
	home := unsafeSeatHome(t, 0o775)
	writeSeatTestFile(t, filepath.Join(home, "Code", ".claude-account"), "chandra\n")
	mainRepo := filepath.Join(home, "Code", "MainRepo")
	adminDir := filepath.Join(mainRepo, ".git", "worktrees", "wt")
	writeSeatTestFile(t, filepath.Join(adminDir, "commondir"), "../..\n")
	worktree := filepath.Join(home, "Worktrees", "wt")
	writeSeatTestFile(t, filepath.Join(worktree, ".git"), "gitdir: "+adminDir+"\n")
	writeSeatTestFile(t, filepath.Join(adminDir, "gitdir"), filepath.Join(worktree, ".git")+"\n")
	sub := filepath.Join(worktree, "svc")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := claudeSeatOverrides(sub); err == nil || !strings.Contains(err.Error(), homeWorldWritable) {
		t.Fatalf("linked worktree whose main checkout sits under a pin, unsafe HOME: want a refusal naming %q, got %v", homeWorldWritable, err)
	}
}

// No pin anywhere: the unrouted launch, dropping only the nested-session
// markers, which is what main did on this path. A ~/.claude-accounts entry
// under the unsafe HOME is refused, because main would have checked the default
// login against its seats and that cannot be done from an untrusted directory.
func TestClaudeSeatUnsafeHomeUnpinnedKeepsMainEnvHygiene(t *testing.T) {
	home := unsafeSeatHome(t, 0o775)
	cwd := filepath.Join(home, "work", "repo")
	writeSeatTestFile(t, filepath.Join(cwd, ".git", "HEAD"), "ref: refs/heads/main\n")

	ov, err := claudeSeatOverrides(cwd)
	if err != nil || len(ov.set) != 0 || !slices.Equal(ov.unsetKeys, []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT"}) {
		t.Fatalf("unpinned, no seats, unsafe HOME: want only the nested-session markers dropped, got %+v, %v", ov, err)
	}

	writeSeatTestFile(t, filepath.Join(home, ".claude-accounts"), "chandra "+filepath.Join(home, ".claude-chandra")+"\n")
	if ov, err = claudeSeatOverrides(cwd); err == nil || !strings.Contains(err.Error(), homeWorldWritable) {
		t.Fatalf("unpinned, ~/.claude-accounts under an unsafe HOME: want a refusal naming %q, got %+v, %v", homeWorldWritable, ov, err)
	}
}

// A usable HOME is unchanged: the same marker placement routes to its seat.
func TestClaudeSeatUsableHomeMarkerBetweenCheckoutAndHomeRoutes(t *testing.T) {
	home := unsafeSeatHome(t, 0o755)
	seat := filepath.Join(home, ".claude-chandra")
	if err := os.MkdirAll(seat, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSeatTestFile(t, filepath.Join(home, ".claude-accounts"), "chandra "+seat+"\n")
	writeSeatTestFile(t, filepath.Join(home, "work", ".claude-account"), "chandra\n")
	repo := filepath.Join(home, "work", "repo")
	writeSeatTestFile(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
	resolvedSeat, err := filepath.EvalSymlinks(seat)
	if err != nil {
		t.Fatal(err)
	}
	ov, err := claudeSeatOverrides(repo)
	if err != nil || !slices.Contains(ov.set, "CLAUDE_CONFIG_DIR="+resolvedSeat) {
		t.Fatalf("usable HOME: want a launch routed to the seat, got %+v, %v", ov, err)
	}
}
