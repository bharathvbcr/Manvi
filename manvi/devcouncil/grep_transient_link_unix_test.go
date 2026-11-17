//go:build unix

package devcouncil

import (
	"strings"
	"testing"
)

// A name that exists only while the engine walks (hard-linked to a withheld file
// just before the search, removed just after) is in neither listing, so what the
// search matched in it was once the only thing that told the reply apart: an
// error for a pattern the secret matches, a normal answer for one it does not,
// one bit per pattern about a withheld file. Both patterns must get the same
// reply, and the secret must not be in it. The hard-link subtest is the
// regression test: it fails on the code before the fix. The symlink subtest is a
// behaviour test only, since the engine never follows a symlink and a symlinked
// alias never leaked.
//
// The writer is the w21Fixture wrapper, which also pins the stamp clock so the
// post-search check always takes the full relist path. The test proves it was
// not vacuous: the writer's hook ran (a marker file it touches), and either the
// walked-file count ended the attempt (hard link) or the full relist-and-judge
// path ran (symlink).
func TestGrepTransientLinkToAWithheldFileIsPatternIndependent(t *testing.T) {
	for name, lnCmd := range map[string]string{
		"hard link": `ln "$ROOT/.env" "$ROOT/src/alias.txt"`,
		"symlink":   `ln -s "$ROOT/.env" "$ROOT/src/alias.txt"`,
	} {
		t.Run(name, func(t *testing.T) {
			rechecks := countFullRechecks(t)
			build := func() *fixture {
				return w21Fixture(t, map[string]string{
					".env":     "API=needle LATE\n",
					"src/a.go": "needle visible\n",
				}, w21Writer{
					searchPre:  lnCmd,
					searchPost: `rm -f "$ROOT/src/alias.txt"`,
				})
			}
			extra := map[string]any{"include_ignored": true}
			fa, fb := build(), build()
			a := advGrepRaw(t, fa, "needle LATE", extra)
			b := advGrepRaw(t, fb, "zzz-none", extra)
			w21WriterRan(t, fa)
			w21WriterRan(t, fb)
			// A hard-linked alias is a regular file the engine walks and the
			// listing did not hold, so the attempt ends on the walked-file count
			// before the post-search check is reached; a symlinked one is not
			// walked as a file and the post-search check runs as before.
			if name == "hard link" {
				if !strings.Contains(a, "the tree changed under the search repeatedly; retry") {
					t.Errorf("the walked-file count did not end the search in the retry error: %s", a)
				}
			} else if *rechecks == 0 {
				t.Fatal("the full relist-and-judge path never ran, so the post-search check was not exercised")
			}
			if a != b {
				t.Fatalf("the reply depends on whether the withheld file matched:\nmatching: %s\nnot matching: %s", a, b)
			}
			if strings.Contains(a, "LATE") || strings.Contains(a, "alias.txt") {
				t.Errorf("the reply carries the withheld file's content or the transient name: %s", a)
			}
		})
	}
}
