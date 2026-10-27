package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// countingTool registers a tool whose handler counts its runs, so a test can
// say "never reached the handler" rather than infer it from a result.
func countingTool(t *testing.T, reg *tools.Registry, tool tools.Tool) *atomic.Int32 {
	t.Helper()
	var ran atomic.Int32
	tool.Handler = func(context.Context, tools.Call) tools.Result {
		ran.Add(1)
		return tools.Result{Text: "ran " + tool.Schema.Name}
	}
	if err := reg.Register(tool); err != nil {
		t.Fatal(err)
	}
	return &ran
}

// resultFor is the tool result the model was shown for one call, read from the
// requests the provider actually received rather than from the log.
func resultFor(t *testing.T, h *harness, id string) llm.ToolResultBlock {
	t.Helper()
	for _, req := range h.provider.Requests() {
		for _, m := range req.Messages {
			for _, b := range m.Content {
				if r, ok := b.(llm.ToolResultBlock); ok && string(r.ToolCallID) == id {
					return r
				}
			}
		}
	}
	t.Fatalf("no request carried a result for call %q", id)
	return llm.ToolResultBlock{}
}

func resultText(r llm.ToolResultBlock) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tb, ok := c.(llm.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

// A chat-only run offers no tools, so a call a provider synthesized from prose
// must not reach a handler, and the model must be told why.
func TestChatOnlyRefusesAnEmittedToolCall(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("c1", "write_anything", `{}`),
		textTurn("done"),
	}, func(c *Config) { c.ChatOnly = true })
	ran := countingTool(t, h.tools, tools.Tool{Schema: llm.ToolSchema{Name: "write_anything"}})

	out, err := h.loop.Run(context.Background(), userMsg("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 0 {
		t.Fatalf("chat-only turn ran a tool the model was never offered (%d runs)", ran.Load())
	}
	if out.Steps != 2 || out.Final.Text() != "done" {
		t.Fatalf("the turn did not continue after the refusal: %+v", out)
	}
	for i, req := range h.provider.Requests() {
		if len(req.Tools) != 0 {
			t.Fatalf("request %d offered %d tools in a chat-only run", i, len(req.Tools))
		}
	}
	r := resultFor(t, h, "c1")
	if !r.IsError || !strings.Contains(resultText(r), "write_anything") ||
		!strings.Contains(resultText(r), "not offered") {
		t.Fatalf("the model was shown %+v, want an error naming the tool and saying it was not offered", r)
	}
}

// What a step offers is the filtered set, not the registry. A tool that exists
// but was filtered out of this step's request is refused all the same.
func TestAToolThatExistsButWasNotOfferedIsRefused(t *testing.T) {
	cases := []struct {
		name string
		cfg  func(*Config)
		tool tools.Tool
	}{
		{"read-only loop, mutating tool", func(c *Config) { c.ReadOnly = true },
			tools.Tool{Schema: llm.ToolSchema{Name: "victim"}}},
		{"core-only loop, extended tool", func(c *Config) { c.CoreToolsOnly = true },
			tools.Tool{Schema: llm.ToolSchema{Name: "victim"}, Extended: true, ReadOnly: true}},
		{"read-only and core-only, extended read-only tool", func(c *Config) { c.ReadOnly, c.CoreToolsOnly = true, true },
			tools.Tool{Schema: llm.ToolSchema{Name: "victim"}, Extended: true, ReadOnly: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := build(t, []replay.Turn{
				toolTurn("c1", "victim", `{}`),
				textTurn("done"),
			}, tc.cfg)
			ran := countingTool(t, h.tools, tc.tool)
			// A tool that is offered, so the request is not simply empty.
			countingTool(t, h.tools, tools.Tool{Schema: llm.ToolSchema{Name: "reader"}, ReadOnly: true})

			if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
				t.Fatal(err)
			}
			if ran.Load() != 0 {
				t.Fatalf("an unoffered tool reached its handler")
			}
			if r := resultFor(t, h, "c1"); !r.IsError || !strings.Contains(resultText(r), "not offered") {
				t.Fatalf("the model was shown %+v, want a not-offered error", r)
			}
		})
	}
}

// A name that is not in the registry at all is refused by the same check, with
// the same wording: the model's mistake is "not offered", whatever the registry
// would have said.
func TestAnUnregisteredNameIsRefusedAsNotOffered(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("c1", "invented", `{}`),
		textTurn("done"),
	}, nil)
	countingTool(t, h.tools, tools.Tool{Schema: llm.ToolSchema{Name: "reader"}})

	if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}
	if r := resultFor(t, h, "c1"); !r.IsError || !strings.Contains(resultText(r), "not offered") {
		t.Fatalf("the model was shown %+v", r)
	}
}

