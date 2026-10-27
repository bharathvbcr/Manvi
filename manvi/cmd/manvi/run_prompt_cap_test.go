package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stdinFile is a regular file standing in for a redirected stdin: not a
// character device, so readPipedPrompt reads it.
func stdinFile(t *testing.T, body string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin.txt")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path) // #nosec G304 -- a file this test made under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// A prompt that does not fit is refused, never cut. It used to be read through
// an io.LimitReader with no overflow check: 1,048,662 bytes in, 1,048,576
// accepted, and the turn ran on the fragment, so an instruction that sat past
// the cut was never seen and nothing said so.
func TestAPipedPromptOverTheCapIsRefusedNotTruncated(t *testing.T) {
	body := strings.Repeat("a", maxPipedPromptBytes) + "\nthe real instruction is this tail\n"
	got, err := readPipedPrompt(stdinFile(t, body))
	t.Logf("prompt in=%d bytes, accepted=%d bytes, err=%v", len(body), len(got), err)
	if err == nil {
		t.Fatalf("a %d-byte prompt was accepted as %d bytes with no error", len(body), len(got))
	}
	if got != "" {
		t.Fatalf("a refused prompt still returned %d bytes", len(got))
	}
	if !errors.Is(err, errPromptTooLarge) {
		t.Errorf("err = %v, want errPromptTooLarge", err)
	}
	for _, want := range []string{"1048576", "1 MiB", "stdin", "shorter prompt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
}

func TestAPipedPromptOfExactlyTheCapIsAccepted(t *testing.T) {
	body := strings.Repeat("a", maxPipedPromptBytes)
	got, err := readPipedPrompt(stdinFile(t, body))
	if err != nil {
		t.Fatalf("a prompt of exactly the cap was refused: %v", err)
	}
	if got != body {
		t.Fatalf("accepted %d bytes, want %d unchanged", len(got), len(body))
	}
}

func TestAPipedPromptOneByteOverTheCapIsRefusedThroughParseRunArgs(t *testing.T) {
	body := strings.Repeat("a", maxPipedPromptBytes+1)
	_, err := parseRunArgs(newTestRegistry(t), nil, stdinFile(t, body))
	if err == nil {
		t.Fatal("parseRunArgs accepted a prompt one byte over the cap")
	}
	if got := statusFor(err, &strings.Builder{}); got != 1 {
		t.Fatalf("exit status = %d, want 1 (a failed input, like an unreadable image)", got)
	}
}
