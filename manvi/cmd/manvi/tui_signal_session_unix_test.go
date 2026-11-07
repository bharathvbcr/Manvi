//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

// tuiPanicEnv makes the re-executed TUI send its model requests through a
// transport that panics on the first completion.
const tuiPanicEnv = "MANVI_TUI_PTY_PANIC"

// panickingModel stands in for http.DefaultTransport in the helper process: the
// models listing answers, the completion panics inside the adapter on the
// turn's goroutine, which is the failure a real adapter bug would be. It is
// reached because the base URL it is given is not a loopback one, and the
// adapter uses http.DefaultTransport for those.
type panickingModel struct{}

func (panickingModel) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/chat/completions") {
		panic("provider adapter: index out of range [3] with length 2")
	}
	body := `{"data":[{"id":"mock-model","object":"model","max_model_len":32768}]}`
	if req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/v1/models") {
		return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: http.NoBody,
			Header: http.Header{}, Request: req}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Request: req,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(body)),
	}, nil
}

// stateFixture is the project state a TUI under test shares with the test: the
// lease store, seeded with one task the model checks out, and the sessions
// directory the turn is persisted to.
type stateFixture struct {
	work     string
	stateDir string
	leases   *store.Client
	env      []string
}

func newStateFixture(t *testing.T, baseURL string) *stateFixture {
	t.Helper()
	f := &stateFixture{work: t.TempDir(), stateDir: filepath.Join(t.TempDir(), "state")}
	if err := os.MkdirAll(f.stateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(f.stateDir, "state.sqlite")
	testsupport.SeedTask(t, db, testsupport.TaskSeed{ID: "TASK-001", Title: "t",
		Scope: `{"planned_files":[{"path":"src/calc.go","allowed_change":"modify"}]}`})
	bin := testsupport.DCStore(t)
	f.leases = store.New(bin, db)
	f.env = []string{
		"MANVI_STATE_DIR=" + f.stateDir,
		"MANVI_STORE_DB=" + db,
		"MANVI_STORE_BINARY=" + bin,
		"MANVI_VERIFY_BINARY=" + testsupport.DCVerify(t),
		"MANVI_HARNESS_INIT_ENABLED=false",
		"MANVI_LLM_PROVIDER_DEFAULT=local",
		"MANVI_LLM_LOCAL_BASE_URL=" + baseURL,
		"MANVI_LLM_LOCAL_MODEL=mock-model",
	}
	return f
}

func (f *stateFixture) activeLeases(t *testing.T) []store.Lease {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	leases, err := f.leases.ActiveLeases(ctx)
	if err != nil {
		t.Fatalf("reading the lease store: %v", err)
	}
	return leases
}

// savedSession returns the one session the run persisted.
func (f *stateFixture) savedSession(t *testing.T) *session.Log {
	t.Helper()
	st := session.NewStore(filepath.Join(f.stateDir, "sessions"))
	saved, err := st.List()
	if err != nil || len(saved) != 1 {
		t.Fatalf("sessions on disk = %v, %v; want exactly the one the TUI ran", saved, err)
	}
	log, _, err := st.Load(saved[0].ID)
	if err != nil {
		t.Fatalf("the persisted session does not load: %v", err)
	}
	return log
}

// eventTypes is the sequence of record kinds a log holds.
func eventTypes(log *session.Log) []string {
	var kinds []string
	for _, e := range log.Events() {
		kinds = append(kinds, string(e.Type))
	}
	return kinds
}

// waitFor polls until cond holds or the deadline passes.
func waitUntil(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// liveTurn starts the TUI on a pty with a session whose turn holds a lease and
// is blocked inside the model: the scripted model's first answer checks out
// TASK-001, and the second never arrives. It returns once both are true.
func liveTurn(t *testing.T) (*stateFixture, *exec.Cmd, *os.File, *os.File, *ptyOutput) {
	t.Helper()
	hold := make(chan struct{})
	reached := make(chan struct{}, 4)
	model := &e2eModel{reply: func(turn int) (map[string]any, string) {
		if turn == 1 {
			return toolCall("devcouncil_checkout_task", map[string]any{"task_id": "TASK-001"})
		}
		reached <- struct{}{}
		<-hold
		return map[string]any{"content": "too late"}, "stop"
	}}
	base := model.start(t)
	t.Cleanup(func() { close(hold) }) // runs before the server closes: LIFO
	f := newStateFixture(t, base)
	cmd, master, slave, out := tuiOnAPtyIn(t, f.work, f.env...)

	// Let the first frame settle: keys that arrive before the composer exists
	// are dropped, and a dropped prompt would leave nothing to wait on.
	waitUntil(t, 30*time.Second, "the composer to draw", func() bool { return out.contains("mock-model") })
	if _, err := master.Write([]byte("claim the task\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reached:
	case <-time.After(60 * time.Second):
		t.Fatalf("the turn never reached its second model request; the screen had %d bytes", out.len())
	}
	if leases := f.activeLeases(t); len(leases) != 1 || leases[0].TaskID != "TASK-001" {
		t.Fatalf("active leases before the stop = %+v, want the one TASK-001 the turn checked out", leases)
	}
	return f, cmd, master, slave, out
}

// quitMidTurn ends a TUI whose turn is in flight the way an operator does,
// which is the reference the signal paths are held to: Ctrl-Q twice, the second
// confirming.
func quitMidTurn(t *testing.T, master *os.File) {
	t.Helper()
	if _, err := master.Write([]byte{0x11}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := master.Write([]byte{0x11}); err != nil {
		t.Fatal(err)
	}
}

// A stop that arrives while a turn holds a lease and is waiting on the model
// must end the run the way a quit does: terminal given back, the lease released
// in the store, and the turn's log on disk. The record sequence is compared
// with the keyboard quit's, so "as a normal quit does" is checked rather than
// assumed.
func TestTUIStopSignalsReleaseLeasesAndPersistTheInFlightTurn(t *testing.T) {
	// The reference: what a keyboard quit leaves behind.
	f, cmd, master, slave, out := liveTurn(t)
	mark := out.len()
	quitMidTurn(t, master)
	quitErr := waitExit(t, cmd, 60*time.Second)
	restoredAfter(t, slave, out, mark)
	if leases := f.activeLeases(t); len(leases) != 0 {
		t.Fatalf("a keyboard quit left leases held: %+v (exit %v)", leases, quitErr)
	}
	reference := f.savedSession(t)
	want := eventTypes(reference)
	if !containsText(reference, "claim the task") {
		t.Fatalf("the keyboard quit did not persist the prompt; the reference is empty: %v", want)
	}

	for _, tc := range []struct {
		name string
		sig  syscall.Signal
		said string
	}{
		{"SIGTERM", syscall.SIGTERM, "stopped by terminated"},
		{"SIGHUP", syscall.SIGHUP, "stopped by hangup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, cmd, _, slave, out := liveTurn(t)
			mark := out.len()
			if err := cmd.Process.Signal(tc.sig); err != nil {
				t.Fatalf("signalling the TUI: %v", err)
			}
			err := waitExit(t, cmd, 60*time.Second)
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("a stopped TUI exited with %v, want status 1", err)
			}
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				t.Fatalf("the TUI was killed by %v instead of shutting down", ws.Signal())
			}
			if ee.ExitCode() != 1 {
				t.Fatalf("exit status %d, want 1", ee.ExitCode())
			}
			restoredAfter(t, slave, out, mark)
			if tail := out.from(mark); !strings.Contains(tail, tc.said) {
				t.Fatalf("the stop was not reported (want %q): %q", tc.said, tail)
			}
			if leases := f.activeLeases(t); len(leases) != 0 {
				t.Fatalf("the stop left leases held: %+v", leases)
			}
			log := f.savedSession(t)
			if got := eventTypes(log); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("the stop persisted %v; a keyboard quit persists %v", got, want)
			}
			if !containsText(log, "claim the task") {
				t.Fatal("the persisted session does not hold the prompt of the interrupted turn")
			}
		})
	}
}

