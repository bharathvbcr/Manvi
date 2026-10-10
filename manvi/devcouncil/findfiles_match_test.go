package devcouncil

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/fnmatch"
)

// matchCandidatesUnskipped is find_files' match loop as it stood before
// matchCandidates learned to skip calls, kept verbatim as the reference: every
// path is tried whole and then by base name, every time.
func matchCandidatesUnskipped(pattern string, paths []string, maxResults int) ([]string, bool) {
	matches := []string{}
	truncated := false
	for _, path := range paths {
		if len(matches) >= maxResults {
			truncated = true
			break
		}
		name := path
		if idx := strings.LastIndex(path, "/"); idx >= 0 {
			name = path[idx+1:]
		}
		if fnmatch.Match(pattern, path) || fnmatch.Match(pattern, name) {
			matches = append(matches, path)
		}
	}
	return matches, truncated
}

// fnmatchCorpus generates patterns and paths over an alphabet built from the
// characters that change how fnmatch reads a pattern — stars that cross '/',
// classes that can hold a '/', negation, ranges, unterminated brackets — so the
// skip rule is tried where it would be wrong if it were wrong anywhere.
func fnmatchCorpus(seed int64) (patterns, paths []string) {
	// #nosec G404 -- seeded fixture data: reproducing a run is the point, secrecy is not.
	rng := rand.New(rand.NewSource(seed))
	patternAlphabet := []string{"a", "b", "/", "*", "?", "[", "]", "!", "-", ".", "^", `\`, "é"}
	pathAlphabet := []string{"a", "b", "/", ".", "-", "]", "[", "!", "*", "é", " "}
	gen := func(alphabet []string, maxLen int) string {
		var b strings.Builder
		for n := rng.Intn(maxLen + 1); n > 0; n-- {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		return b.String()
	}
	patterns = []string{
		"*", "*.go", "**/*.go", "src/*.go", "*/a", "a/*", "/", "*/", "[/]a", "[!a]a",
		"[!/]*", "[a/]b", "a[/]b", "[]/]", "[!]/]", "[a-/]", "[/-a]", "[", "[/", "a[/",
		"a/[b]", "[b]/a", "?/?", "*[/]*", "[!b]*/a", "é/*", "*/é", "a\\/b", "[^/]a",
	}
	for i := 0; i < 2000; i++ {
		patterns = append(patterns, gen(patternAlphabet, 7))
	}
	paths = []string{"", "a", "a/b", "/", "a/", "/a", "b/a/a", "[/]", "a]/b", "é", "x/é", "!/a"}
	for i := 0; i < 400; i++ {
		paths = append(paths, gen(pathAlphabet, 9))
	}
	return patterns, paths
}

// TestMatchCandidatesSkipsOnlyWhatCannotMatch compares matchCandidates with the
// loop that never skipped, over generated patterns and paths and under result
// limits that stop it early, and then checks the skip rule's own claim
// directly: a pattern with a '/' before its first '[' matches no slash-free
// name.
func TestMatchCandidatesSkipsOnlyWhatCannotMatch(t *testing.T) {
	patterns, paths := fnmatchCorpus(1)

	skipped := 0
	for _, pattern := range patterns {
		for _, limit := range []int{1, 3, len(paths) + 1} {
			want, wantTrunc := matchCandidatesUnskipped(pattern, paths, limit)
			got, gotTrunc := matchCandidates(pattern, paths, limit)
			if !reflect.DeepEqual(got, want) || gotTrunc != wantTrunc {
				t.Fatalf("pattern %q limit %d: got %q (truncated %v), want %q (truncated %v)",
					pattern, limit, got, gotTrunc, want, wantTrunc)
			}
		}
		if !slashBeforeFirstBracket(pattern) {
			continue
		}
		skipped++
		for _, path := range paths {
			base := path[strings.LastIndex(path, "/")+1:]
			if fnmatch.Match(pattern, base) {
				t.Fatalf("pattern %q matched the base name %q, so skipping it would lose %q",
					pattern, base, path)
			}
		}
	}
	if skipped == 0 {
		t.Fatal("no generated pattern had a '/' before a '[', so the skip was never exercised")
	}
}

// The JSON payload renders an empty match list as [], not null.
func TestMatchCandidatesReturnsAnEmptyListNotNil(t *testing.T) {
	got, truncated := matchCandidates("*.nothing", []string{"a.go", "b/c.go"}, 10)
	if got == nil || len(got) != 0 || truncated {
		t.Fatalf("got %#v (truncated %v), want an empty non-nil list", got, truncated)
	}
}

// benchmarkListing is a listing at find_files' candidate ceiling, shaped like a
// source tree: a few directory levels, most files with an extension.
func benchmarkListing() []string {
	dirs := []string{"src", "internal/store", "cmd/manvi", "crates/dc-verify/src", "web/app/components", "docs"}
	exts := []string{".go", ".rs", ".ts", "_test.go", ".md", ".json"}
	paths := make([]string, 0, 200000)
	for i := 0; len(paths) < 200000; i++ {
		d := dirs[i%len(dirs)]
		paths = append(paths, d+"/pkg"+itoa(i%97)+"/file"+itoa(i)+exts[i%len(exts)])
	}
	return paths
}

// BenchmarkFindFilesMatch measures the match loop alone over 200,000 paths at
// find_files' default limit, with patterns that match rarely so the whole
// listing is walked: a base-name pattern (nothing to skip, every path has a
// '/'), a path pattern (every base-name call skipped), and a literal.
func BenchmarkFindFilesMatch(b *testing.B) {
	paths := benchmarkListing()
	for _, pattern := range []string{"*file19999?.go", "internal/*/file19999?.go", "nothing-matches-this"} {
		b.Run(pattern+"/skipping", func(b *testing.B) {
			for b.Loop() {
				matchCandidates(pattern, paths, 100)
			}
		})
		b.Run(pattern+"/unskipped", func(b *testing.B) {
			for b.Loop() {
				matchCandidatesUnskipped(pattern, paths, 100)
			}
		})
	}
}
