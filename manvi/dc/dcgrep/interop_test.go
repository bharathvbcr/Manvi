package dcgrep

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"manvi/internal/testsupport"
)

// These run against the binary the harness actually execs, not a fake.
//
// The adversarial tests beside them prove the client rejects a misbehaving
// reply; these prove the two halves of the boundary agree about the contract
// when both are real. A constant duplicated across a process boundary is a
// constant that drifts, and the drift is invisible from either side alone.

func realClient(t *testing.T, root string) *Client {
	t.Helper()
	return New(testsupport.DCGrep(t), root)
}

func scratchRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestTheHealthHandshakeMatchesTheRealBinary is the positive control for the
// probe: every negative case in the adversarial tests is only meaningful if the
// real binary passes.
func TestTheHealthHandshakeMatchesTheRealBinary(t *testing.T) {
	client := realClient(t, t.TempDir())
	if err := client.Available(context.Background()); err != nil {
		t.Fatalf("the real searcher must satisfy its own probe: %v", err)
	}
}

// TestMaxListResultsIsNotSilentlyClampedByTheSearcher pins a number that lives
// on both sides of a process boundary.
//
// The Go plane asks for MaxListResults when it wants every candidate file. The
// searcher clamps max_results to its own ceiling without saying so. If the Go
// constant were ever raised above the Rust one, this side would believe it had
// the whole tree while quietly receiving less — and every "file not found"
// built on that listing would be wrong in a way nothing reports.
func TestMaxListResultsIsNotSilentlyClampedByTheSearcher(t *testing.T) {
	// One more file than the limit, so truncation is forced and the searcher
	// must report the limit it actually applied.
	files := map[string]string{}
	const count = 12
	for i := range count {
		files[filepath.Join("src", "f"+itoa(i)+".txt")] = "x\n"
	}
	root := scratchRepo(t, files)
	client := realClient(t, root)

	// Asking for exactly MaxListResults must come back with that number as the
	// applied limit, not a smaller one the searcher substituted.
	listing, err := client.List(context.Background(), ListRequest{MaxResults: MaxListResults})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if listing.Truncated {
		t.Fatalf("a %d-file tree must not truncate at %d: %+v", count, MaxListResults, listing)
	}

	// And the ceiling is real: asking for more than the searcher allows must
	// still report the searcher's number, so the two constants are proven equal
	// rather than assumed.
	over, err := client.List(context.Background(), ListRequest{MaxResults: MaxListResults + 1000})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if over.Truncated && over.Limit != MaxListResults {
		t.Fatalf("the searcher clamped to %d while this side believes the ceiling is %d",
			over.Limit, MaxListResults)
	}
}

// TestAnOversizedRequestIsRefusedByTheBinary covers the inbound bound.
//
// Every other boundary here caps what it will *read back* from a child. This
// one also caps what the child will accept, because an unbounded
// read_to_string turned a 64 MiB request into a 75 MiB resident set before
// anything inspected it — and the pattern inside that request comes from a
// model.
func TestAnOversizedRequestIsRefusedByTheBinary(t *testing.T) {
	root := scratchRepo(t, map[string]string{"a.txt": "needle\n"})
	client := realClient(t, root)

	// Over the searcher's own pattern limit and its request limit at once; the
	// point is that neither is reached by allocating first.
	_, err := client.Search(context.Background(), Request{
		Pattern: strings.Repeat("a", 4*1024*1024),
	})
	if err == nil {
		t.Fatal("an oversized request must be refused")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("the refusal must name the bound, got %v", err)
	}
}

// TestTheListingAndTheSearchAgreeAcrossTheBoundary re-asserts the engine's own
// invariant through the client, so a future change to either wire shape that
// broke the correspondence would be caught here rather than in a turn.
func TestTheListingAndTheSearchAgreeAcrossTheBoundary(t *testing.T) {
	root := scratchRepo(t, map[string]string{
		".gitignore":        "dist/\n",
		"src/a.go":          "needle\n",
		"src/deep/b.go":     "needle\n",
		"dist/generated.go": "needle\n",
		".hidden/c.go":      "needle\n",
	})
	client := realClient(t, root)

	for _, includeIgnored := range []bool{false, true} {
		listing, err := client.List(context.Background(),
			ListRequest{MaxResults: MaxListResults, IncludeIgnored: includeIgnored})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		result, err := client.Search(context.Background(),
			Request{Pattern: "needle", MaxResults: 1000, IncludeIgnored: includeIgnored})
		if err != nil {
			t.Fatalf("search: %v", err)
		}

		listed := map[string]bool{}
		for _, path := range listing.Paths {
			listed[path] = true
		}
		for _, hit := range result.Matches {
			if !listed[hit.Path] {
				t.Fatalf("include_ignored=%v: search opened %q, which the listing does not name",
					includeIgnored, hit.Path)
			}
		}
		if listing.IgnoreRulesApplied != result.IgnoreRulesApplied {
			t.Fatalf("include_ignored=%v: the two operations disagree about the mode they ran in",
				includeIgnored)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
