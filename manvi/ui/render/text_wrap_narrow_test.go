package render

import (
	"strings"
	"testing"
	"time"
)

// A character wider than the line used to hang Wrap: the hard break cut it to
// an empty head, consumed nothing, and looped forever. Every input must end,
// and no text may be lost on the way.
func TestWrapTerminatesWhenACharacterIsWiderThanTheLine(t *testing.T) {
	for _, tc := range []struct {
		text  string
		width int
	}{
		{"日本", 1},
		{"a日b本c", 1},
		{"日本語のテキスト", 1},
		{"x 日 y", 1},
		{"👩‍👩‍👧 family", 1},
		{"日本", 2},
	} {
		done := make(chan []Line, 1)
		go func() { done <- Styled(tc.text, Style{}).Wrap(tc.width) }()
		select {
		case lines := <-done:
			var got strings.Builder
			for _, l := range lines {
				got.WriteString(l.Text())
			}
			if strip(got.String()) != strip(tc.text) {
				t.Errorf("Wrap(%q, %d) lost text: %q", tc.text, tc.width, got.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Wrap(%q, %d) did not return", tc.text, tc.width)
		}
	}
}

func strip(s string) string { return strings.ReplaceAll(s, " ", "") }
