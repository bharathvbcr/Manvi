package agent

import (
	"bytes"
	"image"
	"image/png"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func countWith(blocks ...llm.ContentBlock) int {
	return CountRequestTokens("", nil, []llm.Message{{Role: llm.RoleUser, Content: blocks}})
}

// An image must cost something wherever it sits. At zero, a screenshot-heavy
// session (every computer-use step returns one) never compacts and overflows.
func TestImagesArePriced(t *testing.T) {
	img := llm.ImageBlock{MediaType: "image/png", Data: pngOf(t, 1000, 1000)}
	base := countWith(llm.TextBlock{Text: "x"})
	if got := countWith(llm.TextBlock{Text: "x"}, img); got <= base {
		t.Fatalf("top-level image priced at 0 tokens: %d == %d", got, base)
	}
	result := func(inner ...llm.ContentBlock) llm.ContentBlock {
		return llm.ToolResultBlock{ToolCallID: "c1", Content: inner}
	}
	rb := countWith(result(llm.TextBlock{Text: "x"}))
	if got := countWith(result(llm.TextBlock{Text: "x"}, img)); got <= rb {
		t.Fatalf("image inside tool result priced at 0 tokens: %d vs base %d", got, rb)
	}
}

// Anthropic's documented rule: ceil(w/28) * ceil(h/28) visual tokens, capped at
// the high-resolution tier's 4784.
// https://platform.claude.com/docs/en/build-with-claude/vision#evaluate-image-size
func TestImageTokensFollowThePatchRule(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		want int
	}{
		{"200x200", pngOf(t, 200, 200), 64},
		{"1000x1000", pngOf(t, 1000, 1000), 1296},
		{"1092x1092", pngOf(t, 1092, 1092), 1521},
		{"1920x1080", pngOf(t, 1920, 1080), 2691},
		{"2000x1500", pngOf(t, 2000, 1500), 3888},
		{"3840x2160 is capped", pngOf(t, 3840, 2160), 4784},
		{"1x1", pngOf(t, 1, 1), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ImageTokens(llm.ImageBlock{MediaType: "image/png", Data: tc.data}); got != tc.want {
				t.Fatalf("ImageTokens = %d, want %d", got, tc.want)
			}
		})
	}
}

// What cannot be measured is priced at the cap, never at zero: this estimate
// guards a context window, so it may only be wrong in the direction that
// compacts early.
func TestUnreadableImagesArePricedAtTheCap(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":     nil,
		"garbage":   []byte("not an image"),
		"truncated": pngOf(t, 100, 100)[:12],
	} {
		if got := ImageTokens(llm.ImageBlock{MediaType: "image/png", Data: data}); got != maxImageTokens {
			t.Fatalf("%s: ImageTokens = %d, want %d", name, got, maxImageTokens)
		}
	}
}
