package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"manvi/core/bus"
	"manvi/llm"
)

// The scrubber is the backstop for credentials that reach a tool result by a
// route nobody designed — the case its own documentation names is a subprocess
// printing its own environment, which `run_command` does because it never sets
// cmd.Env.
//
// It was wired to the renderer and the JSON sink and nowhere else. Those are
// the display surfaces: a person watching a terminal saw redaction marks while
// the session file on disk and the next request body carried the key. This is
// the seam every result crosses, so it is the one the backstop belongs on.

const fakeKey = "sk-ant-api03-NOTAREALKEYAAAAAAAAAAAAAAAAAAAA"

func scrubbingRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry(bus.New())
	r.SetScrubber(func(s string) string {
		return strings.ReplaceAll(s, fakeKey, "[redacted]")
	})
	return r
}

// Every way a result can leave Run must be scrubbed, not just the happy one.
func TestEveryResultPathIsScrubbed(t *testing.T) {
	cases := []struct {
		name string
		tool Tool
	}{
		{
			name: "ordinary result",
			tool: Tool{
				Schema: leakySchema("prints its environment"),
				Handler: func(ctx context.Context, c Call) Result {
					return Result{Text: "ANTHROPIC_API_KEY=" + fakeKey}
				},
			},
		},
		{
			name: "error result",
			tool: Tool{
				Schema: leakySchema("fails loudly"),
				Handler: func(ctx context.Context, c Call) Result {
					return Result{Text: "request failed with " + fakeKey, IsError: true}
				},
			},
		},
		{
			name: "panic recovered into a refusal",
			tool: Tool{
				Schema: leakySchema("panics"),
				Handler: func(ctx context.Context, c Call) Result {
					panic(errors.New("boom while holding " + fakeKey))
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := scrubbingRegistry(t)
			if err := r.Register(tc.tool); err != nil {
				t.Fatal(err)
			}
			res := r.Run(context.Background(), Call{ID: "1", Name: "leaky", Arguments: []byte(`{}`)})
			if strings.Contains(res.Text, fakeKey) {
				t.Fatalf("the credential survived into the tool result: %q", res.Text)
			}
			if !strings.Contains(res.Text, "[redacted]") && tc.name != "panic recovered into a refusal" {
				t.Errorf("expected a redaction mark in %q", res.Text)
			}
		})
	}
}

// A registry with no scrubber installed must still work — that is what tests
// and any consumer without a composition root get.
func TestAnUnarmedRegistryIsUnchanged(t *testing.T) {
	r := NewRegistry(bus.New())
	if err := r.Register(Tool{
		Schema:  namedSchema("plain", "says hello"),
		Handler: func(ctx context.Context, c Call) Result { return Result{Text: "hello"} },
	}); err != nil {
		t.Fatal(err)
	}
	if res := r.Run(context.Background(), Call{ID: "1", Name: "plain", Arguments: []byte(`{}`)}); res.Text != "hello" {
		t.Fatalf("got %q, want %q", res.Text, "hello")
	}
}

// Short values are not watched by the real scrubber, and the seam must not
// invent redactions of its own: a tool result that merely resembles a key is
// left alone.
func TestScrubbingOnlyRemovesWhatIsWatched(t *testing.T) {
	r := scrubbingRegistry(t)
	if err := r.Register(Tool{
		Schema: namedSchema("plain", "prints an unrelated token"),
		Handler: func(ctx context.Context, c Call) Result {
			return Result{Text: "sk-ant-api03-SOMETHINGELSEENTIRELY"}
		},
	}); err != nil {
		t.Fatal(err)
	}
	res := r.Run(context.Background(), Call{ID: "1", Name: "plain", Arguments: []byte(`{}`)})
	if strings.Contains(res.Text, "[redacted]") {
		t.Fatalf("an unwatched value was redacted: %q", res.Text)
	}
}

func leakySchema(desc string) llm.ToolSchema { return namedSchema("leaky", desc) }

func namedSchema(name, desc string) llm.ToolSchema {
	return llm.ToolSchema{
		Name:        name,
		Description: desc,
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}
}
