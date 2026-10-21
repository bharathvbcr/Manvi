package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

const (
	crashDirEnv  = "MANVI_SESSION_CRASH_DIR"
	crashStepEnv = "MANVI_SESSION_CRASH_STEP"
	crashID      = "c0ffee"
)

func appendTurn(t *testing.T, log *Log, text string) {
	t.Helper()
	benchAppend(t, log, TurnStart, nil)
	benchAppend(t, log, UserMessage, MessageData{Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: text}}}})
}

// TestSaveKillChild is the process TestASaveKilledPartWayLeavesALoadableSession
// kills. It resumes the session, appends one turn and saves it, and SIGKILLs
// itself on reaching the named write step, so no deferred cleanup runs and the
// directory is left exactly as that step left it.
func TestSaveKillChild(t *testing.T) {
	dir, step := os.Getenv(crashDirEnv), os.Getenv(crashStepEnv)
	if dir == "" || step == "" {
		t.Skip("only run as the child of the kill test")
	}
	store := NewStore(dir)
	log, _, err := store.Load(crashID)
	if err != nil {
		t.Fatal(err)
	}
	appendTurn(t, log, "the interrupted turn")
	store.writeStep = func(at string) {
		if at == step {
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			select {} // the signal is on its way; never run past the step
		}
	}
	if _, err := store.Save(crashID, log); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("the save finished without reaching step %q", step)
}

// A save is a new generation published only once its bytes are on disk. Killing
// the writer at any step of writeNew — temp file created, bytes written, bytes
// synced, name linked, directory synced — must leave a session that loads at
// the last complete generation, with whole turns and an intact checksum, and
// never a torn file under a generation's name. The child kills itself at the
// step, by SIGKILL, so nothing depends on scheduling.
func TestASaveKilledPartWayLeavesALoadableSession(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns and kills processes")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// wantTurns is what Load must return: the seeded two turns until the link
	// publishes the new generation, three from then on.
	steps := []struct {
		step      string
		wantTurns int
	}{
		{"created", 2}, {"written", 2}, {"synced", 2}, {"linked", 3}, {"published", 3},
	}
	for _, tc := range steps {
		t.Run(tc.step, func(t *testing.T) {
			dir := t.TempDir()
			seed := NewStore(dir)
			log := NewLog()
			appendTurn(t, log, "first")
			if _, err := seed.Save(crashID, log); err != nil {
				t.Fatal(err)
			}
			appendTurn(t, log, "second")
			if _, err := seed.Save(crashID, log); err != nil {
				t.Fatal(err)
			}

			cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestSaveKillChild$", "-test.count=1")
			cmd.Env = append(os.Environ(), crashDirEnv+"="+dir, crashStepEnv+"="+tc.step)
			err := cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("the child was not killed: %v", err)
			}
			if ws, ok := exit.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
				t.Fatalf("the child did not die by SIGKILL at step %q: %v", tc.step, exit)
			}

			check := func(label string) int {
				t.Helper()
				store := NewStore(dir)
				got, report, err := store.Load(crashID)
				if err != nil {
					t.Fatalf("%s: a save killed at %q left an unloadable session: %v", label, tc.step, err)
				}
				if report.Turns != tc.wantTurns {
					t.Fatalf("%s: killed at %q, Load returned %d turns (generation %d), want %d",
						label, tc.step, report.Turns, report.Generation, tc.wantTurns)
				}
				msgs, err := got.DeriveMessages()
				if err != nil || len(msgs) != report.Turns {
					t.Fatalf("%s: loaded %d messages for %d turns, err %v", label, len(msgs), report.Turns, err)
				}
				return report.Generation
			}
			generation := check("after the kill")

			// A kill during the write itself leaves a torn temp file rather
			// than an empty or whole one. It is not a generation, so tearing
			// it further must change nothing.
			// From the link on, the temp file is a second name for the
			// published generation, so it is only torn before that.
			var tmps []string
			if tc.wantTurns == 2 {
				tmps, err = filepath.Glob(filepath.Join(dir, ".session-*.tmp"))
				if err != nil {
					t.Fatal(err)
				}
				if len(tmps) != 1 {
					t.Fatalf("killed at %q: %d temp files left, want 1", tc.step, len(tmps))
				}
			}
			for _, tmp := range tmps {
				info, err := os.Stat(tmp)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(tmp, info.Size()/2); err != nil {
					t.Fatal(err)
				}
			}
			check("with the temp file torn")

			// The next run resumes from what survived and saves on top of it.
			store := NewStore(dir)
			resumed, _, err := store.Load(crashID)
			if err != nil {
				t.Fatal(err)
			}
			appendTurn(t, resumed, "resumed")
			report, err := store.Save(crashID, resumed)
			if err != nil {
				t.Fatalf("resuming after a kill at %q: %v", tc.step, err)
			}
			if report.Generation != generation+1 {
				t.Fatalf("resumed save is generation %d, want %d", report.Generation, generation+1)
			}
		})
	}
}
