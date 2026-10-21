//go:build unix

package devcouncil

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/harness"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// registerWithin opens a session and fails if it takes longer than limit. The
// registration goroutine is released at cleanup by opening the FIFO's write
// end, so a hang does not outlive the test binary's useful life.
func registerWithin(t *testing.T, f *skillFixture, fifo string, limit time.Duration) (*tools.Registry, time.Duration) {
	t.Helper()
	type out struct {
		pipe *tools.Registry
		err  error
	}
	done := make(chan out, 1)
	start := time.Now()
	go func() {
		pipe := tools.NewRegistry(bus.New())
		done <- out{pipe, f.reg.Register(pipe)}
	}()
	t.Cleanup(func() {
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil { //nolint:gosec // G304: the test's own FIFO path
			_ = w.Close()
		}
	})
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("register: %v", o.err)
		}
		return o.pipe, time.Since(start)
	case <-time.After(limit):
		t.Fatalf("opening a session took more than %s: the skillOpenTimeout (%s) bound did not hold", limit, skillOpenTimeout)
		return nil, 0
	}
}

// The 10s bound is carried by a context, but harness.LoadSkills lists
// .agents/skills with os.Root.Open + ReadDir, which take no context. A
// repository that makes .agents/skills a FIFO blocks that open until a writer
// appears: session open (and `manvi tools`) hangs unbounded.
func TestAdversarialFIFOAsSkillsDirectoryCannotStallSessionOpen(t *testing.T) {
	f := newSkillFixture(t)
	if err := os.MkdirAll(filepath.Join(f.root, ".agents"), 0o750); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(f.root, harness.SkillsDir)
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	pipe, took := registerWithin(t, f, fifo, skillOpenTimeout+5*time.Second)
	t.Logf("session open took %s", took)
	requirePresent(t, pipe)
}

// A FIFO as a skill's SKILL.md goes through readContained, which does honour
// the context: open must finish near the bound, and offer the tool so the
// model can see the fault.
func TestAdversarialFIFOAsSkillFileIsBoundedAndOffersTheTool(t *testing.T) {
	f := newSkillFixture(t)
	dir := filepath.Join(f.root, harness.SkillsDir, "evil")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, harness.SkillFile)
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	pipe, took := registerWithin(t, f, fifo, skillOpenTimeout+5*time.Second)
	t.Logf("session open took %s", took)
	requirePresent(t, pipe)
}

// A repository can make its skills directory unreadable to force the tool in.
// That is harmless (the tool answers with the fault) but it must say what
// failed and must not leak a listing.
func TestAdversarialUnreadableSkillsDirectoryOffersToolThatReportsTheFault(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	f := newSkillFixture(t)
	dir := filepath.Join(f.root, harness.SkillsDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // G302: a directory, not a file; 0600 would make it unremovable
	pipe := f.openSession(t)
	requirePresent(t, pipe)
	res := runSkill(pipe, "anything")
	if !res.IsError || strings.TrimSpace(res.Text) == "" {
		t.Fatalf("the tool must answer with the fault, got error=%v %q", res.IsError, res.Text)
	}
	t.Logf("tool answer: %s", res.Text)
}
