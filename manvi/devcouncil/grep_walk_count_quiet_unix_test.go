//go:build unix

package devcouncil

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
)

// False-positive guard for the walked-file count in searchAttempt (card
// ft-d206865e0745d54a684041442b35d557): a quiet tree, with no concurrent writer,
// never ends in the retry error, whatever mix of entries the engine and the
// listing account for. Every test builds a fixture, runs the real engine with
// nothing changing the tree, and asserts the reply is not the retry error and
// holds matches.

const walkQuietRetryError = "the tree changed under the search repeatedly; retry"

// walkQuietGrep runs devcouncil_grep and returns the decoded reply, failing on a
// retry error.
func walkQuietGrep(t *testing.T, f *fixture, pattern string, extra map[string]any) (paths []string, reply map[string]any) {
	t.Helper()
	args := map[string]any{"pattern": pattern}
	for k, v := range extra {
		args[k] = v
	}
	res := f.call("devcouncil_grep", args)
	if res.IsError {
		t.Fatalf("grep %v failed in a quiet tree (no writer): %s", extra, res.Text)
	}
	if strings.Contains(res.Text, walkQuietRetryError) {
		t.Fatalf("retry error in a quiet tree: %s", res.Text)
	}
	if err := json.Unmarshal([]byte(res.Text), &reply); err != nil {
		t.Fatalf("reply is not JSON: %v\n%s", err, res.Text)
	}
	ms, _ := reply["matches"].([]any)
	seen := map[string]bool{}
	for _, m := range ms {
		mm, _ := m.(map[string]any)
		p, _ := mm["path"].(string)
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths, reply
}

func walkQuietWant(t *testing.T, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("matched files = %v, want %v", got, want)
	}
}

