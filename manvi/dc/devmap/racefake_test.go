package devmap

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// A fake devmap that is a small ordinary binary, for the test that forks many
// at once under the race detector.
//
// The ordinary fake in devmap_test.go writes a `#!/bin/sh` script. That is the
// right choice for the great majority of these tests: one fork, readable inline,
// no build step. It is the wrong choice for exactly one shape — two dozen forks
// in flight at the same instant, under `-race`.
//
// Measured on this repository, macOS and Go 1.26:
//
//   - shell fake, `-race`, package alone: 2 of 5 runs died with the child
//     reporting `signal: segmentation fault` before it could exec.
//   - shell fake, `-race`, full `./...`: the package wedged instead, timing out
//     after ten minutes with a goroutine parked in `syscall.readlen`.
//   - shell fake, no `-race`: 8 of 8 clean.
//
// The devmap client is not implicated. It bounds every invocation with a
// context, caps stdout and stderr, and sets `WaitDelay` so a child holding the
// stdout pipe cannot outlive the kill — the production path is already the
// thing this repository claims it is. What breaks is below it: forking from a
// race-instrumented parent, many times over, is not reliable here.
//
// The first attempt at this fake hardlinked the test binary and answered from
// TestMain. That passed 12 of 12 isolated race runs at every worker count from
// 4 to 24 — and still failed inside `./verify.sh --race`, because the forked
// child was then itself race-instrumented. Each such process reserves a large
// shadow-memory region, and a full `go test -race ./...` already has several
// packages running at once; two dozen more on top is what exhausts it. The
// isolated measurement was not wrong, it was just not the condition that fails.
//
// So the fake is built once, without instrumentation, and forked as a plain
// ~2 MB binary. The client under test is exercised through the same exec path
// as every other test here; only the thing on the far side of the fork changes.
//
// The consequence is worth the machinery because the repository is public and
// `go test -race ./...` is the first thing a reviewer of a concurrency-heavy Go
// project runs. A ten-minute hang with no diagnosis is the worst answer that
// question can get.

// fakeSource is the helper program. It reads its reply table from the working
// directory the client sets (`cmd.Dir = Root`), so one built binary serves every
// test with a different table.
const fakeSource = `package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type replies struct {
	Stdout map[string]string ` + "`json:\"stdout\"`" + `
	Stderr map[string]string ` + "`json:\"stderr\"`" + `
}

func main() {
	args := os.Args[1:]
	// clap answers ` + "`manifest --help`" + ` with the flag list and exits before doing
	// any work, whatever else is on the line; the capability probe depends on
	// that short-circuit, so the fake honours it first.
	for _, a := range args {
		if a == "--help" {
			fmt.Println("usage: devmap manifest [OPTIONS]")
			fmt.Println("      --graph-output <PATH>")
			return
		}
	}
	raw, err := os.ReadFile("replies.json")
	if err != nil {
		// Fail loudly: a fake that silently answers nothing turns every test
		// using it into one that passes without exercising anything.
		fmt.Fprintln(os.Stderr, "fake devmap: no reply table:", err)
		os.Exit(2)
	}
	var r replies
	if err := json.Unmarshal(raw, &r); err != nil {
		fmt.Fprintln(os.Stderr, "fake devmap: unreadable reply table:", err)
		os.Exit(2)
	}
	for _, a := range args {
		out, hasOut := r.Stdout[a]
		errOut, hasErr := r.Stderr[a]
		if !hasOut && !hasErr {
			continue
		}
		if hasOut {
			fmt.Println(strings.TrimRight(out, "\n"))
		}
		if hasErr {
			fmt.Fprintln(os.Stderr, strings.TrimRight(errOut, "\n"))
		}
		return
	}
	fmt.Println("{}")
}
`

var (
	fakeBinaryOnce sync.Once
	fakeBinaryPath string
	fakeBinaryErr  error
)

// buildFakeDevmap compiles the helper once per test binary.
//
// Deliberately not `t.TempDir()`: the binary outlives any single test, and
// rebuilding it per test would cost more than the fork it is replacing.
func buildFakeDevmap() (string, error) {
	fakeBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "manvi-fake-devmap-")
		if err != nil {
			fakeBinaryErr = err
			return
		}
		src := filepath.Join(dir, "main.go")
		if err := os.WriteFile(src, []byte(fakeSource), 0o600); err != nil {
			fakeBinaryErr = err
			return
		}
		// A module file keeps the build from inheriting this repository's
		// module context, so the helper compiles standalone and fast.
		if err := os.WriteFile(filepath.Join(dir, "go.mod"),
			[]byte("module fakedevmap\n\ngo 1.21\n"), 0o600); err != nil {
			fakeBinaryErr = err
			return
		}
		out := filepath.Join(dir, "devmap")
		// No -race on the helper: an instrumented child is the thing that
		// exhausts shadow memory when two dozen run at once.
		cmd := exec.Command("go", "build", "-o", out, ".")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=")
		if combined, err := cmd.CombinedOutput(); err != nil {
			fakeBinaryErr = err
			fakeBinaryPath = string(combined)
			return
		}
		fakeBinaryPath = out
	})
	return fakeBinaryPath, fakeBinaryErr
}

// forkSafeFake is fake() for the test that runs many queries concurrently.
func forkSafeFake(t *testing.T, stdout map[string]string, stderr map[string]string) *Client {
	t.Helper()

	binary, err := buildFakeDevmap()
	if err != nil {
		// Fail, never skip. A skipped concurrency test reads exactly like a
		// passing one in the output this repository gates on.
		t.Fatalf("building the fork-safe fake: %v\n%s", err, binary)
	}

	dir := t.TempDir()
	table, err := json.Marshal(struct {
		Stdout map[string]string `json:"stdout"`
		Stderr map[string]string `json:"stderr"`
	}{stdout, stderr})
	if err != nil {
		t.Fatalf("encoding the reply table: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "replies.json"), table, 0o600); err != nil {
		t.Fatalf("writing the reply table: %v", err)
	}
	return New(binary, dir)
}

// TestTheForkSafeFakeActuallyAnswers is the check on the fake.
//
// Every assertion in the concurrent test passes trivially if this helper is
// broken in the direction of "no error, no data" — and a fake that answers
// nothing reads exactly like a client that works. So it states its own contract
// first, on one call, where a failure is legible.
func TestTheForkSafeFakeActuallyAnswers(t *testing.T) {
	c := forkSafeFake(t, map[string]string{
		"status": healthyStatus,
		"search": `{"items":[{"file_path":"a.go","symbol_name":"Alpha"}],"hidden":2}`,
	}, nil)

	r, err := c.Search(t.Context(), "anything")
	if err != nil {
		t.Fatalf("the fork-safe fake did not answer a search: %v", err)
	}
	if len(r.Items) != 1 || r.Items[0].Name != "Alpha" || r.Hidden != 2 {
		t.Fatalf("the fork-safe fake answered %+v, not the canned reply", r)
	}
	if _, err := c.Status(t.Context()); err != nil {
		t.Fatalf("the fork-safe fake did not answer status: %v", err)
	}
}
