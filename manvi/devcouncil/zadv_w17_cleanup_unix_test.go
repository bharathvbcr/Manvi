//go:build unix

package devcouncil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/dcgrep"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
)

// raceFixture stages the race the post-search judgement exists for, with no
// sleeps. A refused decoy (.env) forces the partitioned path, so the sub-searches
// run strictly after candidateRefusals has judged the listing. The wrapper binary
// makes the second name for token.txt inside the first sub-search (a request whose
// path is neither empty nor "." is a sub-search; the discarded speculative one
// carries "." and is left alone), which is after the listing was judged and before the post-search
// judgement: exactly the window of a concurrent writer.
func raceFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	real := testsupport.DCGrep(t)
	writeRepoFile(t, f.root, ".env", "decoy=1\n")
	writeRepoFile(t, f.root, "token.txt", "API=needle LATE\n")
	writeRepoFile(t, f.root, "src/a.go", "needle visible\n")

	wrapper := filepath.Join(t.TempDir(), "dcgrep-race")
	script := fmt.Sprintf(`#!/bin/sh
REAL=%q
ROOT=%q
if [ "$1" = search ]; then
  BODY=$(cat)
  case "$BODY" in
    *'"path":".",'*|*'"path":"."}'*) ;;
    *'"path":"'*) ln "$ROOT/token.txt" "$ROOT/alias.txt" 2>/dev/null ;;
  esac
  printf '%%s' "$BODY" | "$REAL" "$@"
  exit $?
fi
exec "$REAL" "$@"
`, real, f.root)
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil { //nolint:gosec // an executable wrapper script in a test temp dir
		t.Fatal(err)
	}
	f.reg.deps.Grep = dcgrep.New(wrapper, f.root)
	return f
}

// BREAK (attack 1, fails on 0de348e): the post-search error exists only when a
// now-refused file MATCHED, so a writer who links a secret mid-search reads one
// bit per pattern from "error" versus "answer". Same setup, two patterns: one
// that matches the secret's content, one that does not. The outcomes must not
// differ; they do.
func TestZadvW17GrepRaceErrorLeaksPerPatternBit(t *testing.T) {
	hit := raceFixture(t)
	miss := raceFixture(t)
	a := advGrepRaw(t, hit, "needle LATE", map[string]any{"include_ignored": true})
	b := advGrepRaw(t, miss, "zzz-no-such-text", map[string]any{"include_ignored": true})
	t.Logf("pattern matching the secret : %s", a)
	t.Logf("pattern not matching        : %s", b)
	aErr, bErr := strings.HasPrefix(a, "isError=true"), strings.HasPrefix(b, "isError=true")
	if aErr != bErr {
		t.Fatalf("BREAK: the call errors (%v) for a pattern that matches the linked secret and not (%v) for one that does not; "+
			"the error's existence is the per-pattern answer partitioning hides", aErr, bErr)
	}
}

// Attack 1, candidate fix (a) alone: re-run listing plus search after a detected
// refusal. The retry partitions the alias out, so the matching pattern's reply
// is the one a file refused at listing gives. The non-matching pattern raised
// no refusal and so would not retry, and its reply was built from the first
// listing, which did not know the alias. This test pins the difference the
// withheld report would carry between those two paths: it must hold for fix (a)
// to be insufficient, and it does.
func TestZadvW17CandidateAAloneStillLeaksThroughTheWithheldReport(t *testing.T) {
	// What the retry returns for the matching pattern: the secret linked BEFORE
	// the call, so the listing partitions it out.
	retried := newFixture(t)
	writeRepoFile(t, retried.root, ".env", "decoy=1\n")
	linkSecret(t, retried.root, "token.txt", "alias.txt", "API=needle LATE\n")
	writeRepoFile(t, retried.root, "src/a.go", "needle visible\n")
	afterRetry := retried.call("devcouncil_grep", map[string]any{"pattern": "needle", "include_ignored": true}).Text

	// What the non-matching pattern returns when the link lands mid-search and
	// nothing matched in it: no error, no retry, the first listing's report.
	noRetry := raceFixture(t)
	noRetryReply := noRetry.call("devcouncil_grep", map[string]any{"pattern": "needle visible", "include_ignored": true})
	if noRetryReply.IsError {
		t.Fatalf("setup: the non-matching race should answer normally: %s", noRetryReply.Text)
	}
	t.Logf("retry reply (link seen at listing):\n%s", afterRetry)
	t.Logf("no-retry reply (link made mid-search, no match in it):\n%s", noRetryReply.Text)
	nameInRetry := strings.Contains(afterRetry, "alias.txt") || strings.Contains(afterRetry, "token.txt")
	nameInPlain := strings.Contains(noRetryReply.Text, "alias.txt") || strings.Contains(noRetryReply.Text, "token.txt")
	if nameInRetry == nameInPlain {
		t.Skipf("the withheld report does not name the aliased files on either path (retry=%v plain=%v); fix (a) alone would suffice here",
			nameInRetry, nameInPlain)
	}
	t.Logf("fix (a) alone leaves a distinguisher: withheld report names the files after a retry (%v) and not otherwise (%v); "+
		"fix (b) (judge every listed file after the search, retry on ANY new refusal) removes it because the decision no longer depends on content",
		nameInRetry, nameInPlain)
}

