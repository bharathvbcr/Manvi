package session

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

// TestAdvFirstSaveKillChild is the child of the first-save kill test: it saves
// a brand-new session (generation 0 -> 1) and kills itself at the named step.
func TestAdvFirstSaveKillChild(t *testing.T) {
	dir, step := os.Getenv(crashDirEnv), os.Getenv(crashStepEnv)
	if dir == "" || step == "" {
		t.Skip("only run as the child of the first-save kill test")
	}
	store := NewStore(dir)
	log := NewLog()
	appendTurn(t, log, "only")
	store.writeStep = func(at string) {
		if at == step {
			if self, err := os.FindProcess(os.Getpid()); err == nil {
				_ = self.Kill()
			}
			select {}
		}
	}
	if _, err := store.Save(crashID, log); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("the save finished without reaching step %q", step)
}

// The existing kill test seeds two generations, so the first-ever save of a
// session (no previous generation, no prune of an older one) is never
// interrupted. A kill before the link must leave "not found", never a
// CorruptError; from the link on, the session must load with its one turn.
func TestAdvFirstSaveKilledPartWay(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		step   string
		loaded bool
	}{{"created", false}, {"written", false}, {"synced", false}, {"linked", true}, {"published", true}} {
		t.Run(tc.step, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestAdvFirstSaveKillChild$", "-test.count=1")
			cmd.Env = append(os.Environ(), crashDirEnv+"="+dir, crashStepEnv+"="+tc.step)
			var exit *exec.ExitError
			if err := cmd.Run(); !errors.As(err, &exit) {
				t.Fatalf("child not killed: %v", err)
			}
			_, report, err := NewStore(dir).Load(crashID)
			if tc.loaded {
				if err != nil || report.Turns != 1 {
					t.Fatalf("killed at %q: turns=%d err=%v, want a loadable 1-turn session", tc.step, report.Turns, err)
				}
				return
			}
			var nf *NotFoundError
			if !errors.As(err, &nf) {
				t.Fatalf("killed at %q before the link: err=%v, want NotFoundError", tc.step, err)
			}
		})
	}
}

// writeStep is read inside Save under s.mu but written by tests without it;
// concurrent Saves on one Store with the seam unset must be race-free.
func TestAdvConcurrentSavesDoNotRaceOnTheSeam(t *testing.T) {
	store := NewStore(t.TempDir())
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			log := NewLog()
			appendTurn(t, log, "x")
			_, _ = store.Save(crashID, log)
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
}
