package contract

import (
	"path/filepath"
	"slices"
	"testing"
)

// Every bench/test_*.py is a suite, and a suite verify.sh does not run is a
// check nobody sees fail: test_adv_b26.py sat outside the loop after its lane
// landed. The loop is the one list, so a new suite file has to be named in it.
func TestEveryBenchTestFileIsInVerifyShSuiteLoop(t *testing.T) {
	listed := verifyBenchSuites(t)
	var found []string
	for _, pattern := range []string{"test_*.py", "*/test_*.py"} {
		matches, err := filepath.Glob(filepath.Join(docRoot, "bench", pattern))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range matches {
			rel, err := filepath.Rel(filepath.Join(docRoot, "bench"), m)
			if err != nil {
				t.Fatal(err)
			}
			found = append(found, filepath.ToSlash(rel))
		}
	}
	if len(found) < 5 {
		t.Fatalf("found only %d bench test files: %v", len(found), found)
	}
	for _, f := range found {
		if !slices.Contains(listed, f) {
			t.Errorf("bench/%s is not in verify.sh's suite loop, so the gate never runs it", f)
		}
	}
}
