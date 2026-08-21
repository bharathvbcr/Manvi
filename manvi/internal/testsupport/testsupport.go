// Package testsupport locates the repository's build outputs for tests that
// drive real binaries across the Go/Rust boundary.
//
// It exists because of a defect it is designed to make impossible. The store
// tests located the Rust workspace with a hand-counted relative path and
// skipped when it was absent. The path was wrong by one level, so every test in
// that package skipped, and `go test ./...` printed `ok` for a package that had
// executed nothing. A seam that is never exercised reported the same result as
// a seam that passed — the exact failure mode the harness's own policy layer
// exists to prevent, reproduced in its test suite.
//
// Three rules follow, and all are enforced here rather than left to each test:
//
//   - The workspace is found by walking up for a marker, never by counting
//     "..". A test file that moves between directories keeps working.
//   - A missing toolchain fails the test by default. Skipping is available only
//     when an operator opts in explicitly with MANVI_TEST_ALLOW_SKIP=1,
//     which makes an uncovered seam a deliberate, visible choice.
//   - A test never execs a path cargo owns. `go test ./...` gives each package
//     its own process, all of them sharing one target directory, and cargo
//     unlinks and rewrites its output on every build — so the binary a test is
//     exec'ing can vanish because a *different* package rebuilt it. cargoBin
//     hands out a stable copy instead. See its comment for the measurements.
package testsupport

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// AllowSkipEnv opts a run into skipping when a toolchain is missing.
const AllowSkipEnv = "MANVI_TEST_ALLOW_SKIP"

// Unavailable reports a missing prerequisite: a hard failure by default, a skip
// only when the operator has said an uncovered seam is acceptable for this run.
func Unavailable(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv(AllowSkipEnv) == "1" {
		t.Skipf(format+" (skipping because "+AllowSkipEnv+"=1)", args...)
		return
	}
	t.Fatalf(format+"\n\nThis test drives a real binary and cannot verify anything without it. "+
		"Set "+AllowSkipEnv+"=1 to skip instead of failing, accepting that this seam goes uncovered.", args...)
}

// RepoRoot returns the directory containing the Go module and the Rust
// workspace, found by walking up from the test's working directory.
func RepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "crates", "Cargo.toml")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no repository root above %s (looking for crates/Cargo.toml)", dir)
		}
		dir = parent
	}
}

type build struct {
	once sync.Once
	path string
	err  error
	log  []byte
}

var builds = map[string]*build{}
var buildsMu sync.Mutex

// DCStore builds the Rust store binary and returns its path.
func DCStore(t *testing.T) string { return cargoBin(t, "dc-store", "dcstore") }

// DCVerify builds the Rust verifier binary and returns its path.
func DCVerify(t *testing.T) string { return cargoBin(t, "dc-verify", "dcverify") }

