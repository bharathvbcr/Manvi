//go:build unix

package mcp

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A trust file that is a FIFO must not hang the read of it.
func TestZadvW24ATrustFileThatIsAFIFODoesNotHang(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "trust.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("no FIFOs: %v", err)
	}
	t.Setenv(TrustFileEnv, fifo)
	t.Setenv(TrustListEnv, "")
	done := make(chan trustStore, 1)
	go func() { done <- loadTrustStore(t.TempDir()) }()
	// Release a blocked open at the end of the test either way.
	t.Cleanup(func() {
		go func() {
			if f, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil { // #nosec G304 -- a path under this test's own t.TempDir
				_ = f.Close()
			}
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})
	select {
	case store := <-done:
		if store.err == nil {
			t.Fatal("a FIFO was read as an empty authorization list without an error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reading a FIFO trust file hung")
	}
}
