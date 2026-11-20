//go:build unix

package treefile

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// EMFILE likewise.
func TestZadvW24DescriptorExhaustionIsNotARefusal(t *testing.T) {
	anchor, _ := tree(t)
	if err := os.WriteFile(filepath.Join(anchor, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &old); err != nil {
		t.Skip(err)
	}
	var held []*os.File
	t.Cleanup(func() {
		for _, f := range held {
			_ = f.Close()
		}
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &old)
	})
	low := syscall.Rlimit{Cur: 300, Max: old.Max}
	if old.Cur < low.Cur {
		low.Cur = old.Cur
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &low); err != nil {
		t.Skip(err)
	}
	for {
		f, err := os.Open(os.DevNull)
		if err != nil {
			break
		}
		held = append(held, f)
		if len(held) > 100000 {
			t.Skip("could not exhaust descriptors")
		}
	}
	_, err = ReadFile(root, "f", ReadOptions{Limit: 1 << 20})
	if err == nil {
		t.Skip("read succeeded with the table full")
	}
	if errors.Is(err, ErrRefused) {
		t.Errorf("EMFILE reported as a refusal of the entry: %v", err)
	}
}
