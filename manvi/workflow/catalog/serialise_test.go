package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/internal/filelock"
)

// SetStability and setStatus each read current.json, change it and write it
// back. A Revoke that landed between SetStability's read and its rename used to
// be overwritten by the stale "approved", so a revoked capability was approved
// again and unattended replay accepted it. Port of the audit F reproduction.
func TestRevokeIsNotUndoneByAConcurrentStabilityWrite(t *testing.T) {
	const rounds = 300
	resurrected := 0
	for i := 0; i < rounds; i++ {
		s := Store{Root: t.TempDir()}
		e, err := s.Put(catalogFixture(t, "1"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Approve(e.ID, e.Revision, e.SHA256); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var revokeErr, stabErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			stabErr = s.SetStability(e.ID, e.Revision, e.SHA256, Stability{SuccessRate: 1, FirstAttemptRate: 1, Runs: 5})
		}()
		go func() {
			defer wg.Done()
			<-start
			revokeErr = s.Revoke(e.ID, e.Revision, e.SHA256)
		}()
		close(start)
		wg.Wait()
		if revokeErr != nil || stabErr != nil {
			t.Fatalf("revoke=%v stability=%v", revokeErr, stabErr)
		}
		got, err := s.Get(e.ID, e.Revision)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusRevoked {
			resurrected++
		} else if err := CheckUnattended(got); err == nil {
			t.Fatal("CheckUnattended admitted a revoked entry")
		}
	}
	if resurrected > 0 {
		t.Fatalf("Revoke returned success but the entry was not revoked afterwards in %d of %d rounds (CheckUnattended would admit it)", resurrected, rounds)
	}
}

// Another process writes the same catalog: the Jarvis daemon and its CLI both
// go through this package. A writer in this process must wait for it, which is
// what holding the identity's lock file from a separate open file looks like to
// the kernel.
func TestWritersWaitForAnotherProcessHoldingTheEntryLock(t *testing.T) {
	s := Store{Root: t.TempDir()}
	e, err := s.Put(catalogFixture(t, "1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(e.ID, e.Revision, e.SHA256); err != nil {
		t.Fatal(err)
	}
	unlock, err := filelock.Lock(context.Background(), filepath.Join(s.Root, e.ID, currentLockName))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Revoke(e.ID, e.Revision, e.SHA256) }()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("Revoke did not wait for the entry's lock (err = %v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Revoke after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Revoke never ran after the lock was released")
	}
	if got, err := s.Get(e.ID, e.Revision); err != nil || got.Status != StatusRevoked {
		t.Fatalf("entry = %+v, err = %v", got, err)
	}
}

