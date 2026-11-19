package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
)

func w23Registry(t *testing.T, name string, thirdParty bool, text string) *Registry {
	t.Helper()
	r := NewRegistry(bus.New())
	if err := r.Register(Tool{
		Schema:     namedSchema(name, "d"),
		Handler:    func(context.Context, Call) Result { return Result{Text: text} },
		ThirdParty: thirdParty,
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

// Text another program or party wrote is shown with its invisible characters
// made visible, at the one seam every result passes through.
func TestAThirdPartyResultIsEscapedAtTheSeam(t *testing.T) {
	const hostile = "see evil\u202egpj.exe and \x1b[2J"
	got := w23Registry(t, "mcp_x", true, hostile).Run(context.Background(), Call{ID: "1", Name: "mcp_x", Arguments: []byte(`{}`)})
	if strings.ContainsRune(got.Text, '\u202e') || strings.ContainsRune(got.Text, 0x1b) {
		t.Fatalf("a live override or escape sequence reached the result: %q", got.Text)
	}
	if !strings.Contains(got.Text, `\u202e`) || !strings.Contains(got.Text, `\x1b`) {
		t.Fatalf("the characters were dropped rather than shown: %q", got.Text)
	}
}

// And a tool that is not third-party is left exactly as it wrote itself: a file
// read legitimately returns what is in the file.
func TestAFirstPartyResultIsNotRewritten(t *testing.T) {
	const own = "a\x1b[0m\u200db"
	got := w23Registry(t, "read_thing", false, own).Run(context.Background(), Call{ID: "1", Name: "read_thing", Arguments: []byte(`{}`)})
	if got.Text != own {
		t.Fatalf("a first-party result was rewritten: %q", got.Text)
	}
}