// cargoBin builds one binary and returns the path to a stable copy of it.
//
// The copy is the point. `cargo build` does not leave its output alone: every
// invocation re-does the uplift into target/debug, unlinking the existing file
// and writing a fresh one, *including* invocations that compile nothing because
// the build is already fresh. Measured on this workspace, twenty-five no-op
// builds left target/debug/dcstore absent for hundreds of observations of a
// tight stat loop, and its inode changed on every one.
//
// That matters because `go test ./...` runs each package in its own process.
// dc/store and devcouncil both reach cargoBin concurrently, and the sync.Once
// below is per-process, so it cannot order them. One package's no-op build
// therefore unlinks the binary another package's test is exec'ing at that
// instant, and the test fails with a fork/exec ENOENT that has nothing to do
// with what it was testing. sync.Once cannot fix this and neither can building
// more carefully: the artifact is shared, so the fix is to stop exec'ing the
// shared path.
//
// So the build output is copied once to a content-addressed path that cargo
// has no reason to touch, and that copy is what every test execs. Naming the
// copy after its own hash means concurrent processes converge on the same file
// with the same bytes without having to agree on a run identity, and publishing
// it by rename means nobody can exec it half-written.
//
// These copies live under target/, so `cargo clean` removes them with
// everything else. They are not pruned during a run: a copy another test
// process is about to exec is indistinguishable from a leftover, and deleting
// the wrong one would reintroduce exactly the failure this exists to prevent.
func cargoBin(t *testing.T, crate, binary string) string {
	t.Helper()
	root := RepoRoot(t)

	buildsMu.Lock()
	b, ok := builds[binary]
	if !ok {
		b = &build{}
		builds[binary] = b
	}
	buildsMu.Unlock()

	b.once.Do(func() {
		crates := filepath.Join(root, "crates")
		target := filepath.Join(crates, "target")
		if b.err = os.MkdirAll(target, 0o755); b.err != nil {
			return
		}

		// Held across the build *and* the copy. Cargo's workspace lock covers
		// only the compile and is released while target/debug is still being
		// rewritten, which is the window that produces the ENOENT.
		unlock, err := lockBuildOutputs(filepath.Join(target, buildLockName))
		if err != nil {
			b.err = fmt.Errorf("locking cargo build outputs: %w", err)
			return
		}
		defer unlock()

		cmd := exec.Command("cargo", "build", "-p", crate, "--bin", binary)
		cmd.Dir = crates
		b.log, b.err = cmd.CombinedOutput()
		if b.err != nil {
			return
		}

		built := filepath.Join(target, "debug", binary)
		data, err := readStableArtifact(built)
		if err != nil {
			b.err = err
			return
		}
		b.path, b.err = publishArtifact(target, binary, data)
	})
	if b.err != nil {
		Unavailable(t, "cannot build %s: %v\n%s", binary, b.err, b.log)
	}
	return b.path
}

// buildLockName is the lock file guarding the build-and-copy critical section.
const buildLockName = ".manvi-testbin.lock"

// testBinDir holds the stable copies, under target/ so `cargo clean` reaches
// them.
const testBinDir = "manvi-testbin"

const (
	artifactAttempts   = 50
	artifactRetryDelay = 20 * time.Millisecond
)

// readStableArtifact reads a cargo output that another process may be replacing
// underneath it, and returns only a copy it can show is whole.
//
// Two things can go wrong while reading target/debug/<binary>. The file can be
// absent, because cargo unlinks before it writes. Or it can be short, because
// cargo is still writing the replacement. The first is an error to retry. The
// second is the dangerous one: it reads as a successful read of a truncated
// binary, so it is checked for rather than assumed away. The size comes from
// the open descriptor, not the path, so it describes the same file the bytes
// came from even after the name has been reused.
func readStableArtifact(path string) ([]byte, error) {
	var last error
	for attempt := 0; attempt < artifactAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(artifactRetryDelay)
		}
		f, err := os.Open(path)
		if err != nil {
			last = err
			continue
		}
		data, readErr := io.ReadAll(f)
		info, statErr := f.Stat()
		f.Close()
		switch {
		case readErr != nil:
			last = readErr
		case statErr != nil:
			last = statErr
		case int64(len(data)) != info.Size():
			last = fmt.Errorf("read %d bytes of %s but it is %d bytes: it is being rewritten",
				len(data), path, info.Size())
		case len(data) == 0:
			last = fmt.Errorf("%s is empty", path)
		default:
			return data, nil
		}
	}
	return nil, fmt.Errorf("could not read a complete %s in %d attempts: %w",
		path, artifactAttempts, last)
}

// publishArtifact writes the bytes to a path named after their own hash and
// returns it, creating the file by rename so no reader ever sees a partial one.
// An entry that already exists is left alone: its name is its content, so it
// cannot differ from what we would write.
func publishArtifact(target, binary string, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	dir := filepath.Join(target, testBinDir, binary+"-"+hex.EncodeToString(sum[:])[:16])
	path := filepath.Join(dir, binary)
	if info, err := os.Stat(path); err == nil && info.Size() == int64(len(data)) {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, binary+".partial-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

// Tool checks that an external command the test depends on exists.
func Tool(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		Unavailable(t, "%s is not on PATH: %v", name, err)
	}
	return path
}
