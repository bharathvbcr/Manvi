//go:build unix

package devcouncil

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// RLIMIT_FSIZE is how this test makes a write fail part-way, and it exists
// only on unix.
// TestAWriteThatFailsPartWayLeavesTheOriginal is the ENOSPC/EFBIG case: the
// subprocess lowers RLIMIT_FSIZE below the replacement's size, so the write
// fails after some bytes have gone out.
func TestAWriteThatFailsPartWayLeavesTheOriginal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("RLIMIT_FSIZE is not available on windows")
	}
	if dir := os.Getenv("MANVI_FSIZE_CHILD_DIR"); dir != "" {
		// Only the soft limit is lowered, and only around the write: a coverage
		// build writes its data when the child exits, and under the limit that
		// failed the run for a reason that has nothing to do with the write
		// under test.
		var orig syscall.Rlimit
		if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &orig); err != nil {
			t.Fatalf("getrlimit: %v", err)
		}
		lim := syscall.Rlimit{Cur: 4096, Max: orig.Max}
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
			t.Fatalf("setrlimit: %v", err)
		}
		defer func() {
			if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig); err != nil {
				t.Errorf("restoring the file size limit: %v", err)
			}
		}()
		pin, err := pinWriteTarget(dir, "a.txt")
		if err != nil {
			t.Fatal(err)
		}
		if err := pin.Write([]byte(strings.Repeat("N", 64<<10)), 0o644); err == nil {
			t.Fatal("a write over the file size limit reported success")
		}
		return
	}

	root := t.TempDir()
	original := strings.Repeat("O", 2048)
	full := seedFile(t, root, "a.txt", original)
	cmd := exec.Command(os.Args[0], "-test.run=^TestAWriteThatFailsPartWayLeavesTheOriginal$")
	cmd.Env = append(os.Environ(), "MANVI_FSIZE_CHILD_DIR="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(full); string(got) != original {
		t.Fatalf("the original was destroyed by a failed write: %d bytes, starts %q", len(got), string(got[:8]))
	}
	if extra := leftovers(t, root, "a.txt"); len(extra) != 0 {
		t.Fatalf("a failed write left %v behind", extra)
	}
}

// TestAFailedWriteThatMayHaveChangedTheFileStillClaimsIt: when the rename has
// landed and only the directory sync failed, the bytes are in place, so the
// checkpoint must be told to look at the path.
