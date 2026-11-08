//go:build unix

package codingagent

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// FIFOs where the seat code reads files other than the three the existing test
// covers: a seat's .claude.json, ~/.claude.json and the worktree commondir.
func TestAdvW16FIFOsInIdentityAndCommondirFilesDoNotHang(t *testing.T) {
	cases := map[string]func(w advSeatWorld) string{
		"seat .claude.json": func(w advSeatWorld) string { return filepath.Join(w.seatWork, ".claude.json") },
		"home .claude.json": func(w advSeatWorld) string { return filepath.Join(w.home, ".claude.json") },
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			w := advSeats(t)
			advWrite(t, filepath.Join(w.seatLappi, ".claude.json"), `{"oauthAccount":{"emailAddress":"l@example.com"}}`)
			if err := syscall.Mkfifo(plant(w), 0o600); err != nil {
				t.Skip(err)
			}
			advRunBounded(t, func() { _, _ = claudeSeatOverrides(w.repo) })
		})
	}
	t.Run("commondir", func(t *testing.T) {
		w := advSeats(t)
		gd := filepath.Join(w.repo, "gd")
		advWrite(t, filepath.Join(gd, "placeholder"), "x")
		if err := syscall.Mkfifo(filepath.Join(gd, "commondir"), 0o600); err != nil {
			t.Skip(err)
		}
		advWrite(t, filepath.Join(w.repo, ".git"), "gitdir: "+gd+"\n")
		advRunBounded(t, func() { _, _ = claudeSeatOverrides(w.repo) })
	})
	t.Run("marker in an ancestor", func(t *testing.T) {
		w := advSeats(t)
		if err := syscall.Mkfifo(filepath.Join(w.home, "Code", ".claude-account"), 0o600); err != nil {
			t.Skip(err)
		}
		advRunBounded(t, func() { _, _ = claudeSeatOverrides(w.repo) })
	})
}

func advRunBounded(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("blocked reading a FIFO")
	}
}
