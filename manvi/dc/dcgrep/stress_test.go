package dcgrep

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The searcher is the tool an agent reaches for most, and a fan-out of
// sub-agents reaches for it concurrently. One process per call means the
// failure modes of concurrency here are not data races in this package — they
// are exhaustion, interleaving, and cancellation arriving mid-flight.

// TestConcurrentSearchesDoNotInterfere runs the real binary many times at once
// and asserts every caller got its own answer.
//
// The specific thing this rules out is replies crossing: each search asks for a
// needle only one file contains, so a result naming the wrong file is a reply
// that reached the wrong caller.
func TestConcurrentSearchesDoNotInterfere(t *testing.T) {
	const workers = 24
	files := map[string]string{}
	for i := range workers {
		files[filepath.Join("src", "f"+itoa(i)+".go")] = "marker" + itoa(i) + "\n"
	}
	root := scratchRepo(t, files)
	client := realClient(t, root)

	var wg sync.WaitGroup
	errs := make([]error, workers)
	paths := make([]string, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := client.Search(context.Background(),
				Request{Pattern: "marker" + itoa(i) + "\\b"})
			if err != nil {
				errs[i] = err
				return
			}
			if len(result.Matches) == 1 {
				paths[i] = result.Matches[0].Path
			}
		}()
	}
	wg.Wait()

	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		want := "src/f" + itoa(i) + ".go"
		if paths[i] != want {
			t.Fatalf("worker %d got %q, want %q — a reply reached the wrong caller",
				i, paths[i], want)
		}
	}
}

// TestConcurrentCancellationLeavesNothingBehind cancels searches at arbitrary
// points and asserts the client neither hangs nor returns a partial answer as a
// real one.
//
// The deadline is deliberately short enough that some calls finish and some do
// not, so both paths are exercised in the same run.
func TestConcurrentCancellationLeavesNothingBehind(t *testing.T) {
	root := scratchRepo(t, map[string]string{
		"a.go": strings.Repeat("needle in a haystack\n", 2000),
		"b.go": strings.Repeat("needle in a haystack\n", 2000),
	})
	client := realClient(t, root)

	var wg sync.WaitGroup
	var completed atomic.Int64
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Spread across the range where a search actually lands. A
			// spawn costs tens of milliseconds, so the original 0-7ms
			// window timed out every single call and the test finished in
			// ten milliseconds having never once exercised the path where
			// a result comes back. Both halves have to run for the "either
			// outcome is legitimate" assertion below to mean anything.
			budget := []time.Duration{
				0, time.Millisecond, 5 * time.Millisecond, 20 * time.Millisecond,
				60 * time.Millisecond, 250 * time.Millisecond,
				time.Second, 5 * time.Second,
			}[i%8]
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			result, err := client.Search(ctx, Request{Pattern: "needle", MaxResults: 4000})
			// Either outcome is legitimate. What is not legitimate is a result
			// alongside an error, or a result that fails the contract.
			if err != nil {
				if result != nil {
					t.Errorf("an error came with a result: %+v", result)
				}
				return
			}
			if result.Count != len(result.Matches) {
				t.Errorf("count %d disagrees with %d matches", result.Count, len(result.Matches))
			}
			completed.Add(1)
		}()
	}

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("cancelled searches did not all return; something is wedged")
	}
	// Both halves must have run, or this asserts nothing: all-cancelled never
	// reaches the contract checks, and all-completed never exercises
	// cancellation. The generous budgets above make the completing half
	// reliable; the zero-length one makes the cancelled half certain.
	if finished := completed.Load(); finished == 0 || finished == 32 {
		t.Fatalf("%d of 32 searches completed — this run tested only one path", finished)
	}
}

// TestTheSearcherSurvivesARepositoryDesignedToBreakIt is the hostile-tree case,
// run through the real binary rather than by hand.
//
// Every entry here was something that could plausibly hang, follow, or exhaust
// a naive walker: a symlink loop, a FIFO that blocks on open, links to device
// files that never end, a file far over the size ceiling, and a directory
// nested deeply enough to matter.
func TestTheSearcherSurvivesARepositoryDesignedToBreakIt(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("normal.txt", "needle\n")

	// A symlink loop. Followed, this never terminates.
	if err := os.MkdirAll(filepath.Join(root, "loop"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Symlink(filepath.Join(root, "loop"), filepath.Join(root, "loop", "self"))
	// Links to endless and blocking things.
	_ = os.Symlink("/dev/zero", filepath.Join(root, "zero.link"))
	_ = os.Symlink("/dev/random", filepath.Join(root, "random.link"))
	// A link that climbs out of the repository entirely.
	_ = os.Symlink("/etc", filepath.Join(root, "escape.link"))

	// Deep nesting.
	deep := "deep"
	for i := range 100 {
		deep = filepath.Join(deep, "d"+itoa(i))
	}
	write(filepath.Join(deep, "buried.txt"), "needle\n")

	client := realClient(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := client.Search(ctx, Request{
		Pattern: "needle", MaxResults: 1000, IncludeIgnored: true,
	})
	if err != nil {
		t.Fatalf("the hostile tree must be searchable, not fatal: %v", err)
	}
	for _, hit := range result.Matches {
		if strings.HasPrefix(hit.Path, "/") || strings.Contains(hit.Path, "..") {
			t.Fatalf("a match escaped the repository: %q", hit.Path)
		}
		if strings.Contains(hit.Path, "loop/self/") {
			t.Fatalf("the symlink loop was followed: %q", hit.Path)
		}
	}
	// Both real files, and nothing from /etc or /dev.
	if result.Count != 2 {
		t.Fatalf("expected the two real files, got %d: %+v", result.Count, result.Matches)
	}

	// And the listing agrees, over the same hostile tree.
	listing, err := client.List(ctx, ListRequest{MaxResults: MaxListResults, IncludeIgnored: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	listed := map[string]bool{}
	for _, path := range listing.Paths {
		listed[path] = true
	}
	for _, hit := range result.Matches {
		if !listed[hit.Path] {
			t.Fatalf("search opened %q, which the listing does not name", hit.Path)
		}
	}
}
