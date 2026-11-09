//go:build unix

package codingagent

import (
	"os"
	"path/filepath"
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
		{"world-writable", 0o777, homeWorldWrtable},
		{"world-writable sticky (/tmp style)", 0o1777, homeWorldWrtable},
		{"group-writable", 0o775, homeWorldWrtable},
		{"group-writable only", 0o770, homeWorldWrtable},
		{"other-writable only", 0o702, homeWorldWrtable},
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
	if err == nil || !strings.Contains(err.Error(), homeWorldWrtable) {
		t.Fatalf("pinned checkout under a world-writable HOME: want %q, got %v", homeWorldWrtable, err)
	}
	if strings.Contains(err.Error(), home) {
		t.Fatalf("the refusal names the HOME path: %v", err)
	}
}