// walkQuietOddTree writes a quiet tree holding every kind of entry the engine and
// the listing might count differently. Every legitimate file holds "needle".
// nested/n.go sits in a nested git repository, so it is returned with the files
// that only an include_ignored search reaches.
func walkQuietOddTree(t *testing.T, f *fixture) (visible []string, visibleIgnored []string) {
	t.Helper()
	root := f.root
	w := func(rel, content string) { writeRepoFile(t, root, rel, content) }
	w("src/a.go", "needle a\n")
	w("src/b.txt", "needle b\n")
	w("src/deep/c.txt", "needle c\n")
	w(".gitignore", "ignored/\n*.log\n")
	w("ignored/x.txt", "needle ignored\n")
	w("z.log", "needle log\n")
	w(".ignore", "dotignored.txt\n")
	w("dotignored.txt", "needle dotignored\n")
	w(".hid/h.txt", "needle hidden\n")
	w(".hidden.txt", "needle hidden file\n")
	w("nested/n.go", "needle nested\n")
	w("nested/.git/HEAD", "ref: refs/heads/main\n")
	w("nested/.git/config", "needle in nested git\n")
	w("binary.bin", "needle\x00\x00\n")
	big := strings.Repeat("x", 2*1024*1024+16) + "\nneedle\n"
	w("big.txt", big)
	// exactly at and just under the limit (the limit is >=, so 2 MiB is skipped)
	w("exact.txt", strings.Repeat("y", 2*1024*1024-len("\nneedle\n")-1)+"\nneedle\n")
	w("atlimit.txt", strings.Repeat("y", 2*1024*1024-len("\nneedle\n"))+"\nneedle\n")
	w("empty.txt", "")
	w("uni/café.txt", "needle nfc\n")
	w("uni/caféd.txt", "needle nfd\n")
	if err := os.MkdirAll(filepath.Join(root, ".git", "info"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "info", "exclude"), []byte("gitexcluded.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w("gitexcluded.txt", "needle gitexcluded\n")
	// symlinks: a file link, a directory link, a broken link
	for name, target := range map[string]string{"link-to-a": "src/a.go", "link-to-dir": "src", "broken": "does/not/exist"} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"src/a.go", "src/b.txt", "src/deep/c.txt", "exact.txt", "uni/café.txt", "uni/caféd.txt"},
		[]string{"nested/n.go", "ignored/x.txt", "z.log", "dotignored.txt", ".hid/h.txt", ".hidden.txt", "gitexcluded.txt", ".gitignore"}
}

// Card criterion "no regression": a legitimate grep in an ordinary tree never
// fails because of the walked-file count, for every listing/search shape the
// engine accounts for differently (ignore rules, hidden, nested repo, symlinks,
// FIFO, too large, binary), with nothing refused (speculative search).
func TestGrepQuietOddTreeSpeculativeSearchIsNotARetryError(t *testing.T) {
	for _, ignored := range []bool{false, true} {
		t.Run(fmt.Sprintf("include_ignored=%v", ignored), func(t *testing.T) {
			f := newFixture(t)
			visible, extra := walkQuietOddTree(t, f)
			got, reply := walkQuietGrep(t, f, "needle", map[string]any{"include_ignored": ignored, "max_results": 500})
			want := visible
			if ignored {
				for _, e := range extra {
					if e != ".gitignore" {
						want = append(append([]string{}, want...), e)
					}
				}
			}
			t.Logf("files_searched=%v withheld=%v", reply["files_searched"], reply["withheld"])
			// nested/ holds its own .git: dc-grep (29cf2dde) does not descend an untracked
			// nested repository unless ignore rules are off (include_ignored).
			for _, g := range got {
				if g == "nested/n.go" && !ignored {
					t.Errorf("nested/n.go is inside an untracked nested git repo and must not match with include_ignored=false, got %v", got)
				}
			}
			for _, w := range want {
				found := false
				for _, g := range got {
					found = found || g == w
				}
				if !found {
					t.Errorf("expected a match in %q, got %v", w, got)
				}
			}
		})
	}
}

// Same tree, but a withheld .env is inside the searched path, so the search is
// partitioned around it and the refused subtraction applies.
func TestGrepQuietOddTreeWithARefusedFileIsNotARetryError(t *testing.T) {
	for _, path := range []string{"", ".", "src", "nested", "uni", "src/deep", "src/a.go"} {
		for _, ignored := range []bool{false, true} {
			t.Run(fmt.Sprintf("path=%q/include_ignored=%v", path, ignored), func(t *testing.T) {
				f := newFixture(t)
				walkQuietOddTree(t, f)
				writeRepoFile(t, f.root, ".env", "needle SECRET\n")
				writeRepoFile(t, f.root, "src/.env", "needle SECRET2\n")
				writeRepoFile(t, f.root, "nested/.env", "needle SECRET3\n")
				extra := map[string]any{"include_ignored": ignored, "max_results": 500}
				if path != "" {
					extra["path"] = path
				}
				got, _ := walkQuietGrep(t, f, "needle", extra)
				for _, g := range got {
					if strings.HasSuffix(g, ".env") {
						t.Errorf("withheld file matched: %v", got)
					}
				}
			})
		}
	}
}

// path at a single file and at subdirectories; with and without a refused
// sibling.
func TestGrepQuietPathShapesAreNotARetryError(t *testing.T) {
	for _, withRefused := range []bool{false, true} {
		for path, want := range map[string][]string{
			"src/a.go": {"src/a.go"},
			"src":      {"src/a.go", "src/b.txt", "src/deep/c.txt"},
			"./src/":   {"src/a.go", "src/b.txt", "src/deep/c.txt"},
			"uni":      nil, // two names, one NFC and one NFD; only the count is asserted
		} {
			t.Run(fmt.Sprintf("refused=%v/%s", withRefused, path), func(t *testing.T) {
				f := newFixture(t)
				walkQuietOddTree(t, f)
				if withRefused {
					writeRepoFile(t, f.root, ".env", "needle S\n")
					writeRepoFile(t, f.root, "src/.env", "needle S\n")
				}
				extra := map[string]any{"path": path, "include_ignored": true, "max_results": 500}
				got, _ := walkQuietGrep(t, f, "needle", extra)
				if want == nil {
					if len(got) != 2 {
						t.Fatalf("matched files = %v, want the two names under uni", got)
					}
					return
				}
				walkQuietWant(t, got, want...)
			})
		}
	}
}

// The reason the listing's skipped total is allowed for as slack: a too-large
// file inside a searched directory is walked by the engine but is not in the
// listing's paths, and one in a directory the search does not reach (it holds a
// refused file) is in the listing's skipped total only. A quiet tree holding
// both, beside a refused .env, must get a normal reply and no retry.
func TestGrepQuietTooLargeFilesBesideARefusedFileAreNotARetryError(t *testing.T) {
	huge := strings.Repeat("x", 2*1024*1024+16) + "\nneedle\n"
	f := newFixture(t)
	writeRepoFile(t, f.root, ".env", "needle SECRET\n")
	writeRepoFile(t, f.root, "src/a.go", "needle a\n")
	writeRepoFile(t, f.root, "src/big.txt", huge)
	writeRepoFile(t, f.root, "sec/.env", "needle SECRET2\n")
	writeRepoFile(t, f.root, "sec/ok.txt", "needle ok\n")
	writeRepoFile(t, f.root, "sec/big.txt", huge)
	got, reply := walkQuietGrep(t, f, "needle", map[string]any{"include_ignored": true, "max_results": 500})
	walkQuietWant(t, got, "src/a.go", "sec/ok.txt")
	if reply["withheld"] == nil || reply["skipped"] == nil {
		t.Errorf("expected a withheld report and a skipped report, got %v", reply)
	}
}

// Unreadable file and directory (chmod 0), an invalid .gitignore, and a locked
// directory, with and without a refused file.
func TestGrepQuietUnreadableAndMalformedIgnoreAreNotARetryError(t *testing.T) {
	setups := map[string]func(t *testing.T, f *fixture){
		"chmod 000 file": func(t *testing.T, f *fixture) {
			writeRepoFile(t, f.root, "src/locked.txt", "needle locked\n")
			if err := os.Chmod(filepath.Join(f.root, "src/locked.txt"), 0); err != nil {
				t.Fatal(err)
			}
		},
		"chmod 000 directory": func(t *testing.T, f *fixture) {
			writeRepoFile(t, f.root, "src/lockdir/l.txt", "needle l\n")
			if err := os.Chmod(filepath.Join(f.root, "src/lockdir"), 0); err != nil {
				t.Fatal(err)
			}
		},
		"malformed gitignore at root": func(t *testing.T, f *fixture) {
			writeRepoFile(t, f.root, ".gitignore", "[unclosed\n**bad**[\nok.txt\n")
		},
		"malformed gitignore in a subdirectory": func(t *testing.T, f *fixture) {
			writeRepoFile(t, f.root, "src/.gitignore", "[unclosed\n")
		},
		"malformed .ignore": func(t *testing.T, f *fixture) {
			writeRepoFile(t, f.root, ".ignore", "[unclosed\n")
		},
		"non-UTF-8 file name": func(t *testing.T, f *fixture) {
			writeRepoFile(t, f.root, "src/ok.txt", "needle ok\n")
			if err := os.WriteFile(filepath.Join(f.root, "src", "bad\xff.txt"), []byte("needle bad\n"), 0o600); err != nil {
				t.Logf("filesystem refuses a non-UTF-8 name, so this case ran without one: %v", err)
			}
		},
	}
	for name, setup := range setups {
		for _, withRefused := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/refused=%v", name, withRefused), func(t *testing.T) {
				f := newFixture(t)
				writeRepoFile(t, f.root, "src/a.go", "needle a\n")
				writeRepoFile(t, f.root, "src/b.go", "needle b\n")
				writeRepoFile(t, f.root, "top.go", "needle top\n")
				setup(t, f)
				t.Cleanup(func() {
					_ = os.Chmod(filepath.Join(f.root, "src/lockdir"), 0o700)    //nolint:errcheck,gosec // restore a test directory so t.TempDir can remove it
					_ = os.Chmod(filepath.Join(f.root, "src/locked.txt"), 0o600) //nolint:errcheck // restore for cleanup
				})
				if withRefused {
					writeRepoFile(t, f.root, ".env", "needle S\n")
					writeRepoFile(t, f.root, "src/.env", "needle S\n")
				}
				got, _ := walkQuietGrep(t, f, "needle", map[string]any{"include_ignored": true, "max_results": 500})
				if len(got) == 0 {
					t.Errorf("no matches")
				}
			})
		}
	}
}

// A big tree, with max_results small so the engine cuts early and prunes.
func TestGrepQuietBigTreeSmallCapIsNotARetryError(t *testing.T) {
	for _, withRefused := range []bool{false, true} {
		for _, limit := range []int{1, 3, 50} {
			t.Run(fmt.Sprintf("refused=%v/max_results=%d", withRefused, limit), func(t *testing.T) {
				f := newFixture(t)
				for i := range 5000 {
					writeRepoFile(t, f.root, fmt.Sprintf("d%02d/f%04d.txt", i%50, i), "needle line\nmore\n")
				}
				if withRefused {
					writeRepoFile(t, f.root, ".env", "needle S\n")
					writeRepoFile(t, f.root, "d07/.env", "needle S\n")
				}
				got, reply := walkQuietGrep(t, f, "needle", map[string]any{"include_ignored": true, "max_results": limit})
				if len(got) == 0 {
					t.Errorf("no matches")
				}
				t.Logf("files_searched=%v truncated=%v", reply["files_searched"], reply["truncated"])
			})
		}
	}
}
