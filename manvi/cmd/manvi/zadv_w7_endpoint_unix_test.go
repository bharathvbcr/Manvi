//go:build unix

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The registry opens the repository file with OpenRegular, which refuses a
// FIFO so that a hostile tree cannot hang the harness. The refusal reads the
// same path first with a plain os.Open, which blocks on a FIFO with no writer.
func TestZAdvRepositoryConfigFIFODoesNotHang(t *testing.T) {
	isolatedUserConfig(t)
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := syscall.Mkfifo(cfg, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := newRegistry(cfg)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		// Release every blocked reader (the refusal's, then DevCouncil's own
		// retired-key scan) so no goroutine outlives the test.
		for {
			if w, err := os.OpenFile(cfg, os.O_RDWR, 0); err == nil {
				_ = w.Close()
			}
			select {
			case <-done:
				t.Fatal("newRegistry blocked on a FIFO at the repository config path: Manvi's readConfigFile and DevCouncil's refuseRetiredConfig both open it with a plain os.Open")
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
}
