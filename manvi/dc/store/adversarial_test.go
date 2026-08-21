package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestColdStartContentionYieldsNoLockErrors covers the case its neighbour below
// deliberately excludes. That test creates the schema first, "so the race is
// over the lease, not over migration" — which means nothing in this package
// exercised the migration race itself, and a real one lived there.
//
// Opening a database that does not exist yet has to convert it to WAL, and that
// conversion takes an exclusive lock which SQLite does *not* run the busy
// handler for: every process but the winner was handed SQLITE_BUSY instantly,
// regardless of the store's five-second busy timeout. A multi-agent run starts
// exactly this way — several builders exec the store against a database none of
// them has created yet — so "database is locked" reached agents that had no way
// to act on it.
//
// Every racer here takes a *distinct* task, which is what makes the test sharp:
// contention over a lease is impossible by construction, so a *Conflict would be
// as wrong as a lock error and any failure at all is the bug. The database is
// left uncreated on purpose. Do not add a warm-up call.
func TestColdStartContentionYieldsNoLockErrors(t *testing.T) {
	bin := binary(t)
	dir := t.TempDir()

	const racers = 24
	// A single round is close to a coin toss: the losing process has to reach
	// the pragma inside the window the winner holds the exclusive lock, and
	// whether it does depends on how the machine happens to schedule 24
	// cold-starting processes. Rounds are what make this worth having — each
	// races a database that has never existed, and a reintroduced regression
	// has to survive every one of them to go unnoticed.
	const rounds = 8

	for round := 0; round < rounds; round++ {
		db := filepath.Join(dir, fmt.Sprintf("state-%d.sqlite", round))

		var wg sync.WaitGroup
		results := make(chan error, racers)
		start := make(chan struct{})

		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				c := New(bin, db)
				c.Timeout = 30 * time.Second
				<-start
				_, err := c.Acquire(context.Background(), AcquireRequest{
					TaskID: fmt.Sprintf("TASK-COLD-%d", i), Owner: fmt.Sprintf("builder-%d", i),
					TTL: time.Minute,
				})
				results <- err
			}(i)
		}
		close(start)
		wg.Wait()
		close(results)

		for err := range results {
			if err == nil {
				continue
			}
			// Name the specific regression when it is the one that came back,
			// so a future reader does not have to rediscover which statement
			// was busy.
			if strings.Contains(err.Error(), "database is locked") {
				t.Fatalf("round %d: cold-start acquire returned a raw lock error: %v\n"+
					"a racing WAL conversion must be waited out, not reported to the caller",
					round, err)
			}
			t.Fatalf("round %d: cold-start acquire on an uncontended task failed: %v", round, err)
		}
	}
}

// TestConcurrentAcquireElectsExactlyOneHolder is the mutual-exclusion claim the
// whole design rests on, tested the only way that proves anything: many real
// processes racing for one task against one database file. A check-then-insert
// implementation passes a sequential test and fails this one.
func TestConcurrentAcquireElectsExactlyOneHolder(t *testing.T) {
	bin := binary(t)
	db := filepath.Join(t.TempDir(), "state.sqlite")

	// Create the schema once so the race is over the lease, not over migration.
	if err := New(bin, db).Available(context.Background()); err != nil {
		t.Fatalf("prepare store: %v", err)
	}

	const racers = 24
	var wg sync.WaitGroup
	results := make(chan error, racers)
	tokens := make(chan string, racers)
	start := make(chan struct{})

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := New(bin, db)
			c.Timeout = 30 * time.Second
			<-start
			lease, err := c.Acquire(context.Background(), AcquireRequest{
				TaskID: "TASK-RACE", Owner: fmt.Sprintf("builder-%d", i), TTL: time.Minute,
			})
			if err != nil {
				results <- err
				return
			}
			tokens <- lease.Token
			results <- nil
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(tokens)

	winners := 0
	for err := range results {
		switch e := err.(type) {
		case nil:
			winners++
		case *Conflict:
			if e.Holder == "" {
				t.Error("a conflict must name the holder so the caller can report who has it")
			}
		default:
			// A busy database is a legitimate transient under this much
			// contention, but it must surface as an error, never as a lease.
			if !strings.Contains(err.Error(), "database is locked") && !strings.Contains(err.Error(), "busy") {
				t.Errorf("unexpected acquire error: %v", err)
			}
		}
	}
	if winners != 1 {
		t.Fatalf("%d racers acquired the same task, want exactly 1", winners)
	}
	if len(tokens) != 1 {
		t.Fatalf("%d tokens issued, want exactly 1", len(tokens))
	}
}

