package testsupport

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestBuiltBinaryIsNotAPathCargoRewrites is the defect this package's third
// rule exists for.
//
// `go test ./...` runs dc/store and devcouncil in separate processes against
// one target directory. Whichever reaches cargo second unlinks and rewrites
// target/debug/dcstore while the first is exec'ing it, and that test dies with
// a fork/exec ENOENT naming a binary it never touched. The per-process
// sync.Once in cargoBin cannot order two processes, and cargo's workspace lock
// is released before the artifact stops moving.
//
// The rebuild loop here stands in for the other package's process, and it is
// deliberately *not* a stale build: cargo re-does its uplift even when it
// compiles nothing, so a no-op build is enough to replace the file. That is
// the case a full test run actually hits.
//
// What is asserted is that the file does not move, not that some exec happened
// to survive. Racing the exec directly would be the honest-looking test and the
// useless one: the window between cargo's unlink and its write is tens of
// microseconds, so a loop of a few hundred exec attempts misses a *present*
// regression almost every time. Measured against the pre-fix helper, such a
// loop passed three runs out of three while a tight stat loop caught the same
// build replacing the binary hundreds of times. So the test asserts the
// property that makes exec safe — the path handed to a test is one cargo has
// no reason to touch — which either holds or does not.
func TestBuiltBinaryIsNotAPathCargoRewrites(t *testing.T) {
	bin := DCStore(t)
	crates := filepath.Join(RepoRoot(t), "crates")

	before, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("stat the binary cargoBin handed out: %v", err)
	}

	for round := 0; round < 4; round++ {
		cmd := exec.Command("cargo", "build", "-p", "dc-store", "--bin", "dcstore")
		cmd.Dir = crates
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("round %d: cargo build: %v\n%s", round, err, out)
		}

		after, err := os.Stat(bin)
		if err != nil {
			t.Fatalf("round %d: a cargo build removed the binary cargoBin handed out: %v\n"+
				"tests must be given a path cargo does not own", round, err)
		}
		if !os.SameFile(before, after) {
			t.Fatalf("round %d: a cargo build replaced the binary cargoBin handed out (%s)\n"+
				"the file is a different one than the test was given; another package's "+
				"build would have done this mid-exec", round, bin)
		}
	}

	// The invariant is about the file; this is the consequence that matters.
	if err := exec.Command(bin, "--db", filepath.Join(t.TempDir(), "s.sqlite"), "health").Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("after four concurrent-style rebuilds the binary no longer execs: %v", err)
		}
	}
}
