package devmap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The producer is a binary built from another repository and resolved from
// PATH. Everything here treats it as hostile input rather than as a component:
// it can hang, die, fork, lie about its own counts, or answer in a shape this
// build has never seen. None of that may produce an answer, and none of it may
// hold a turn open forever.

// binary writes an arbitrary script as the devmap stand-in.
func binary(t *testing.T, script string) *Client {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "devmap")
	if err := os.WriteFile(path, []byte(clapHelp(script)), 0o755); err != nil {
		t.Fatal(err)
	}
	c := New(path, dir)
	c.Timeout = 5 * time.Second
	return c
}

// TestAMisbehavingProducerNeverProducesAnAnswer walks the ways the binary can
// answer badly. The assertion is the same for every one of them: an error, not
// a Status — because a zero-valued Status is a fully usable-looking answer that
// says the repository has no symbols in it.
func TestAMisbehavingProducerNeverProducesAnAnswer(t *testing.T) {
	cases := []struct {
		name   string
		script string
		// mentions is a fragment the failure must contain, so the operator is
		// sent to the right problem rather than to a generic exec error.
		mentions string
	}{
		{"exits zero saying nothing", "#!/bin/sh\nexit 0\n", "unparseable"},
		{"exits non-zero with a reason", "#!/bin/sh\necho 'store locked' >&2\nexit 3\n", "store locked"},
		{"answers null", "#!/bin/sh\necho null\n", "unavailable"},
		{"answers an array", "#!/bin/sh\necho '[]'\n", "unparseable"},
		{"answers valid JSON then garbage", "#!/bin/sh\necho '{\"generation_id\":1} oops'\n", "unparseable"},
		{"answers a number too large for the field",
			"#!/bin/sh\necho '{\"generation_id\":1,\"node_count\":99999999999999999999999}'\n", "unparseable"},
		{"answers a count as a string",
			"#!/bin/sh\necho '{\"generation_id\":1,\"node_count\":\"many\"}'\n", "unparseable"},
		{"reports a negative symbol count",
			"#!/bin/sh\necho '{\"generation_id\":4,\"node_count\":-5,\"is_fresh\":true}'\n", "unavailable"},
		{"reports no committed generation",
			"#!/bin/sh\necho '{\"generation_id\":0,\"node_count\":900,\"is_fresh\":true}'\n", "unavailable"},
		{"declares itself degraded",
			"#!/bin/sh\necho '{\"generation_id\":4,\"node_count\":9,\"degraded_reason\":\"partial resolve\"}'\n", "partial resolve"},
		// A non-executable binary now fails the capability probe before any
		// command runs, so the failure names the probe that caught it.
		{"is not executable at all", "", "could not answer its own --help"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := binary(t, c.script)
			if c.script == "" {
				if err := os.Chmod(client.Binary, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			_, err := client.Available(context.Background())
			if err == nil {
				t.Fatal("this producer must not yield a usable index")
			}
			if !strings.Contains(err.Error(), c.mentions) {
				t.Fatalf("failure must name the problem (%q), got %v", c.mentions, err)
			}
		})
	}
}

// TestAMissingBinaryAndAMissingRootAreNamedNotGuessed.
func TestAMissingBinaryAndAMissingRootAreNamedNotGuessed(t *testing.T) {
	t.Run("no binary configured", func(t *testing.T) {
		c := &Client{Root: t.TempDir()}
		if _, err := c.Status(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "no devmap binary") {
			t.Fatalf("want a named absence, got %v", err)
		}
	})
	t.Run("no root configured", func(t *testing.T) {
		c := &Client{Binary: "devmap"}
		if _, err := c.Status(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "no repository root") {
			t.Fatalf("want a named absence, got %v", err)
		}
	})
	t.Run("root does not exist", func(t *testing.T) {
		c := binary(t, "#!/bin/sh\necho '{}'\n")
		c.Root = filepath.Join(t.TempDir(), "gone")
		if _, err := c.Status(context.Background()); err == nil {
			t.Fatal("a root that is not there must fail rather than answer from somewhere else")
		}
	})
}

// TestAHangingProducerIsBoundedByItsTimeout. A query is local and indexed. One
// that does not return is a turn that does not end.
func TestAHangingProducerIsBoundedByItsTimeout(t *testing.T) {
	c := binary(t, "#!/bin/sh\nsleep 300\n")
	c.Timeout = 500 * time.Millisecond

	start := time.Now()
	_, err := c.Status(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a producer that never answers must not produce an answer")
	}
	if elapsed > 20*time.Second {
		t.Fatalf("the timeout did not bound the call: %s", elapsed)
	}
}

// TestAnOrphanedChildHoldingThePipeDoesNotHoldTheTurn.
//
// The classic subprocess hang, and the reason WaitDelay is set: the command
// exits, but a grandchild it forked still holds the write end of stdout, so
// Wait blocks on a pipe nobody will close. A repository indexer that shells out
// is exactly the sort of program that leaves one behind.
func TestAnOrphanedChildHoldingThePipeDoesNotHoldTheTurn(t *testing.T) {
	c := binary(t, "#!/bin/sh\nsleep 120 &\necho '{\"generation_id\":1,\"node_count\":5}'\n")
	c.Timeout = 30 * time.Second

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.Status(context.Background())
	}()

	select {
	case <-done:
	case <-time.After(25 * time.Second):
		t.Fatal("a forked child holding stdout held the call open past WaitDelay")
	}
}

