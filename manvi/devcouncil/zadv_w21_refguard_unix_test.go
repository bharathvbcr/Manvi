//go:build unix

package devcouncil

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// canDenyRead reports whether chmod 000 stops this process from opening p (it
// does not when the tests run as root).
func canDenyRead(p string) bool {
	f, err := os.Open(p) // #nosec G304 -- a file this test made.
	if err == nil {
		_ = f.Close()
		return false
	}
	return true
}

func denied(t *testing.T, p string) {
	t.Helper()
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o700) }) // #nosec G302 -- restores a file this test made so the temp dir can be removed.
}

// A ref-shaped file git dir that cannot be opened refuses the command before it
// runs, never skips it.
func TestAdvW21UnreadableRefSourceRefusesBeforeTheCommandRuns(t *testing.T) {
	for _, kind := range []string{"file", "dir", "file_in_subdir"} {
		t.Run(kind, func(t *testing.T) {
			f := shellCommitFixture(t, false)
			common := gitCommon(t, f.root)
			target := filepath.Join(common, "LOCKED_HEAD")
			switch kind {
			case "file":
				if err := os.WriteFile(target, []byte(headOf(t, f.root)+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "dir":
				target = filepath.Join(common, "lockeddir")
				if err := os.MkdirAll(target, 0o750); err != nil {
					t.Fatal(err)
				}
			case "file_in_subdir":
				sub := filepath.Join(common, "sub")
				if err := os.MkdirAll(sub, 0o750); err != nil {
					t.Fatal(err)
				}
				target = filepath.Join(sub, "x")
				if err := os.WriteFile(target, []byte(headOf(t, f.root)+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			denied(t, target)
			if !canDenyRead(target) {
				t.Log("running as a user that ignores file modes; the denial cannot be staged")
				return
			}
			res := shell(f, "echo should-not-run > ran.txt")
			if !res.IsError || !strings.Contains(res.Text, "ref snapshot") {
				t.Fatalf("a command ran beside an unreadable ref source: %+v", res)
			}
			if _, err := os.Stat(filepath.Join(f.root, "ran.txt")); err == nil {
				t.Fatal("the command ran")
			}
		})
	}
}

// A command that parks a leak in a file and makes it unreadable is blocked, and
// no later command runs beside it.
func TestAdvW21CommandMadeUnreadableLeakIsBlocked(t *testing.T) {
	f := shellCommitFixture(t, true)
	hid := filepath.Join(gitCommon(t, f.root), "HID_HEAD")
	t.Cleanup(func() { _ = os.Chmod(hid, 0o600) })
	write(t, f.root, "park.sh", parkLeak+"echo $c > $d/HID_HEAD\nchmod 000 $d/HID_HEAD\n")
	res := shell(f, "sh park.sh")
	if !canDenyRead(hid) {
		t.Log("running as a user that ignores file modes; the denial cannot be staged")
		return
	}
	requireStopped(t, f, res, "HID_HEAD")
}

// FIFOs, sockets and directories named like pseudo-refs are not read, and do not
// hang the walk (a FIFO opened for reading blocks until a writer comes).
func TestAdvW21NonRegularFilesNamedLikeRefsDoNotHangTheGuard(t *testing.T) {
	f := shellCommitFixture(t, false)
	common := gitCommon(t, f.root)
	outside := filepath.Join(t.TempDir(), "outside.fifo")
	for _, p := range []string{filepath.Join(common, "FIFO_HEAD"), outside} {
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(common, "LINK_TO_FIFO")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(common, "nowhere"), filepath.Join(common, "DANGLING_HEAD")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(common, "ORIG_HEAD"), 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, f.root, "mk.sh", "mkfifo \"$(git rev-parse --git-common-dir)/NEW_FIFO\"\necho fine\n")
	res := shell(f, "sh mk.sh")
	if res.Blocked || !strings.Contains(res.Text, "fine") {
		t.Fatalf("a FIFO or directory named like a ref changed the verdict: %+v", res)
	}
}
