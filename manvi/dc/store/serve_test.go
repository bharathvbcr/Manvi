package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// framedReply renders one reply the way a serving store writes it.
func framedReply(status, body string) string {
	return fmt.Sprintf("%s\n%d\n%s", status, len(body), body)
}

// fakeStore writes an executable that emits exactly these bytes on stdout and
// exits, ignoring whatever it is asked.
//
// The bytes are written to a file and cat'd rather than embedded in the script,
// so a reply containing quotes, braces or newlines reaches the client
// unmangled — which is the point of several of the cases below.
func fakeStore(t *testing.T, stdout string, preamble ...string) string {
	t.Helper()
	dir := t.TempDir()
	payload := filepath.Join(dir, "reply.bin")
	if err := os.WriteFile(payload, []byte(stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "fake-store.sh")
	body := "#!/bin/sh\n" + strings.Join(preamble, "\n") + "\ncat " + payload + "\n"
	// #nosec G306 -- this writes a shell script the test then execs, so the
	// owner execute bit is the point; no mode at or below 0600 would work.
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

func fakeClient(t *testing.T, stdout string, preamble ...string) *Client {
	t.Helper()
	c := New(fakeStore(t, stdout, preamble...), filepath.Join(t.TempDir(), "state.sqlite"))
	t.Cleanup(c.Close)
	return c
}

// assertNoSlotsHeld checks that every borrowed slot was given back.
//
// The semaphore's length is the number of slots currently held, so once every
// call has returned this must be zero. It is asserted directly because a leak
// is otherwise invisible until the pool is exhausted — at which point the
// symptom is a harness that stops making store calls, with no error naming the
// path that lost the slot.
func assertNoSlotsHeld(t *testing.T, c *Client) {
	t.Helper()
	if held := len(c.pool.sem); held != 0 {
		t.Fatalf("%d borrowed slot(s) were never released", held)
	}
}

// TestAServedFailureIsNotReadAsAnExpiredLease is the reason a served reply
// carries a status at all.
//
// The one-shot path reports a store failure twice: as JSON on stdout and as a
// non-zero exit, and decodeReply uses the exit code to tell a fault from an
// answer. Four commands treat ok:false as a real answer, and Renew is the
// sharpest of them — it reads ok:false as "the lease had already expired" and
// returns no error at all. A served transport with no equivalent of the exit
// code would therefore have reported a broken store to the turn as an expired
// lease: the agent would have been told something false about its own state and
// carried on, checking out again against a store that was not working.
func TestAServedFailureIsNotReadAsAnExpiredLease(t *testing.T) {
	c := fakeClient(t, framedReply("err", `{"ok":false,"error":"the database is locked"}`))

	lease, err := c.Renew(context.Background(), "TASK-1", "token", time.Minute)
	if err == nil {
		t.Fatal("a store that failed while renewing was reported as an expired lease")
	}
	if lease != nil {
		t.Fatalf("a failed renewal produced a lease: %+v", lease)
	}
	if !strings.Contains(err.Error(), "the database is locked") {
		t.Fatalf("the failure does not carry what the store said: %v", err)
	}
}

// TestAServedConflictIsStillAnOutcome is the other half of that distinction.
//
// Contention is the expected case when two builders run, and the one-shot path
// deliberately exits zero on it so that a caller checking the exit code does
// not read a busy task as an outage. The served status has to preserve that, or
// every lease conflict becomes a store fault.
func TestAServedConflictIsStillAnOutcome(t *testing.T) {
	c := fakeClient(t, framedReply("ok",
		`{"ok":false,"code":"lease_held_by_other","task_id":"TASK-1","holder":"builder-2"}`))

	_, err := c.Acquire(context.Background(), AcquireRequest{
		TaskID: "TASK-1", Owner: "builder-1", TTL: time.Minute,
	})
	var conflict *Conflict
	if !errors.As(err, &conflict) {
		t.Fatalf("a held task was not reported as contention: %v", err)
	}
	if conflict.Holder != "builder-2" {
		t.Fatalf("the conflict lost the holder: %+v", conflict)
	}
}

// TestABinaryWithoutServeIsAnsweredOneShot keeps the transport an optimisation
// rather than a new requirement.
//
// Binaries are discovered from MANVI_STORE_BINARY, from PATH and from sibling
// build output, so the store on a machine is not necessarily the one built from
// this tree. One that predates serve refuses the command from argv and exits
// before reading a byte of the request, which is why falling back is safe: no
// command ran. The refusal is longer than a status header is allowed to be, so
// it is recognised by its first byte — getting that wrong reported a working
// store as unreachable.
func TestABinaryWithoutServeIsAnsweredOneShot(t *testing.T) {
	// No framing: the whole answer is the JSON object an older store prints,
	// whatever it was asked.
	c := fakeClient(t, `{"ok":true,"store":"dc-store","schema_version":1,"exclusion_index":"verified","active_leases":0}`)

	if err := c.Available(context.Background()); err != nil {
		t.Fatalf("a store that does not speak serve was reported unavailable: %v", err)
	}
	if !c.oneShot.Load() {
		t.Fatal("the client did not record that this binary needs the one-shot path")
	}
}

// TestTheServedChildIsReusedAcrossCalls is the whole point of the transport.
//
// Before this, every store call was its own process: ~3.9ms per call measured
// from here on an idle machine, of which ~2.1ms was fork and exec and the rest
// was opening SQLite and verifying its schema — on the path every write-gate
// Diagnose and every Acquire goes through. A regression that quietly went back
// to a process per call would not fail any other test in this package; they
// would all still pass, slowly.
//
// Reuse is asserted directly rather than through a latency bound, because a
// timing gate on a shared machine measures the machine: the same comparison
// run back to back on a loaded one moved between 19x and 104x.
func TestTheServedChildIsReusedAcrossCalls(t *testing.T) {
	c := client(t)
	t.Cleanup(c.Close)
	ctx := context.Background()

	lease, err := c.Acquire(ctx, AcquireRequest{TaskID: "TASK-1", Owner: "builder", TTL: time.Hour})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	first := idleChild(t, c)
	for range 20 {
		if _, err := c.Valid(ctx, "TASK-1", lease.Token); err != nil {
			t.Fatalf("diagnose: %v", err)
		}
	}
	if got := idleChild(t, c); got != first {
		t.Fatal("the store process was replaced between calls; the fork per call is back")
	}
	c.pool.mu.Lock()
	idle := len(c.pool.idle)
	c.pool.mu.Unlock()
	if idle != 1 {
		t.Fatalf("a caller with one call in flight left %d store processes idle, not 1", idle)
	}
}

func idleChild(t *testing.T, c *Client) *servedChild {
	t.Helper()
	c.pool.mu.Lock()
	defer c.pool.mu.Unlock()
	if len(c.pool.idle) == 0 {
		t.Fatal("no store process was kept between calls")
	}
	return c.pool.idle[len(c.pool.idle)-1]
}

// TestAValueContainingNewlinesSurvivesTheWire is why the frame carries lengths
// instead of splitting on newlines.
//
// Values crossing this boundary are not line-shaped. An owner is free text
// reaching the harness from the environment, and a request framed by newlines
// would have been cut in half by one — the front read as a whole request and
// the tail read as the next one, which is a command nobody sent. This drives
// the real binary with such a value and reads it back.
func TestAValueContainingNewlinesSurvivesTheWire(t *testing.T) {
	c := client(t)
	t.Cleanup(c.Close)
	ctx := context.Background()

	owner := "builder-1\n7\nacquire\nnot-a-command"
	lease, err := c.Acquire(ctx, AcquireRequest{TaskID: "TASK-1", Owner: owner, TTL: time.Hour})
	if err != nil {
		t.Fatalf("acquire with a multi-line owner: %v", err)
	}
	if lease.Owner != owner {
		t.Fatalf("the owner changed crossing the wire:\n got %q\nwant %q", lease.Owner, owner)
	}

	// And the session is still coherent afterwards: if the newline had been
	// read as a frame boundary, the tail would be sitting in the stream waiting
	// to be answered as the next request.
	active, err := c.Active(ctx, "TASK-1")
	if err != nil {
		t.Fatalf("the connection did not survive a multi-line value: %v", err)
	}
	if active == nil || active.Owner != owner {
		t.Fatalf("the following call read a desynchronised stream: %+v", active)
	}
}

// TestAChildThatMissedItsDeadlineIsNotReused is the desync guard.
//
// A timed-out request's reply may still be in flight. Returning that child to
// the pool would hand the *next* caller the previous caller's document — a
// diagnose answered with someone else's lease code, and nothing about it would
// look like an error. The child is destroyed instead, so the next call starts a
// fresh one.
func TestAChildThatMissedItsDeadlineIsNotReused(t *testing.T) {
	c := fakeClient(t, framedReply("ok", `{"ok":true,"released":true}`), "sleep 30")
	c.Timeout = 150 * time.Millisecond

	// Once per slot and one more, so a slot lost on this path exhausts the pool
	// rather than merely shrinking it.
	for range serveMaxChildren + 1 {
		if _, err := c.Release(context.Background(), "TASK-1", "token"); err == nil {
			t.Fatal("a store that never answered was reported as a successful release")
		}
	}

	c.pool.mu.Lock()
	idle := len(c.pool.idle)
	c.pool.mu.Unlock()
	if idle != 0 {
		t.Fatalf("a child that missed its deadline was kept for the next caller (%d idle)", idle)
	}
	assertNoSlotsHeld(t, c)
}

// TestAnOverSizedServedReplyIsRefused keeps the served path's bound the same
// bound the one-shot path has.
//
// A reply past the cap is refused rather than read, because reading it is how a
// broken store turns into an out-of-memory kill of the harness. The refusal
// names the cap, and the connection is not reused: the document was left
// unread, so the stream position is no longer known.
func TestAnOverSizedServedReplyIsRefused(t *testing.T) {
	c := fakeClient(t, fmt.Sprintf("ok\n%d\n", maxOutput+1))

	_, err := c.Task(context.Background(), "TASK-1")
	if err == nil {
		t.Fatal("a reply over the cap was accepted")
	}
	if !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("the refusal does not name the bound it hit: %v", err)
	}
	c.pool.mu.Lock()
	idle := len(c.pool.idle)
	c.pool.mu.Unlock()
	if idle != 0 {
		t.Fatalf("a connection left mid-document was kept for the next caller (%d idle)", idle)
	}
	assertNoSlotsHeld(t, c)
}

// TestACancelledCallReleasesItsSlot is the pool's own failure mode.
//
// Every path out of a borrowed slot has to give it back: the exchange that
// broke, the child that missed its deadline, and the start the caller gave up
// waiting for. Miss one and the pool shrinks by a slot each time it happens,
// silently, until the client wedges on a semaphore that will never be posted —
// a harness that stops making store calls at all, with no error to explain it.
//
// The start is on its own goroutine for the same reason the one-shot path runs
// under RunBounded: exec.Cmd.Start can block in the kernel, and every other
// bound on that child only begins counting once Start has returned.
func TestACancelledCallReleasesItsSlot(t *testing.T) {
	c := client(t)
	t.Cleanup(c.Close)

	lease, err := c.Acquire(context.Background(), AcquireRequest{
		TaskID: "TASK-1", Owner: "builder", TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Comfortably more cancelled calls than the pool has slots, so a slot lost
	// on any of them exhausts it.
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	for range serveMaxChildren * 3 {
		if _, err := c.Valid(cancelled, "TASK-1", lease.Token); err == nil {
			t.Fatal("a cancelled call reported a lease as valid")
		}
	}

	// If any slot leaked, this blocks until its own bound expires and fails.
	ok, err := c.Valid(context.Background(), "TASK-1", lease.Token)
	if err != nil {
		t.Fatalf("the pool did not recover from cancelled calls: %v", err)
	}
	if !ok {
		t.Fatal("the lease stopped being valid")
	}
	assertNoSlotsHeld(t, c)
}