// The wait for an entry's lock is bounded as a whole, the in-process part
// included: writers queued behind a wedged holder fail after one bound, not one
// bound each in turn.
func TestWritersBehindAWedgedHolderAllFailWithinOneBound(t *testing.T) {
	old := lockWait
	lockWait = 200 * time.Millisecond
	t.Cleanup(func() { lockWait = old })
	s := Store{Root: t.TempDir()}
	e, err := s.Put(catalogFixture(t, "1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(e.ID, e.Revision, e.SHA256); err != nil {
		t.Fatal(err)
	}
	root, err := s.open(false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	dir, err := openChild(root, e.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	release, err := s.lockCurrent(dir, e.ID) // the wedged holder
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	const writers = 6
	errs := make(chan error, writers)
	start := time.Now()
	for range writers {
		go func() { errs <- s.Revoke(e.ID, e.Revision, e.SHA256) }()
	}
	for range writers {
		select {
		case err := <-errs:
			if err == nil || !strings.Contains(err.Error(), "waiting for another writer") {
				t.Fatalf("a writer behind a wedged holder err = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a writer behind a wedged holder is still waiting: the in-process wait is unbounded")
		}
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("%d writers took %s to fail, want about one bound (%s)", writers, took, lockWait)
	}
	if got, err := s.Get(e.ID, e.Revision); err != nil || got.Status != StatusApproved {
		t.Fatalf("a refused writer changed the entry: %+v, %v", got, err)
	}
}

const writerHelperEnv = "MANVI_CATALOG_WRITER_HELPER"

// TestMain lets this test binary be the second process: with the helper
// environment set it rewrites the stability of one entry in a loop until its
// parent closes its stdin, instead of running the tests. (A helper test function
// would show as a permanent skip in the gate's skip report.)
func TestMain(m *testing.M) {
	if spec := os.Getenv(writerHelperEnv); spec != "" {
		os.Exit(stabilityWriter(spec))
	}
	os.Exit(m.Run())
}

func stabilityWriter(spec string) int {
	var in struct{ Root, ID, Revision, SHA string }
	if err := json.Unmarshal([]byte(spec), &in); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	s := Store{Root: in.Root}
	stop := make(chan struct{})
	go func() {
		_, _ = os.Stdin.Read(make([]byte, 1))
		close(stop)
	}()
	for n := 0; ; n++ {
		select {
		case <-stop:
			return 0
		default:
		}
		if err := s.SetStability(in.ID, in.Revision, in.SHA, Stability{SuccessRate: 1, FirstAttemptRate: 1, Runs: 1 + n%7}); err != nil {
			fmt.Fprintln(os.Stderr, "SetStability:", err)
			return 1
		}
	}
}

// The same lost update across two processes, which no in-process mutex can stop.
func TestRevokeHoldsAgainstAnotherProcessWritingStability(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("re-executes the test binary with POSIX plumbing")
	}
	s := Store{Root: t.TempDir()}
	e, err := s.Put(catalogFixture(t, "1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(e.ID, e.Revision, e.SHA256); err != nil {
		t.Fatal(err)
	}
	spec, err := json.Marshal(struct{ Root, ID, Revision, SHA string }{s.Root, e.ID, e.Revision, e.SHA256})
	if err != nil {
		t.Fatal(err)
	}
	// Cancelled only after the helper has been asked to stop and has exited, so the
	// context is a backstop for a helper that ignores its stdin closing.
	helperCtx, killHelper := context.WithCancel(context.Background())
	defer killHelper()
	cmd := exec.CommandContext(helperCtx, os.Args[0]) // #nosec G204 G702 -- re-executes this test binary; the environment selects the helper role
	cmd.Env = append(os.Environ(), writerHelperEnv+"="+string(spec))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var helperOut safeBuffer
	cmd.Stdout, cmd.Stderr = &helperOut, &helperOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stopHelper := func() {
		if stopped {
			return
		}
		stopped = true
		_ = stdin.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("the writer process failed: %v\n%s", err, helperOut.String())
		}
	}
	t.Cleanup(stopHelper)
	// Let the helper start writing before the first revocation.
	time.Sleep(300 * time.Millisecond)

	const rounds = 60
	lost := 0
	for i := 0; i < rounds; i++ {
		if err := s.Approve(e.ID, e.Revision, e.SHA256); err != nil {
			t.Fatal(err)
		}
		if err := s.Revoke(e.ID, e.Revision, e.SHA256); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(e.ID, e.Revision)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusRevoked {
			lost++
		}
	}
	stopHelper()
	if lost > 0 {
		t.Fatalf("Revoke succeeded and the entry was not revoked afterwards in %d of %d rounds", lost, rounds)
	}
}

type safeBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.b = append(b.b, p...)
	return len(p), nil
}
func (b *safeBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.b) }

// SetStability changes the stability field and nothing else: the status, the
// promotion flag and fields this build does not know stay as their writer left
// them.
func TestSetStabilityWritesOnlyTheStabilityField(t *testing.T) {
	s := Store{Root: t.TempDir()}
	e, err := s.Put(catalogFixture(t, "1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(e.ID, e.Revision, e.SHA256); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(e.ID, e.Revision, e.SHA256); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Root, e.ID, "current.json")
	before := readFields(t, path)
	before["reviewed_by"] = json.RawMessage(`"someone"`)
	raw, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStability(e.ID, e.Revision, e.SHA256, Stability{SuccessRate: 0.5, FirstAttemptRate: 0.25, Runs: 4}); err != nil {
		t.Fatal(err)
	}
	after := readFields(t, path)
	for k, v := range before {
		if k == "stability" {
			continue
		}
		if string(after[k]) != string(v) {
			t.Errorf("field %q changed from %s to %s", k, v, after[k])
		}
	}
	var st Stability
	if err := json.Unmarshal(after["stability"], &st); err != nil || st.Runs != 4 || st.SuccessRate != 0.5 {
		t.Fatalf("stability = %s, err = %v", after["stability"], err)
	}
	if got, err := s.Get(e.ID, e.Revision); err != nil || got.Status != StatusRevoked {
		t.Fatalf("entry = %+v, err = %v", got, err)
	}
}

func readFields(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(path) // #nosec G304 -- a file under this test's own t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
