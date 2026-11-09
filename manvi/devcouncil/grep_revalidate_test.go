package devcouncil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/dcgrep"
)

// matchesIn builds n matches spread over the named files, in path order.
func matchesIn(n int, files ...string) []dcgrep.Match {
	if len(files) == 0 {
		return nil
	}
	out := make([]dcgrep.Match, 0, n)
	for i := range n {
		out = append(out, dcgrep.Match{Path: files[i*len(files)/n], LineNumber: i + 1})
	}
	return out
}

// The per-file memo judges each matched file once, after the search. It must
// not turn a file that is refused by then into an allow: several matches in a
// refused file, mixed with matches in readable ones, still fail the reply, and
// the error names neither file.
func TestRevalidateMatchesStillWithholdsAFileRefusedDuringTheSearch(t *testing.T) {
	f := newFixture(t)
	writeRepoFile(t, f.root, "src/a.go", "needle\n")
	writeRepoFile(t, f.root, "src/b.go", "needle\n")
	// A second name for a secret appears after the listing: the same event the
	// speculative-search race test stages with a wrapper binary.
	linkSecret(t, f.root, "token.txt", "alias.txt", "needle\n")

	if err := f.reg.revalidateMatches(matchesIn(300, "src/a.go", "src/b.go")); err != nil {
		t.Fatalf("readable files were refused: %v", err)
	}
	for _, files := range [][]string{
		{"alias.txt"},
		{"src/a.go", "alias.txt"},
		{"alias.txt", "src/a.go", "src/b.go"},
		{"src/a.go", "src/b.go", "alias.txt"},
	} {
		err := f.reg.revalidateMatches(matchesIn(300, files...))
		if err == nil {
			t.Fatalf("matches in %v: a refused file was let through", files)
		}
		for _, leak := range []string{"alias.txt", "token.txt", "src/"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("matches in %v: the error names %q: %v", files, leak, err)
			}
		}
	}
}

// The post-search judgement is the dear part of a many-match reply: 5,000
// matches in a few files, judged per match (what the loop did) against once
// per file (what revalidateMatches does).
func BenchmarkGrepRevalidate(b *testing.B) {
	f := newFixture(b)
	var files []string
	for i := range 20 {
		name := fmt.Sprintf("src/pkg%d/file%d.go", i%5, i)
		full := filepath.Join(f.root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("needle\n"), 0o600); err != nil {
			b.Fatal(err)
		}
		files = append(files, name)
	}
	matches := matchesIn(grepMaxLimit, files...)

	b.Run("per-match", func(b *testing.B) {
		for range b.N {
			for _, m := range matches {
				if f.reg.readRefusal(m.Path) != "" {
					b.Fatal("refused")
				}
			}
		}
	})
	b.Run("per-file", func(b *testing.B) {
		for range b.N {
			if err := f.reg.revalidateMatches(matches); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkGrepTreeCheck prices the pieces of the post-search decision at 5,000
// and 50,000 listed files: the first listing, recording the stamps, reading them
// back (the fast path), and the full relist-and-judge path. The clock is moved
// an hour on so the files, which were just written, do not read as fresh.
func BenchmarkGrepTreeCheck(b *testing.B) {
	for _, n := range []int{5000, 50000} {
		f := newFixture(b)
		for i := range n {
			full := filepath.Join(f.root, fmt.Sprintf("d%d/f%d.go", i%100, i))
			if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
				b.Fatal(err)
			}
			if err := os.WriteFile(full, []byte("x\n"), 0o600); err != nil {
				b.Fatal(err)
			}
		}
		req := dcgrep.Request{IncludeIgnored: true}
		lreq := dcgrep.ListRequest{MaxResults: grepListCeiling, IncludeIgnored: true}
		list, err := f.reg.deps.Grep.List(b.Context(), lreq)
		if err != nil {
			b.Fatal(err)
		}
		refused, stamps := f.reg.judgeListing(list.Paths)
		prev := grepNow
		grepNow = func() time.Time { return time.Now().Add(time.Hour) }
		b.Cleanup(func() { grepNow = prev })
		snap := f.reg.snapshotTree(list.Paths, stamps, grepNow())
		if snap == nil || snap.fresh {
			b.Fatalf("no usable snapshot: %+v", snap == nil)
		}
		b.Run(fmt.Sprintf("n=%d/first-listing", n), func(b *testing.B) {
			for range b.N {
				if _, err := f.reg.deps.Grep.List(b.Context(), lreq); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("n=%d/record-stamps", n), func(b *testing.B) {
			for range b.N {
				_ = f.reg.snapshotTree(list.Paths, stamps, grepNow())
			}
		})
		b.Run(fmt.Sprintf("n=%d/stat-pass", n), func(b *testing.B) {
			tree := &listedTree{paths: list.Paths, snap: snap}
			for range b.N {
				ok, err := f.reg.treeUnchanged(b.Context(), req, tree, refused, nil)
				if err != nil || !ok {
					b.Fatalf("changed or failed: %v", err)
				}
			}
		})
		b.Run(fmt.Sprintf("n=%d/full-path", n), func(b *testing.B) {
			tree := &listedTree{paths: list.Paths}
			for range b.N {
				ok, err := f.reg.treeUnchanged(b.Context(), req, tree, refused, nil)
				if err != nil || !ok {
					b.Fatalf("changed or failed: %v", err)
				}
			}
		})
	}
}
