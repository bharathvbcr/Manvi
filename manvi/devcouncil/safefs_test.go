package devcouncil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests freeze the check-then-act race deterministically: pin the
// target, perform the attacker's swap while "policy deliberation" is in
// flight, then attempt the write. A naive implementation — os.WriteFile on a
// resolved path — fails every one of these, because the kernel re-traverses
// the swapped chain at open time. That is exactly the defect this file
// exists to prevent, and these are the tests that stay red if the identity
// verification is ever removed.

func swapDirWithSymlink(t *testing.T, root, dir, outside string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(root, dir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, dir)); err != nil {
		t.Fatal(err)
	}
}

// TestWriteRefusesAfterDirectorySwapToOutside: the canonical TOCTOU. configs/
// is a real directory when policy evaluates configs/app.conf; by the time the
// approval prompt is answered it points at ~/.ssh. The write must refuse, and
// nothing may land outside the repository.
func TestWriteRefusesAfterDirectorySwapToOutside(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "app.conf")
	if err := os.WriteFile(victim, []byte("innocent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "configs", "app.conf"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pinned, err := pinWriteTarget(root, "configs/app.conf")
	if err != nil {
		t.Fatalf("pin failed before the swap: %v", err)
	}

	swapDirWithSymlink(t, root, "configs", outside)

	err = pinned.Write([]byte("exfiltrated credentials\n"), 0o644)
	if err == nil {
		t.Fatal("wrote straight through a swapped directory")
	}
	if !strings.Contains(err.Error(), "identity") && !strings.Contains(err.Error(), "symbolic") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	after, readErr := os.ReadFile(victim)
	if readErr != nil || string(after) != "innocent\n" {
		t.Fatalf("the file outside the repository was touched: %q (%v)", after, readErr)
	}
}

// TestWriteRefusesWhenLeafBecomesASymlink: the leaf itself swapped from a
// regular file to a link aimed elsewhere. ELOOP or identity mismatch — either
// refusal is correct; silence is not.
func TestWriteRefusesWhenLeafBecomesASymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "real.txt")
	if err := os.WriteFile(victim, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pinned, err := pinWriteTarget(root, "file.txt")
	if err != nil {
		t.Fatalf("pin failed: %v", err)
	}

	if err := os.Remove(filepath.Join(root, "file.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(root, "file.txt")); err != nil {
		t.Fatal(err)
	}

	if err := pinned.Write([]byte("through the link\n"), 0o644); err == nil {
		t.Fatal("wrote through a symlinked leaf")
	}
	after, readErr := os.ReadFile(victim)
	if readErr != nil || string(after) != "keep\n" {
		t.Fatalf("the link target was overwritten: %q (%v)", after, readErr)
	}
}

// TestCreateRefusesAfterDirectorySwap: the create case. The file did not
// exist at pin time, so protection rests entirely on the directory identity;
// a swapped directory must void the write even though there is no leaf
// identity to compare.
func TestCreateRefusesAfterDirectorySwap(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}

	pinned, err := pinWriteTarget(root, "configs/new.conf")
	if err != nil {
		t.Fatalf("pin failed: %v", err)
	}

	swapDirWithSymlink(t, root, "configs", outside)

	if err := pinned.Write([]byte("stray payload\n"), 0o644); err == nil {
		t.Fatal("created a file through a swapped directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.conf")); !os.IsNotExist(err) {
		t.Fatal("a stray file landed outside the repository")
	}
}

// TestPinRefusesPreexistingEscapingSymlink: a symlink planted BEFORE
// evaluation is the easy case — the pin's containment walk must refuse it
// outright.
func TestPinRefusesPreexistingEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "configs")); err != nil {
		t.Fatal(err)
	}
	if _, err := pinWriteTarget(root, "configs/app.conf"); err == nil {
		t.Fatal("an escaping symlink was accepted at pin time")
	}
}

// TestPinRefusesSymlinkedLeaf: writing through a link named as the leaf is
// refused at evaluation time, before any identity games are needed.
func TestPinRefusesSymlinkedLeaf(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := pinWriteTarget(root, "link.txt"); err == nil {
		t.Fatal("a symlinked leaf was accepted for writing")
	}
}

// TestReadRefusesAfterDirectorySwap: reading through a swapped directory is
// an exfiltration channel — the model would see whatever file now sits at the
// path. The read must verify identity just like a write.
func TestReadRefusesAfterDirectorySwap(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "configs", "app.conf"), []byte("inside\n"), 0o755); err != nil {
		if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "configs", "app.conf"), []byte("inside\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pinned, err := pinWriteTarget(root, "configs/app.conf")
	if err != nil {
		t.Fatalf("pin failed: %v", err)
	}

	swapDirWithSymlink(t, root, "configs", outside)
	if err := os.WriteFile(filepath.Join(outside, "app.conf"), []byte("OUTSIDE SECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	data, err := pinned.Read(0)
	if err == nil {
		t.Fatalf("read through a swapped directory: %q", data)
	}
}

// TestWriteRefusesNonRegularFile: a FIFO planted at the target path must not
// become a place to block forever (a FIFO opened write-only with no reader
// blocks without O_NONBLOCK).
func TestWriteRefusesNonRegularFile(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "pipe")
	if err := syscall_Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("cannot create a fifo here: %v", err)
	}

	pinned, err := pinWriteTarget(root, "pipe")
	if err != nil {
		t.Fatalf("pin failed: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- pinned.Write([]byte("x"), 0o644) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("wrote to a fifo")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the write blocked on a fifo; non-regular refusal did not fire")
	}
}

// TestPinnedHappyPath: the ordinary cases must keep working end to end —
// create, overwrite, read back, remove.
func TestPinnedHappyPath(t *testing.T) {
	root := t.TempDir()

	// Production creates missing directories before pinning (writeFile's
	// MkdirAll step), so the test does too.
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	pinned, err := pinWriteTarget(root, "src/main.go")
	if err != nil {
		t.Fatalf("create pin: %v", err)
	}
	if err := pinned.Write([]byte("package main\n"), 0o644); err != nil {
		t.Fatalf("create write: %v", err)
	}

	data, err := ReadPinned(root, "src/main.go", 0)
	if err != nil || string(data) != "package main\n" {
		t.Fatalf("read-back: %q (%v)", data, err)
	}

	pinned2, err := pinWriteTarget(root, "src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := pinned2.Write([]byte("package main // v2\n"), 0o644); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	data, _ = ReadPinned(root, "src/main.go", 0)
	if string(data) != "package main // v2\n" {
		t.Fatalf("overwrite produced %q", data)
	}

	if err := RemovePinned(root, "src/main.go"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "src/main.go")); !os.IsNotExist(err) {
		t.Fatal("file survived removal")
	}
}

// TestPinRefusesTraversal: defence in depth beyond the policy normalizer.
func TestPinRefusesTraversal(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"../outside.txt", "/etc/passwd", "..", "."} {
		if _, err := pinWriteTarget(root, rel); err == nil {
			t.Fatalf("pin accepted %q", rel)
		}
	}
}
