package input

import (
	"io"
	"strings"
	"testing"
	"time"
)

// Input that arrives immediately before the stream ends must still be decoded.
//
// The reading goroutine writes the final chunk to `raw` and the error to
// `readErr` with nothing between them, so both are ready by the time the decode
// loop selects. select chooses uniformly among ready cases, so the error was
// taken first about half the time and the chunk still sitting in `raw` was
// dropped — the loop found nothing buffered, emitted EOF, and returned.
//
// On a terminal that is the last thing typed before the descriptor closes. It
// showed up as roughly one full-suite run in six decoding "ab\x1b[A" into
// nothing at all, which reads like a flaky test and is not one.
func TestInputArrivingJustBeforeEOFIsNotDropped(t *testing.T) {
	// Repeated because the bug is a scheduling coin flip: a single pass proves
	// nothing about a race that lost half its attempts.
	for i := 0; i < 300; i++ {
		r := NewReader(strings.NewReader("ab\x1b[A"))
		r.SetEscapeDelay(200 * time.Millisecond)
		go r.Run()

		var got []string
		for ev := range r.Events() {
			if k, ok := ev.(Key); ok {
				got = append(got, k.String())
				continue
			}
			if e, ok := ev.(Error); ok && e.Err == io.EOF {
				break
			}
		}
		want := []string{"a", "b", "up"}
		if len(got) != len(want) {
			t.Fatalf("iteration %d: got %v, want %v — input was dropped when the stream ended", i, got, want)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("iteration %d: got %v, want %v", i, got, want)
			}
		}
	}
}

// The same property with the bytes split across two writes, so the final chunk
// races EOF from a different starting state.
func TestASplitSequenceSurvivesTheStreamEnding(t *testing.T) {
	for i := 0; i < 200; i++ {
		r := NewReader(io.MultiReader(
			strings.NewReader("x"),
			strings.NewReader("\x1b[B"),
		))
		r.SetEscapeDelay(200 * time.Millisecond)
		go r.Run()

		var got []string
		for ev := range r.Events() {
			if k, ok := ev.(Key); ok {
				got = append(got, k.String())
				continue
			}
			if e, ok := ev.(Error); ok && e.Err == io.EOF {
				break
			}
		}
		if len(got) != 2 || got[0] != "x" || got[1] != "down" {
			t.Fatalf("iteration %d: got %v, want [x down]", i, got)
		}
	}
}