// TestUnreachableStoreNeverReportsAValidLease walks every way the subprocess
// can fail and asserts none of them produce a permissive answer. This is the
// governing invariant at its most consequential: Valid() gates every write.
func TestUnreachableStoreNeverReportsAValidLease(t *testing.T) {
	dir := t.TempDir()

	notThere := New(filepath.Join(dir, "no-such-binary"), filepath.Join(dir, "state.sqlite"))

	notAnExecutable := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(notAnExecutable, []byte("not a program"), 0o644); err != nil {
		t.Fatal(err)
	}

	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	garbage := write("garbage.sh", "#!/bin/sh\necho 'this is not json'\n")
	lies := write("lies.sh", "#!/bin/sh\necho '{\"ok\":true}'\n")
	silent := write("silent.sh", "#!/bin/sh\nexit 0\n")
	hang := write("hang.sh", "#!/bin/sh\nsleep 60\n")
	flood := write("flood.sh", "#!/bin/sh\nexec yes '{\"ok\":true,\"code\":\"valid\"}'\n")

	cases := []struct {
		name   string
		client *Client
	}{
		{"missing binary", notThere},
		{"not an executable", New(notAnExecutable, filepath.Join(dir, "state.sqlite"))},
		{"non-JSON output", New(garbage, filepath.Join(dir, "state.sqlite"))},
		{"JSON with no lease code", New(lies, filepath.Join(dir, "state.sqlite"))},
		{"no output at all", New(silent, filepath.Join(dir, "state.sqlite"))},
		{"no database configured", New(notThere.Binary, "")},
		{"no binary configured", New("", filepath.Join(dir, "state.sqlite"))},
	}
	for _, tc := range cases {
		valid, err := tc.client.Valid(context.Background(), "TASK-1", "token")
		if valid {
			t.Errorf("%s: Valid returned true", tc.name)
		}
		if err == nil {
			t.Errorf("%s: Valid returned (false, nil) — an unanswered check must be an error, not a quiet no", tc.name)
		}
		if err := tc.client.Available(context.Background()); err == nil {
			t.Errorf("%s: Available reported healthy", tc.name)
		}
	}

	t.Run("a hung store times out rather than blocking the turn", func(t *testing.T) {
		c := New(hang, filepath.Join(dir, "state.sqlite"))
		c.Timeout = 300 * time.Millisecond
		done := make(chan struct{})
		go func() {
			defer close(done)
			if valid, err := c.Valid(context.Background(), "T", "tok"); valid || err == nil {
				t.Errorf("hung store: valid=%v err=%v", valid, err)
			}
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("Valid did not return; the timeout is not bounding the subprocess")
		}
	})

	t.Run("an unbounded store does not exhaust memory", func(t *testing.T) {
		c := New(flood, filepath.Join(dir, "state.sqlite"))
		c.Timeout = 2 * time.Second
		done := make(chan struct{})
		go func() {
			defer close(done)
			if valid, _ := c.Valid(context.Background(), "T", "tok"); valid {
				t.Error("a flooding store reported a valid lease")
			}
		}()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatal("Valid never returned against an infinite stdout")
		}
	})
}

// TestEmptyIdentifiersAreRefused: an empty task id or token is a caller bug,
// and answering it as "no such task" hides that bug behind a plausible result.
func TestEmptyIdentifiersAreRefused(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	if _, err := c.Acquire(ctx, AcquireRequest{TaskID: "", Owner: "b", TTL: time.Minute}); err == nil {
		t.Error("acquiring a lease on an empty task id succeeded")
	}
	if _, err := c.Acquire(ctx, AcquireRequest{TaskID: "T", Owner: "", TTL: time.Minute}); err == nil {
		t.Error("acquiring a lease with an empty owner succeeded")
	}
	if valid, err := c.Valid(ctx, "TASK-1", ""); valid && err == nil {
		t.Error("an empty token validated")
	}
}

// TestFlagLikeIdentifiersDoNotBecomeFlags is the argument-injection check. Task
// ids reach this boundary from a model, so a value that looks like a flag must
// be carried as data.
func TestFlagLikeIdentifiersDoNotBecomeFlags(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	for _, hostile := range []string{"--force", "--db", "-x", "--task"} {
		lease, err := c.Acquire(ctx, AcquireRequest{TaskID: hostile, Owner: "builder", TTL: time.Minute})
		if err != nil {
			continue // refusing outright is an acceptable answer
		}
		if lease.TaskID != hostile {
			t.Errorf("task id %q round-tripped as %q — it was parsed, not carried", hostile, lease.TaskID)
		}
		if _, err := c.Release(ctx, hostile, lease.Token); err != nil {
			t.Errorf("release %q: %v", hostile, err)
		}
	}
}

// TestUnknownFlagIsRefused: a mistyped flag that is silently ignored is the
// same failure the flag registry exists to prevent — the setting appears to
// apply and does nothing.
func TestUnknownFlagIsRefused(t *testing.T) {
	c := client(t)
	out, err := c.run(context.Background(), "list", "--not-a-real-flag", "x")
	if err == nil {
		t.Fatalf("an unknown flag was accepted (response %+v)", out)
	}
}
