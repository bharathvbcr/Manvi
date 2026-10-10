package session

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

const (
	crashDirEnv = "MANVI_SESSION_CRASH_DIR"
	crashID     = "c0ffee"
)

// TestSaveLoopChild is the process TestASaveKilledPartWayLeavesALoadableSession
// kills. It saves a growing log, a generation per turn, until it is killed.
func TestSaveLoopChild(t *testing.T) {
	dir := os.Getenv(crashDirEnv)
	if dir == "" {
		t.Skip("only run as the child of the kill test")
	}
	store := NewStore(dir)
	log, _, err := store.Load(crashID)
	var notFound *NotFoundError
	if errors.As(err, &notFound) {
		log = NewLog()
	} else if err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("y", 64<<10)
	for turn := 0; ; turn++ {
		benchAppend(t, log, TurnStart, nil)
		benchAppend(t, log, UserMessage, MessageData{Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: text}}}})
		if _, err := store.Save(crashID, log); err != nil {
			t.Fatal(err)
		}
	}
}

// A save is a new generation published only once its bytes are on disk. Killing
// the writer at arbitrary points — mid-encode, mid-write, between fsync and
// link — must therefore leave a session that loads, with whole turns and an
// intact checksum, and never a torn file under a generation's name. The kill
// is a SIGKILL, so no deferred cleanup runs: whatever recovery needs is on disk.
func TestASaveKilledPartWayLeavesALoadableSession(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns and kills processes")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	lastTurns := 0
	for round := 0; round < 12; round++ {
		cmd := exec.Command(exe, "-test.run=^TestSaveLoopChild$", "-test.count=1")
		cmd.Env = append(os.Environ(), crashDirEnv+"="+dir)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		// Staggered so the kill lands at different points of the cycle.
		time.Sleep(time.Duration(40+round*17) * time.Millisecond)
		if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()

		store := NewStore(dir)
		log, report, err := store.Load(crashID)
		var notFound *NotFoundError
		switch {
		case errors.As(err, &notFound):
			continue // killed before the first generation was published
		case err != nil:
			t.Fatalf("round %d: a killed save left an unloadable session: %v", round, err)
		}
		if report.Turns < lastTurns {
			t.Fatalf("round %d: the session went back from %d turns to %d", round, lastTurns, report.Turns)
		}
		lastTurns = report.Turns
		t.Logf("round %d: generation %d, %d turns survived the kill", round, report.Generation, report.Turns)
		msgs, err := log.DeriveMessages()
		if err != nil || len(msgs) != report.Turns {
			t.Fatalf("round %d: loaded %d messages for %d turns, err %v", round, len(msgs), report.Turns, err)
		}
		// The next run resumes from what survived and saves on top of it.
		benchAppend(t, log, TurnStart, nil)
		benchAppend(t, log, UserMessage, MessageData{Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "resumed " + strconv.Itoa(round)}}}})
		if _, err := store.Save(crashID, log); err != nil {
			t.Fatalf("round %d: resuming after a kill: %v", round, err)
		}
		lastTurns = report.Turns + 1
		if round >= 2 && report.Turns < 3*(round/3) {
			t.Fatalf("round %d: only %d turns survived; the child was not saving", round, report.Turns)
		}
	}
	if lastTurns == 0 {
		t.Fatal("no round published a generation; the kill test exercised nothing")
	}
}