// The control: what the request offered still runs, including a tool with an
// MCP-style dynamic name.
func TestAnOfferedToolStillRuns(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("c1", "reader", `{}`),
		toolTurn("c2", "mcp__docs__lookup", `{}`),
		textTurn("done"),
	}, nil)
	reader := countingTool(t, h.tools, tools.Tool{Schema: llm.ToolSchema{Name: "reader"}, ReadOnly: true})
	mcp := countingTool(t, h.tools, tools.Tool{Schema: llm.ToolSchema{Name: "mcp__docs__lookup"}, Extended: true})

	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatal(err)
	}
	if reader.Load() != 1 || mcp.Load() != 1 || out.ToolCalls != 2 {
		t.Fatalf("reader ran %d, mcp ran %d, out.ToolCalls=%d; want 1, 1, 2", reader.Load(), mcp.Load(), out.ToolCalls)
	}
	if r := resultFor(t, h, "c2"); r.IsError {
		t.Fatalf("an offered call came back as an error: %+v", r)
	}
}

// A tool activated in one step is offered in the next, and runs there. Naming
// it in the same response that activates it is refused: that request never
// offered it, so the model cannot have been given its schema.
func TestActivatedToolRunsOnlyOnceItIsOffered(t *testing.T) {
	register := func(t *testing.T, h *harness) *atomic.Int32 {
		t.Helper()
		if err := h.tools.Register(tools.Tool{
			Schema: llm.ToolSchema{Name: "devcouncil_activate_tools"}, ReadOnly: true, Group: tools.GroupCore,
			Handler: func(context.Context, tools.Call) tools.Result {
				if _, err := h.tools.Activate("specialized"); err != nil {
					return tools.Errorf("activation failed: %v", err)
				}
				return tools.Result{Text: "activated"}
			},
		}); err != nil {
			t.Fatal(err)
		}
		ran := countingTool(t, h.tools, tools.Tool{
			Schema: llm.ToolSchema{Name: "specialized_tool"}, ReadOnly: true, Group: "specialized", Extended: true,
		})
		h.tools.EnableDynamic("devcouncil_activate_tools")
		return ran
	}

	t.Run("next step", func(t *testing.T) {
		h := build(t, []replay.Turn{
			toolTurn("c1", "devcouncil_activate_tools", `{}`),
			toolTurn("c2", "specialized_tool", `{}`),
			textTurn("done"),
		}, nil)
		ran := register(t, h)
		if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
			t.Fatal(err)
		}
		if ran.Load() != 1 {
			t.Fatalf("the activated tool ran %d times in the step after it was offered, want 1", ran.Load())
		}
		if r := resultFor(t, h, "c2"); r.IsError {
			t.Fatalf("the activated call came back as an error: %+v", r)
		}
	})

	t.Run("same response", func(t *testing.T) {
		h := build(t, []replay.Turn{
			twoCallTurn("devcouncil_activate_tools", `{}`, "specialized_tool", `{}`),
			textTurn("done"),
		}, nil)
		ran := register(t, h)
		if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
			t.Fatal(err)
		}
		if ran.Load() != 0 {
			t.Fatalf("a tool named in the response that activated it ran %d times", ran.Load())
		}
		if r := resultFor(t, h, "a"); r.IsError {
			t.Fatalf("the activation itself was refused: %+v", r)
		}
		if r := resultFor(t, h, "b"); !r.IsError || !strings.Contains(resultText(r), "not offered") {
			t.Fatalf("the model was shown %+v for the not-yet-offered call", r)
		}
	})
}

// The set that is enforced is the set that went out: a listener that narrows
// the request's tools narrows what may run, and the offered set is rebuilt each
// step, so a tool refused in one step is not remembered as offered in the next.
func TestTheOfferedSetIsTheOneThatWasSent(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("c1", "reader", `{}`),
		toolTurn("c2", "reader", `{"again":true}`),
		textTurn("done"),
	}, nil)
	ran := countingTool(t, h.tools, tools.Tool{Schema: llm.ToolSchema{Name: "reader"}, ReadOnly: true})

	var steps int
	if _, err := bus.OnWaterfall(h.bus, func(e LLMRequest, next bus.Next[LLMRequest]) LLMRequest {
		steps++
		if steps == 1 {
			e.Request.Tools = nil // the first request goes out with no tools at all
		}
		return next(e)
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 1 {
		t.Fatalf("reader ran %d times, want 1: refused in the step that stripped it, run in the step that offered it", ran.Load())
	}
	if r := resultFor(t, h, "c1"); !r.IsError {
		t.Fatalf("the call in the stripped step was not refused: %+v", r)
	}
	if r := resultFor(t, h, "c2"); r.IsError {
		t.Fatalf("the call in the offering step was refused: %+v", r)
	}
}
