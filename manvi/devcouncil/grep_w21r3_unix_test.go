//go:build unix

package devcouncil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A legitimate file created in a directory that holds no listed file (an empty
// directory the engine walks), after the listing and before the sub-search, and
// left there. No stamp used to cover such a directory, so on the stamp fast path
// nothing changed, the file was searched and counted, and its match was dropped
// as a name the listing did not hold. Every directory the listing's tree holds is
// stamped now, so the new name changes a stamp and the full relist sees it.
//
// The clock is moved on so no stamp reads fresh and the fast path is the one
// under test; the test fails unless the full relist ran, which is what the new
// directory stamp forces.
func TestGrepLegitFileInAnEmptyDirectoryIsNotDroppedOnTheFastPath(t *testing.T) {
	f := w21Fixture(t, map[string]string{
		".env":     "API=needle SECRET\n",
		"src/a.go": "needle visible\n",
	}, w21Writer{
		searchPre: `echo "needle fresh" > "$ROOT/src/empty/new.txt"`,
	})
	if err := os.MkdirAll(filepath.Join(f.root, "src", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	grepNow = func() time.Time { return time.Now().Add(time.Hour) }
	rechecks := countFullRechecks(t)
	got := advGrepRaw(t, f, "needle", map[string]any{"include_ignored": true})
	w21WriterRan(t, f)
	if *rechecks == 0 {
		t.Errorf("a name created in a directory that holds no listed file did not change a stamp, so the full relist never ran: %s", got)
	}
	if !strings.Contains(got, `"path":"src/empty/new.txt"`) || !strings.Contains(got, `"count":2`) {
		t.Errorf("the match in src/empty/new.txt was dropped: %s", got)
	}
}

// The search target directory is swapped for a symlink to a directory that holds
// a withheld file, only while the sub-search runs, so the engine (which
// canonicalises its target) walks the withheld directory and reads sec/.env
// under its own listed, refused name. No link to the file is made at all. The
// match in that name is dropped because the listing held it as refused; both
// patterns must get the same reply and the content must not be in it.
func TestGrepTargetDirectorySwappedForASymlinkToAWithheldDirectory(t *testing.T) {
	build := func() *fixture {
		return w21Fixture(t, map[string]string{
			"sec/.env":    "API=needle SECRET\n",
			"sec/pub.txt": "needle pub\n",
			"src/a.go":    "needle visible\n",
		}, w21Writer{
			searchPre:  `if [ -d "$ROOT/src" ] && [ ! -L "$ROOT/src" ]; then mv "$ROOT/src" "$ROOT/src.real" && ln -s sec "$ROOT/src"; fi`,
			searchPost: `if [ -L "$ROOT/src" ]; then rm "$ROOT/src" && mv "$ROOT/src.real" "$ROOT/src"; fi`,
		})
	}
	fa, fb := build(), build()
	a := advGrepRaw(t, fa, "needle (SECRET|visible|pub)", map[string]any{"include_ignored": true})
	b := advGrepRaw(t, fb, "needle (zzz|visible|pub)", map[string]any{"include_ignored": true})
	w21WriterRan(t, fa)
	w21WriterRan(t, fb)
	if a != b {
		t.Errorf("the reply depends on whether the withheld file matched:\nmatching: %s\nnot matching: %s", a, b)
	}
	if strings.Contains(a, "SECRET") {
		t.Errorf("withheld content in the reply: %s", a)
	}
}

// The same swap with max_results=1, so a match read from the withheld file
// competes for the only slot (sec/.env sorts before every listed match). The
// partitioned merge used to cut to the limit before the withheld file's match
// was dropped, so the dropped match took Manvi's own slot and the reply carried
// no match for a pattern the withheld file matched. Dropping per sub-result,
// before the merge and the cut, makes the two replies equal.
func TestGrepTargetDirectorySwapDoesNotTakeAResultSlot(t *testing.T) {
	build := func() *fixture {
		return w21Fixture(t, map[string]string{
			"sec/.env":  "API=needle SECRET\n",
			"src/a.go":  "nothing\n",
			"zz/z.go":   "needle visible\n",
			"zz/zz.txt": "needle visible2\n",
		}, w21Writer{
			searchPre:  `if [ -d "$ROOT/src" ] && [ ! -L "$ROOT/src" ]; then mv "$ROOT/src" "$ROOT/src.real" && ln -s sec "$ROOT/src"; fi`,
			searchPost: `if [ -L "$ROOT/src" ]; then rm "$ROOT/src" && mv "$ROOT/src.real" "$ROOT/src"; fi`,
		})
	}
	fa, fb := build(), build()
	extra := map[string]any{"include_ignored": true, "max_results": 1}
	a := advGrepRaw(t, fa, "needle (SECRET|visible)", extra)
	b := advGrepRaw(t, fb, "needle (zzz|visible)", extra)
	w21WriterRan(t, fa)
	w21WriterRan(t, fb)
	if a != b {
		t.Errorf("the capped reply depends on whether the withheld file matched:\nmatching: %s\nnot matching: %s", a, b)
	}
	if strings.Contains(a, "SECRET") {
		t.Errorf("withheld content in the reply: %s", a)
	}
}