// containsText reports whether any record of the log carries text.
func containsText(log *session.Log, text string) bool {
	for _, e := range log.Events() {
		if strings.Contains(string(e.Data), text) {
			return true
		}
	}
	return false
}

// A panic on the turn's goroutine -- here inside the model adapter, in the real
// binary on a real terminal -- becomes an error in the transcript. The terminal
// stays the TUI's until the operator quits, the trace is written to stderr once
// the terminal is back, and the session is saved with the turn's prompt.
func TestTUITurnPanicIsAnErrorWithTheTerminalRestored(t *testing.T) {
	f := newStateFixture(t, "http://models.invalid/v1")
	cmd, master, slave, out := tuiOnAPtyIn(t, f.work, append(f.env, tuiPanicEnv+"=1")...)
	waitUntil(t, 30*time.Second, "the composer to draw", func() bool { return out.contains("mock-model") })
	if _, err := master.Write([]byte("this turn will panic\r")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 60*time.Second, "the panic to be reported as an error", func() bool {
		return out.contains("internal error")
	})
	// Still running: a recovered panic does not take the process with it.
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the TUI is gone after a turn panicked: %v", err)
	}
	if out.contains("\x1b[?1049l") {
		t.Fatal("the alternate screen was left before the operator quit")
	}

	mark := out.len()
	quitMidTurn(t, master)
	err := waitExit(t, cmd, 60*time.Second)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			t.Fatalf("the TUI was killed by %v", ws.Signal())
		}
	}
	restoredAfter(t, slave, out, mark)
	tail := out.from(mark)
	if !strings.Contains(tail, "a turn panicked") || !strings.Contains(tail, "index out of range") {
		t.Fatalf("the panic's trace was not written to the restored terminal: %q", tail)
	}
	if !containsText(f.savedSession(t), "this turn will panic") {
		t.Fatal("the panicked turn's session was not saved with its prompt")
	}
	if leases := f.activeLeases(t); len(leases) != 0 {
		t.Fatalf("leases still held: %+v", leases)
	}
}
