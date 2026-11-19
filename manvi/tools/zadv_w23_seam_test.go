package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
)

func zadvSeamRegistry(t *testing.T, name string, thirdParty bool, h Handler) *Registry {
	t.Helper()
	r := NewRegistry(bus.New())
	if err := r.Register(Tool{Schema: namedSchema(name, "d"), Handler: h, ThirdParty: thirdParty}); err != nil {
		t.Fatal(err)
	}
	return r
}

func zadvRun(r *Registry, name string) Result {
	return r.Run(context.Background(), Call{ID: "1", Name: name, Arguments: []byte(`{}`)})
}

func zadvLive(s string) bool {
	return strings.ContainsRune(s, '\u202e') || strings.ContainsRune(s, 0x1b) || strings.ContainsRune(s, '\u200b') ||
		strings.ContainsRune(s, '\U000E0041') || strings.ContainsRune(s, '\u2066')
}

const zadvHostile = "a\u202eb\x1b[2Jc\u200bd\U000E0041e\u2066f"

func TestZadvW23ThirdPartyContentBlocksAreEscaped(t *testing.T) {
	r := zadvSeamRegistry(t, "mcp_x", true, func(context.Context, Call) Result {
		return Result{Content: llm.Content{llm.TextBlock{Text: zadvHostile}}}
	})
	got := zadvRun(r, "mcp_x")
	if len(got.Content) != 1 {
		t.Fatalf("content = %+v", got.Content)
	}
	block, ok := got.Content[0].(llm.TextBlock)
	if !ok {
		t.Fatalf("content[0] is %T, want a text block", got.Content[0])
	}
	if zadvLive(block.Text) {
		t.Fatalf("a live character reached a content block: %q", block.Text)
	}
}

func TestZadvW23ThirdPartyErrorAndPanicTextIsEscaped(t *testing.T) {
	t.Run("error result", func(t *testing.T) {
		r := zadvSeamRegistry(t, "mcp_x", true, func(context.Context, Call) Result {
			return Result{Text: zadvHostile, IsError: true}
		})
		if got := zadvRun(r, "mcp_x"); zadvLive(got.Text) {
			t.Fatalf("%q", got.Text)
		}
	})
	t.Run("panic carrying third-party text", func(t *testing.T) {
		r := zadvSeamRegistry(t, "mcp_x", true, func(context.Context, Call) Result {
			panic(zadvHostile)
		})
		got := zadvRun(r, "mcp_x")
		if zadvLive(got.Text) {
			t.Fatalf("a live character reached a panic result's text: %q", got.Text)
		}
		for _, d := range got.Degraded {
			if zadvLive(d) {
				t.Fatalf("a live character reached Degraded: %q", d)
			}
		}
	})
}

func TestZadvW23EscapingIsNotDoneTwice(t *testing.T) {
	const already = "ran \\x1b[0m and \\u202e"
	r := zadvSeamRegistry(t, "mcp_x", true, func(context.Context, Call) Result { return Result{Text: already} })
	if got := zadvRun(r, "mcp_x"); got.Text != already {
		t.Fatalf("text that holds no live character was changed: %q", got.Text)
	}
}

// The scrubber sees what a reader will see: a credential split by a zero-width
// joiner is a credential once the joiner is shown, and is still scrubbed.
func TestZadvW23AJoinerSplitCredentialIsStillScrubbed(t *testing.T) {
	r := zadvSeamRegistry(t, "mcp_x", true, func(context.Context, Call) Result {
		return Result{Text: "token sk-live-12\u200d3456789012345678 end"}
	})
	r.SetScrubber(func(s string) string { return strings.ReplaceAll(s, "sk-live-12\\u200d3456789012345678", "[scrubbed]") })
	got := zadvRun(r, "mcp_x")
	if strings.Contains(got.Text, "sk-live") {
		t.Fatalf("the split credential survived: %q", got.Text)
	}
}
