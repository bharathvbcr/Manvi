//go:build unix

package devcouncil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A name that exists only while the engine walks (linked to a withheld file just
// before the search, removed just after) is in neither listing, so what the
// search matched in it was once the only thing that told the reply apart: an
// error for a pattern the secret matches, a normal answer for one it does not,
// one bit per pattern about a withheld file. Both patterns must get the same
// reply, and the secret must not be in it.
//
// The writer is the w21Fixture wrapper, which also pins the stamp clock so the
// post-search check always takes the full relist path. The test proves it was
// not vacuous: the writer's hook ran (a marker file it touches), and the full
// relist-and-judge path ran.
func TestGrepTransientLinkToAWithheldFileIsPatternIndependent(t *testing.T) {
	for name, lnCmd := range map[string]string{
		"hard link": `ln "$ROOT/.env" "$ROOT/src/alias.txt"`,
		"symlink":   `ln -s "$ROOT/.env" "$ROOT/src/alias.txt"`,
	} {
		t.Run(name, func(t *testing.T) {
			rechecks := countFullRechecks(t)
			build := func() (*fixture, string) {
				fired := filepath.Join(t.TempDir(), "writer.fired")
				f := w21Fixture(t, map[string]string{
					".env":     "API=needle LATE\n",
					"src/a.go": "needle visible\n",
				}, w21Writer{
					searchPre:  `touch "` + fired + `"; ` + lnCmd,
					searchPost: `rm -f "$ROOT/src/alias.txt"`,
				})
				return f, fired
			}
			extra := map[string]any{"include_ignored": true}
			fa, firedA := build()
			a := advGrepRaw(t, fa, "needle LATE", extra)
			fb, firedB := build()
			b := advGrepRaw(t, fb, "zzz-none", extra)
			for _, fired := range []string{firedA, firedB} {
				if _, err := os.Stat(fired); err != nil {
					t.Fatalf("the writer never ran around a search, so this test proved nothing: %v", err)
				}
			}
			if *rechecks == 0 {
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
