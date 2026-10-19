//go:build darwin || linux

package credentials

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// lockedMemChildEnv switches this test binary into the child half of
// TestSealingFailsClosedWithoutLockedMemory. Its value names the phase.
const lockedMemChildEnv = "MANVI_CREDENTIALS_LOCKED_MEM_CHILD"

// Markers the child prints. None of them carries a credential.
const (
	markerFailedClosed  = "LOCKED-MEM: failed closed"
	markerSealed        = "LOCKED-MEM: sealed without locked memory"
	markerNotEnforced   = "LOCKED-MEM: mlock limit not enforced here"
	fakeLockedMemKey    = "fake-key-for-locked-memory-test-0000"
	lockedMemChildBound = 20 * time.Second
)

// TestSealingFailsClosedWithoutLockedMemory: when the process cannot get
// locked memory, sealing a credential must return an error, and promptly.
//
// memguard v0.23.0 answers a failed mlock inside core.NewBuffer with
// core.Panic, whose Purge takes the key mutex and then the key coffer's mutex.
// Both callers that allocate on the sealing path already hold one of those —
// getOrCreateKey holds keyMtx across NewCoffer, Coffer.View holds the coffer
// across NewBuffer(32) — and Go mutexes are not reentrant, so the goroutine
// blocks on itself forever. A Vani test hung 9m25s in exactly that frame.
//
// The failure is made deterministic by lowering RLIMIT_MEMLOCK to zero in a
// child process, which is the only process whose limit the test may touch. The
// parent bounds the child: on code that deadlocks the child is killed and the
// test fails as a hang, never by hanging the suite.
//
// The phases cover every memguard allocation this package makes. memguard
// deadlocks on two different locks: "cold" fails the first allocation of the
// process (the key coffer, under keyMtx); "warm" seals once with memory
// available, so the key exists, and then fails the per-seal key view (under
// the coffer mutex). "reveal" and "watch" open a credential sealed while
// memory was available; there memguard does not deadlock but completes a Purge
// and panics, which wipes every buffer in the process — the scrubber's
// included — so it is no more acceptable than the hang.
func TestSealingFailsClosedWithoutLockedMemory(t *testing.T) {
	if phase := os.Getenv(lockedMemChildEnv); phase != "" {
		lockedMemChild(phase)
		return
	}
	for _, phase := range []string{"cold", "warm", "reveal", "watch"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), lockedMemChildBound)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSealingFailsClosedWithoutLockedMemory$", "-test.count=1")
			cmd.Env = append(os.Environ(), lockedMemChildEnv+"="+phase)
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			err := cmd.Run()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Fatalf("sealing without locked memory hung for %s instead of failing closed; child output:\n%s", lockedMemChildBound, tail(out.Bytes()))
			}
			text := out.String()
			switch {
			case bytes.Contains(out.Bytes(), []byte(markerNotEnforced)):
				t.Skipf("this kernel does not enforce RLIMIT_MEMLOCK on mlock; the failure cannot be forced here")
			case bytes.Contains(out.Bytes(), []byte(markerSealed)):
				t.Fatalf("a credential was sealed with no locked memory available — that is a silent downgrade, not a fail-closed error:\n%s", tail(out.Bytes()))
			case !bytes.Contains(out.Bytes(), []byte(markerFailedClosed)):
				t.Fatalf("child exited (%v) without failing closed:\n%s", err, tail([]byte(text)))
			}
			if bytes.Contains(out.Bytes(), []byte(fakeLockedMemKey)) {
				t.Fatal("the child's output carried the credential value")
			}
			for _, line := range bytes.Split(out.Bytes(), []byte("\n")) {
				if bytes.HasPrefix(line, []byte("LOCKED-MEM:")) {
					t.Logf("child: %s", line)
				}
			}
		})
	}
}

// lockedMemChild runs one phase and exits; it never returns to the test
// framework, so its own result is only what it prints.
func lockedMemChild(phase string) {
	r := resolverWith(map[string]string{"ANTHROPIC_API_KEY": fakeLockedMemKey})
	var sealed Secret
	if phase != "cold" {
		var err error
		if sealed, err = r.Resolve("anthropic"); err != nil {
			fmt.Println("LOCKED-MEM: warm-up seal failed:", err)
			os.Exit(3)
		}
	}
	if err := unix.Setrlimit(unix.RLIMIT_MEMLOCK, &unix.Rlimit{Cur: 0, Max: 0}); err != nil {
		fmt.Println("LOCKED-MEM: setrlimit:", err)
		os.Exit(3)
	}
	if mlockStillWorks() {
		fmt.Println(markerNotEnforced)
		os.Exit(0)
	}
	switch phase {
	case "reveal":
		if _, err := sealed.Reveal(); err != nil {
			fmt.Println(markerFailedClosed+":", err)
			os.Exit(0)
		}
		fmt.Println("LOCKED-MEM: opened without locked memory")
		os.Exit(1)
	case "watch":
		// Watch has no error to return; refusing to arm is what failing
		// closed can mean for it, and the process must survive to say so.
		s := NewScrubber()
		s.Watch(sealed)
		if s.Count() != 0 {
			fmt.Println("LOCKED-MEM: armed the scrubber without locked memory")
			os.Exit(1)
		}
		if got := s.Clean("echo " + fakeLockedMemKey); !strings.HasPrefix(got, Withheld) {
			fmt.Println("LOCKED-MEM: an unarmed scrubber passed text on as cleaned")
			os.Exit(1)
		}
		fmt.Println(markerFailedClosed + ": scrubber unarmed and withholding")
		os.Exit(0)
	}
	_, err := r.Resolve("anthropic")
	if err == nil {
		fmt.Println(markerSealed)
		os.Exit(1)
	}
	fmt.Println(markerFailedClosed+":", err)
	os.Exit(0)
}

// mlockStillWorks reports whether one page can still be locked, which is what
// the child needs to be false before its result means anything.
func mlockStillWorks() bool {
	page, err := unix.Mmap(-1, 0, os.Getpagesize(), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
	if err != nil {
		return false
	}
	defer func() { _ = unix.Munmap(page) }()
	if err := unix.Mlock(page); err != nil {
		return false
	}
	_ = unix.Munlock(page)
	return true
}

func tail(b []byte) string {
	const max = 4000
	if len(b) > max {
		b = b[len(b)-max:]
	}
	return string(b)
}