// TestCancellationIsPromptAndReported. A cancelled turn must not wait out the
// timeout of a command nobody is listening to any more.
func TestCancellationIsPromptAndReported(t *testing.T) {
	c := binary(t, "#!/bin/sh\nsleep 300\n")
	c.Timeout = 5 * time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := c.Status(ctx)
	if err == nil {
		t.Fatal("a cancelled query must not answer")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("cancellation was not prompt: %s", elapsed)
	}
}

// TestConcurrentQueriesDoNotInterfere. Each tool call forks a process, and a
// turn can have several in flight. Run under -race this is also the check that
// the client holds no mutable state it shares between them.
func TestConcurrentQueriesDoNotInterfere(t *testing.T) {
	// forkSafeFake, not fake: two dozen simultaneous forks of `/bin/sh` under
	// -race is a combination the race runtime does not survive on macOS, and
	// this test is the only one that produces it. See racefake_test.go.
	c := forkSafeFake(t, map[string]string{
		"status": healthyStatus,
		"search": `{"items":[{"file_path":"a.go","symbol_name":"Alpha"}],"hidden":2}`,
		"deps":   `{"items":[{"source_file":"a.go","target_file":"b.go"}],"hidden":0}`,
		"dead":   `{"items":[{"file_path":"a.go","symbol_name":"Unused"}],"hidden":0}`,
	}, nil)

	const workers = 24
	var wg sync.WaitGroup
	errs := make(chan error, workers*3)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			r, err := c.Search(ctx, fmt.Sprintf("query-%d", i))
			if err != nil {
				errs <- err
			} else if len(r.Items) != 1 || r.Items[0].Name != "Alpha" || r.Hidden != 2 {
				errs <- fmt.Errorf("search %d got %+v", i, r)
			}
			if _, err := c.Deps(ctx, fmt.Sprintf("file-%d.go", i)); err != nil {
				errs <- err
			}
			if _, err := c.Dead(ctx); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestHostileStringsSurviveTheRoundTrip. Symbol names and paths come from the
// repository and reach a shell-adjacent boundary; none of them may be
// interpreted on the way through.
func TestHostileStringsSurviveTheRoundTrip(t *testing.T) {
	hostile := []string{
		"$(rm -rf /)",
		"`whoami`",
		"; echo pwned",
		"--budget=1",
		"-b",
		"a'b\"c",
		"emoji-🙈-name",
		"日本語シンボル",
		"newline\nin\vname",
		strings.Repeat("A", 8192),
		"",
	}
	c, argv := recording(t, map[string]string{
		"status": healthyStatus,
		"search": `{"items":[{"file_path":"a.go","symbol_name":"x"}],"hidden":0}`,
	})
	for _, q := range hostile {
		if _, err := c.Search(context.Background(), q); err != nil {
			t.Fatalf("query %q failed: %v", q, err)
		}
	}
	// Every query must appear verbatim as its own argument, after the fence.
	calls := invocations(t, argv)
	seen := map[string]bool{}
	for _, call := range calls {
		fenced := false
		for _, a := range call {
			if a == "--" {
				fenced = true
				continue
			}
			if fenced {
				seen[a] = true
			}
		}
	}
	for _, q := range hostile {
		// A query containing a newline arrives as several argv lines through
		// the recording fake's own format, so only its first line is checked.
		want := strings.SplitN(q, "\n", 2)[0]
		if !seen[want] {
			t.Errorf("query %q did not reach the command line intact as a positional", q)
		}
	}
}

// TestAResultThatIsAllPaddingIsRefused re-states assertShape's guarantee under
// volume: a producer answering with a thousand well-formed but empty records is
// the shape change that reads as "nothing found".
func TestAResultThatIsAllPaddingIsRefused(t *testing.T) {
	var items []string
	for i := 0; i < 1000; i++ {
		items = append(items, `{"filePath":"a.go","symbolName":"x"}`)
	}
	c := fake(t, map[string]string{
		"status": healthyStatus,
		"search": `{"items":[` + strings.Join(items, ",") + `],"hidden":0}`,
	})
	_, err := c.Search(context.Background(), "anything")
	if err == nil {
		t.Fatal("a thousand records carrying nothing this build understands is a changed contract")
	}
	if !strings.Contains(err.Error(), "response shape has changed") {
		t.Fatalf("the failure must name the contract, got %v", err)
	}
}

// TestBuildTimeoutsAreBoundedIndependently. A build's cost scales with the
// repository, so it carries its own bound — but it still has one.
func TestBuildTimeoutsAreBoundedIndependently(t *testing.T) {
	c := binary(t, "#!/bin/sh\nsleep 300\n")
	start := time.Now()
	_, err := c.Build(context.Background(), 500*time.Millisecond)
	if err == nil {
		t.Fatal("a build that never finishes must not report one")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("the build bound did not hold: %s", elapsed)
	}
}

// TestAZeroBuildTimeoutFallsBackRatherThanRunningForever guards the argument
// that would otherwise mean "no deadline".
func TestAZeroBuildTimeoutFallsBackRatherThanRunningForever(t *testing.T) {
	c := binary(t, "#!/bin/sh\necho '{\"files_indexed\":1,\"symbols\":1,\"edges\":1}'\n")
	report, err := c.Build(context.Background(), 0)
	if err != nil {
		t.Fatalf("a zero timeout must take the default, not fail: %v", err)
	}
	if report == nil {
		t.Fatal("no report")
	}
}

// TestNoticesFromAStreamWithNoNewlines. stderr is not line-oriented by
// contract; a producer can emit one very long line, and the reader must not
// assume otherwise.
func TestNoticesFromAStreamWithNoNewlines(t *testing.T) {
	n := readNotices(said{text: []byte(strings.Repeat("x", 200000))})
	if n.Clean() {
		t.Fatal("an unreadable 200KB line is not a clean build")
	}
	if len(n.Unrecognised) != 1 {
		t.Fatalf("a single line is one notice, got %d", len(n.Unrecognised))
	}
}

// TestErrCappedIsNotMistakenForAProducerFailure pins the ordering in decode:
// breaking the pipe is how a flood is stopped, so the broken pipe must never be
// what the operator is told about.
func TestErrCappedIsNotMistakenForAProducerFailure(t *testing.T) {
	c := binary(t, "#!/bin/sh\nwhile :; do printf 'BBBBBBBBBBBBBBBB'; done\n")
	c.maxOutput = 32 << 10
	_, err := c.Status(context.Background())
	if err == nil {
		t.Fatal("a flood must not answer")
	}
	if strings.Contains(err.Error(), "broken pipe") || strings.Contains(err.Error(), "signal:") {
		t.Fatalf("the bound that stopped it must be reported, not the way it was stopped: %v", err)
	}
	if errors.Is(err, errCapped) {
		t.Fatalf("the sentinel is internal; the message is what an operator reads: %v", err)
	}
}
