package serve

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// runGit reports a cut listing as truncated rather than returning a prefix
// that reads as the whole answer; scanGit turns that into an error.
func TestRunGitReportsTruncation(t *testing.T) {
	dir := t.TempDir()
	out, truncated, err := runGit(context.Background(), dir, 5, "--version")
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(out) != 5 {
		t.Fatalf("out=%q truncated=%v, want 5 bytes and truncated", out, truncated)
	}
	out, truncated, err = runGit(context.Background(), dir, 4096, "--version")
	if err != nil || truncated || !bytes.HasPrefix(out, []byte("git version")) {
		t.Fatalf("out=%q truncated=%v err=%v, want the whole answer", out, truncated, err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("boom") }

func TestLimitedWriterBoundsAndPropagates(t *testing.T) {
	var buf bytes.Buffer
	w := &limitedWriter{w: &buf, left: 4}
	for _, chunk := range []string{"ab", "cdef", "gh"} {
		if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v; want the full length reported", chunk, n, err)
		}
	}
	if buf.String() != "abcd" {
		t.Fatalf("kept %q, want the first 4 bytes", buf.String())
	}
	if _, err := (&limitedWriter{w: failingWriter{}, left: 4}).Write([]byte("x")); err == nil {
		t.Fatal("an underlying write error was swallowed")
	}
}
