package codingagent

import (
	"context"
	"io"
	"strings"
	"testing"
)

// The stdout cap is per session on purpose: it bounds a runaway or hostile
// provider over the whole unattended turn, and it counts every frame the
// provider writes, including tool results the run does not record. Because
// that is surprising for a long session that reads a lot of files, the failure
// says so.
func TestSessionOutputCapIsCumulativeAndSaysSo(t *testing.T) {
	r, w := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := &connection{ctx: ctx, cancel: cancel, stdout: r, frames: make(chan wireRead, 4),
		validate: func([]byte) error { return nil }}
	go c.read()
	go func() {
		line := `{"type":"user","pad":"` + strings.Repeat("x", 200*1024) + `"}` + "\n"
		for i := 0; i < 200; i++ { // 200 frames of ~200 KiB: far past 32 MiB in total, never past one frame's cap
			if _, err := io.WriteString(w, line); err != nil {
				return
			}
		}
		_ = w.Close()
	}()
	for rd := range c.frames {
		if rd.err == nil {
			continue
		}
		for _, want := range []string{"32 MiB", "whole session", "every frame"} {
			if !strings.Contains(rd.err.Error(), want) {
				t.Fatalf("cap failure %q does not say %q", rd.err, want)
			}
		}
		return
	}
	t.Fatal("200 frames of 200 KiB did not trip the session output cap")
}
