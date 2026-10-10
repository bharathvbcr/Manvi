package devcouncil

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
)

// failingFsync swaps one durability seam for the length of a test.
func failSeam[T any](t *testing.T, seam *T, replacement T) {
	t.Helper()
	old := *seam
	*seam = replacement
	t.Cleanup(func() { *seam = old })
}

func seedFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.WriteFile(full, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	return full
}

// leftovers lists anything in dir besides the named files: a temp sibling the
// write failed to clean up is clutter in the operator's tree.
func leftovers(t *testing.T, dir string, keep ...string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var extra []string
	for _, e := range entries {
		kept := false
		for _, k := range keep {
			kept = kept || e.Name() == k
		}
		if !kept {
			extra = append(extra, e.Name())
		}
	}
	return extra
}

// TestPinnedWriteReturnsASyncFailure: an fsync that fails means the bytes are
// not known to be on disk, so the write did not succeed.
func TestPinnedWriteReturnsASyncFailure(t *testing.T) {
	root := t.TempDir()
	full := seedFile(t, root, "a.txt", "original")
	pin, err := pinWriteTarget(root, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	failSeam(t, &syncFile, func(*os.File) error { return errors.New("injected fsync failure") })

	if err := pin.Write([]byte("replacement"), 0o644); err == nil {
		t.Fatal("a failed fsync was reported as a successful write")
	}
	if got, _ := os.ReadFile(full); string(got) != "original" {
		t.Fatalf("the original was lost to a write that failed: %q", got)
	}
	if extra := leftovers(t, root, "a.txt"); len(extra) != 0 {
		t.Fatalf("a failed write left %v behind", extra)
	}
}

// TestPinnedWriteReturnsACloseFailure: close-time write-back errors (NFS, SMB,
// delayed allocation) are the only place some failures are reported.
func TestPinnedWriteReturnsACloseFailure(t *testing.T) {
	for _, existing := range []bool{true, false} {
		root := t.TempDir()
		if existing {
			seedFile(t, root, "a.txt", "original")
		}
		pin, err := pinWriteTarget(root, "a.txt")
		if err != nil {
			t.Fatal(err)
		}
		failSeam(t, &closeFile, func(f *os.File) error {
			_ = f.Close()
			return errors.New("injected close failure")
		})
		if err := pin.Write([]byte("replacement"), 0o644); err == nil {
			t.Fatalf("existing=%v: a failed close was reported as a successful write", existing)
		}
		got, readErr := os.ReadFile(filepath.Join(root, "a.txt"))
		switch {
		case existing && string(got) != "original":
			t.Fatalf("the original was lost to a write that failed: %q", got)
		case !existing && readErr == nil:
			t.Fatalf("a file that never finished being written was left in place: %q", got)
		}
	}
}

// TestPinnedWriteKeepsTheModeOfTheFileItReplaces: replacing through a temp file
// must not turn an operator's 0640 file into 0644.
func TestPinnedWriteKeepsTheModeOfTheFileItReplaces(t *testing.T) {
	root := t.TempDir()
	full := seedFile(t, root, "a.txt", "original")
	pin, err := pinWriteTarget(root, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := pin.Write([]byte("replacement"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(full)
	if got, _ := os.ReadFile(full); string(got) != "replacement" {
		t.Fatalf("content = %q", got)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, want 0640", info.Mode().Perm())
	}
	if extra := leftovers(t, root, "a.txt"); len(extra) != 0 {
		t.Fatalf("a successful write left %v behind", extra)
	}
}

// TestAWriteThatFailsPartWayLeavesTheOriginal is the ENOSPC/EFBIG case: the
// subprocess lowers RLIMIT_FSIZE below the replacement's size, so the write
// fails after some bytes have gone out.
func TestAWriteThatFailsPartWayLeavesTheOriginal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("RLIMIT_FSIZE is not available on windows")
	}
	if dir := os.Getenv("MANVI_FSIZE_CHILD_DIR"); dir != "" {
		var lim syscall.Rlimit
		lim.Cur, lim.Max = 4096, 4096
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
			t.Fatalf("setrlimit: %v", err)
		}
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
func TestAFailedWriteThatMayHaveChangedTheFileStillClaimsIt(t *testing.T) {
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})
	const rel = "internal/helper.go"
	if first := f.call("devcouncil_write_file", map[string]string{"path": rel, "content": "package internal\n"}); first.IsError {
		t.Fatalf("setup write failed: %s", first.Text)
	}
	failSeam(t, &syncDir, func(*os.Root) error { return errors.New("injected directory sync failure") })

	// The first pass replaces an existing file, the second creates one: both
	// land their bytes before the directory sync fails.
	for _, target := range []string{rel, "internal/second.go"} {
		result := f.call("devcouncil_write_file", map[string]string{"path": target, "content": "package internal // v2\n"})
		if !result.IsError {
			t.Fatalf("%s: a write whose directory sync failed reported success: %q", target, result.Text)
		}
		got, _ := os.ReadFile(filepath.Join(f.root, target))
		if string(got) != "package internal // v2\n" {
			t.Fatalf("%s: the new bytes did not land (%q); the write was refused: %s", target, got, result.Text)
		}
		if len(result.Wrote) != 1 || result.Wrote[0] != target {
			t.Fatalf("%s: the changed path was not claimed: Wrote=%v", target, result.Wrote)
		}
	}
}