// Cost of fix (b): judging every searched file after the search with the cheap
// listing judgement (candidateRefusals: name and inode, no content) rather than
// only the matched files with the full one.
func BenchmarkZadvW17FixBCostOverListing(b *testing.B) {
	f := newFixture(b)
	const n = 5000
	for i := range n {
		full := filepath.Join(f.root, fmt.Sprintf("d%d/f%d.go", i%50, i))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x\n"), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	list, err := f.reg.deps.Grep.List(b.Context(), dcgrep.ListRequest{MaxResults: grepListCeiling, IncludeIgnored: true})
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("listed %d files", len(list.Paths))
	b.Run("candidateRefusals-all-listed", func(b *testing.B) {
		for range b.N {
			_ = f.reg.candidateRefusals(list.Paths)
		}
	})
	b.Run("readRefusal-all-listed", func(b *testing.B) {
		for range b.N {
			for _, p := range list.Paths {
				_ = f.reg.readRefusal(p)
			}
		}
	})
	b.Run("revalidateMatches-5000-matches-in-20-files", func(b *testing.B) {
		matches := matchesIn(grepMaxLimit, list.Paths[:20]...)
		for range b.N {
			_ = f.reg.revalidateMatches(matches)
		}
	})
}

// Attack 2: the memo is keyed by the exact path string the engine returned.
// Each distinct spelling is judged on its own, a clean path judged first never
// vouches for a later one, and a symlink naming a secret is refused.
func TestZadvW17RevalidateMemoIsPerPathAndSeesSymlinksAndCase(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, "src/a.go", "x\n")
	writeRepoFile(t, f.root, ".env", "K=v\n")
	linkSecret(t, f.root, "token.txt", "alias.txt", "x\n")
	if err := os.Symlink(".env", filepath.Join(f.root, "innocent.txt")); err != nil {
		t.Skipf("no symlinks: %v", err)
	}
	if err := os.Symlink("/etc/hosts", filepath.Join(f.root, "outside.txt")); err != nil {
		t.Skipf("no symlinks: %v", err)
	}
	m := func(paths ...string) []dcgrep.Match {
		var out []dcgrep.Match
		for i, p := range paths {
			out = append(out, dcgrep.Match{Path: p, LineNumber: i + 1})
		}
		return out
	}
	for name, paths := range map[string][]string{
		"clean-then-alias":         {"src/a.go", "src/a.go", "alias.txt"},
		"alias-then-clean":         {"alias.txt", "src/a.go"},
		"alias-duplicated":         {"alias.txt", "alias.txt", "alias.txt"},
		"symlink-to-secret":        {"src/a.go", "innocent.txt"},
		"symlink-outside-root":     {"outside.txt"},
		"secret-name":              {"src/a.go", ".env"},
		"dot-slash-spelling":       {"src/a.go", "./alias.txt"},
		"clean-and-dotslash-clean": {"src/a.go", "./src/a.go", "src/../alias.txt"},
		"case-variant-of-alias":    {"src/a.go", "ALIAS.TXT"},
		"case-variant-secret":      {"src/a.go", ".ENV"},
		"trailing-dot-secret":      {"src/a.go", ".env."},
		"alias-after-many-clean":   append(repeatPath("src/a.go", 50), "alias.txt"),
		"alias-between-many-clean": append(append(repeatPath("src/a.go", 10), "alias.txt"), repeatPath("src/a.go", 10)...),
	} {
		if err := f.reg.revalidateMatches(m(paths...)); err == nil {
			// A case variant only names the alias on a case-insensitive
			// filesystem; elsewhere the file does not exist and is not a secret
			// by name either, which is not a leak.
			if strings.Contains(name, "case-variant") {
				if _, statErr := os.Lstat(filepath.Join(f.root, paths[len(paths)-1])); statErr != nil {
					t.Logf("%s: %q does not exist on this filesystem; not judged refused, not a read either", name, paths[len(paths)-1])
					continue
				}
			}
			t.Errorf("BREAK %s: %v was let through", name, paths)
		}
	}
	if err := f.reg.revalidateMatches(m("src/a.go", "src/a.go", "seed.txt")); err != nil {
		t.Errorf("clean matches refused: %v", err)
	}
}

func repeatPath(p string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = p
	}
	return out
}
