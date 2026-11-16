//go:build unix

package tui

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/internal/ptytest"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
	"github.com/bharathvbcr/Manvi/manvi/ui/term"
)

// readPTY collects what the runner writes to the terminal side of a pty.
func readPTY(master *os.File) <-chan []byte {
	written := make(chan []byte, 1024)
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				written <- chunk
			}
			if err != nil {
				close(written)
				return
			}
		}
	}()
	return written
}

func gather(ch <-chan []byte) string {
	var all []byte
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				return string(all)
			}
			all = append(all, chunk...)
		case <-time.After(300 * time.Millisecond):
			return string(all)
		}
	}
}

// The runner's half: every frame it draws is followed by the tab being brought
// in step, on a real terminal, and fifty frames of one running phase put one
// title and one progress sequence on the wire. This is the test that would
// fail if the sync were ever moved somewhere a frame does not reach, or if the
// title were ever given a spinner.
func TestRunnerKeepsTheTabInStepWithTheFrame(t *testing.T) {
	for _, k := range []string{"MANVI_TERM_TITLE", "MANVI_TERM_PROGRESS", "TMUX", "STY", "ConEmuANSI", "TERM_PROGRAM", "TERM_PROGRAM_VERSION"} {
		t.Setenv(k, "")
	}
	t.Setenv("TERM", "xterm-256color")
	// The locale is what says the terminal draws block characters, and so
	// whether the title leads with the ribbon or with the letter.
	t.Setenv("LC_ALL", "en_US.UTF-8")
	t.Setenv("WT_SESSION", "0c1d")

	master, slave := ptytest.Open(t)
	ptytest.SetWinsize(t, slave, 100, 30)
	written := readPTY(master)

	r, err := New(Config{Host: &stubHost{}, In: slave, Out: slave, Title: "manvi"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.term.Start(term.Options{AltScreen: true, Title: r.cfg.Title}); err != nil {
		t.Fatal(err)
	}
	w, h := r.term.Size()
	r.painter = render.NewPainter(w, h, r.term.Profile())
	r.app.Dispatch(ActionResize{W: w, H: h})
	_ = r.app.AddSession("S1", "session one")
	frame := func() {
		t.Helper()
		if err := r.draw(); err != nil {
			t.Fatal(err)
		}
	}

	frame()
	r.app.Dispatch(ActionTurnStarted{SessionID: "S1"})
	for range 50 {
		r.app.Dispatch(ActionTick{})
		frame()
	}
	r.app.Dispatch(ActionTurnEnded{SessionID: "S1", Err: errors.New("provider refused")})
	frame()
	if err := r.term.Stop(); err != nil {
		t.Fatal(err)
	}
	all := gather(written)

	count := func(seq string) int { return strings.Count(all, seq) }
	if n := count("\x1b]0;▞▚▞▚ session one\x07"); n != 1 {
		t.Fatalf("the idle title was written %d times, want 1", n)
	}
	if n := count("\x1b]0;▞▚▞▚ working · session one\x07"); n != 1 {
		t.Fatalf("fifty frames of one running phase wrote the title %d times, want 1", n)
	}
	if n := count("\x1b]9;4;3;0\x07"); n != 1 {
		t.Fatalf("fifty frames of one running phase wrote the busy indicator %d times, want 1", n)
	}
	if n := count("\x1b]0;▞▚▞▚ failed · session one\x07"); n != 1 {
		t.Fatalf("the failed title was written %d times, want 1", n)
	}
	failed := strings.Index(all, "\x1b]9;4;2;100\x07")
	cleared := strings.LastIndex(all, "\x1b]9;4;0;0\x07")
	plain := strings.LastIndex(all, "\x1b]0;manvi\x07")
	if failed < 0 || cleared < failed || plain < failed {
		t.Fatalf("the run ended in an error state and Stop did not clear it and put the plain title back (error at %d, clear at %d, plain title at %d)",
			failed, cleared, plain)
	}
	// Start, idle, working, failed, and the plain one put back: five titles
	// for a run of fifty-two frames.
	if n := count("\x1b]0;"); n != 5 {
		t.Fatalf("%d title sequences were written in all, want 5", n)
	}
}
