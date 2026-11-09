//go:build unix

package devcouncil

import (
	"encoding/json"
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
// refusal. That leaves the withheld report as the distinguisher, because only a
// pattern that matched the linked file would retry and so only its reply would
// name the new link. Fix (b) retries on ANY change, so the decision no longer
// depends on content and the two replies are identical. This test asserts that
// invariant (it used to skip once (b) made both paths name the files): a link
// made mid-search gives the same reply, withheld list, files_searched and count
// included, for a pattern that matches the linked secret and one that does not,
// and that reply is the one a link made before the call gives.
func TestZadvW17CandidateAAloneStillLeaksThroughTheWithheldReport(t *testing.T) {
	// The matching pattern, with the link made mid-search.
	hit := raceFixture(t).call("devcouncil_grep", map[string]any{"pattern": "LATE", "include_ignored": true})
	// The non-matching pattern, same race.
	miss := raceFixture(t).call("devcouncil_grep", map[string]any{"pattern": "zzz-no-such-text", "include_ignored": true})
	// The reference: the secret linked BEFORE the call, so the listing
	// partitions it out from the start.
	before := newFixture(t)
	writeRepoFile(t, before.root, ".env", "decoy=1\n")
	linkSecret(t, before.root, "token.txt", "alias.txt", "API=needle LATE\n")
	writeRepoFile(t, before.root, "src/a.go", "needle visible\n")
	ref := before.call("devcouncil_grep", map[string]any{"pattern": "zzz-no-such-text", "include_ignored": true})

	if hit.IsError || miss.IsError || ref.IsError {
		t.Fatalf("a reply is an error: hit=%v miss=%v ref=%v\n%s\n%s", hit.IsError, miss.IsError, ref.IsError, hit.Text, miss.Text)
	}
	shape := func(text string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(text), &m); err != nil {
			t.Fatalf("reply is not JSON: %v: %s", err, text)
		}
		delete(m, "pattern")
		return m
	}
	h, m, r := shape(hit.Text), shape(miss.Text), shape(ref.Text)
	for _, key := range []string{"withheld", "files_searched", "count"} {
		if fmt.Sprint(h[key]) != fmt.Sprint(m[key]) {
			t.Errorf("%s differs between a pattern that matches the linked secret and one that does not:\n%v\n%v", key, h[key], m[key])
		}
		if fmt.Sprint(m[key]) != fmt.Sprint(r[key]) {
			t.Errorf("%s after a mid-search link is not what a link made before the call gives:\n%v\n%v", key, m[key], r[key])
		}
	}
	for _, name := range []string{".env", "alias.txt", "token.txt"} {
		if !strings.Contains(hit.Text, name) || !strings.Contains(miss.Text, name) {
			t.Errorf("the withheld report is missing %q on one of the two replies", name)
		}
	}
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
