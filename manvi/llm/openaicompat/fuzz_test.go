package openaicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"manvi/llm"
)

// fuzzTools is the shape the recovery path reads types from. Two tools with
// differently-typed parameters, because the XML form carries no types and the
// schema is the only thing that stops "0755" becoming 755.
var fuzzTools = []llm.ToolSchema{
	{
		Name:        "write_file",
		Description: "write a file",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"mode":{"type":"integer"},"overwrite":{"type":"boolean"}}}`),
	},
	{
		Name:        "run_command",
		Description: "run a command",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string"},"timeout":{"type":"number"},"args":{"type":"array"}}}`),
	},
}

// FuzzExtractFallbackToolCallsHoldsItsContract fuzzes the recovery parsers that
// read tool calls out of message text when the server did not parse them.
//
// Three invariants, and all three are load-bearing downstream:
//
//   - It never panics. This is hand-written scanning over model output, which
//     is the least trustworthy input in the system.
//   - Arguments are always valid JSON when a call is returned. The tools
//     pipeline unmarshals them without a second guard; a call that parsed into
//     invalid JSON would fail far from here, with a message naming the tool
//     rather than the parse.
//   - A returned call always has a name. A nameless call cannot be dispatched,
//     and returning one converts a recoverable text response into a dispatch
//     error.
func FuzzExtractFallbackToolCallsHoldsItsContract(f *testing.F) {
	seeds := []string{
		"",
		"plain text with no calls at all",
		`<tool_call>{"name":"write_file","arguments":{"path":"a.go"}}</tool_call>`,
		`<tool_call>{"name":"write_file","arguments":{"path":"a.go","mode":"0755"}}</tool_call>`,
		"<tool_call><function=write_file><parameter=path>a.go</parameter><parameter=mode>0755</parameter></function></tool_call>",
		"<tool_call><function=run_command><parameter=cmd>ls</parameter><parameter=timeout>1.5</parameter></function></tool_call>",
		"```json\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"a.go\"}}\n```",
		"<tool_call>",
		"</tool_call>",
		"<tool_call></tool_call>",
		"<tool_call><function=></function></tool_call>",
		"<tool_call><function=write_file><parameter=></parameter></function></tool_call>",
		`<tool_call>{"name":"nonexistent_tool","arguments":{}}</tool_call>`,
		`<tool_call>{"name":"write_file"}</tool_call>`,
		`<tool_call>{"arguments":{"path":"a"}}</tool_call>`,
		"<tool_call>not json at all</tool_call>",
		"<tool_call>{\"name\":\"write_file\",\"arguments\":{\"path\":\"a\"}}</tool_call><tool_call>{\"name\":\"run_command\",\"arguments\":{\"cmd\":\"ls\"}}</tool_call>",
		"```json\n[{\"name\":\"write_file\",\"arguments\":{}}]\n```",
		"<function=write_file>",
		strings.Repeat("<tool_call>", 200),
		strings.Repeat("<parameter=x>", 200),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		cleaned, blocks, format := extractFallbackToolCalls(raw, fuzzTools)

		if len(blocks) == 0 {
			if format != FallbackNone {
				t.Fatalf("format %v reported with no calls recovered", format)
			}
			return
		}
		if format == FallbackNone {
			t.Fatalf("%d calls recovered but format is FallbackNone", len(blocks))
		}
		for i, b := range blocks {
			if b.Name == "" {
				t.Fatalf("block %d has no name; it cannot be dispatched (input %q)", i, clipFuzz(raw))
			}
			if b.ID == "" {
				t.Fatalf("block %d has no call id; a tool result could not be paired to it", i)
			}
			if len(b.Arguments) == 0 {
				t.Fatalf("block %d (%s) has empty arguments; the pipeline unmarshals this", i, b.Name)
			}
			if !json.Valid(b.Arguments) {
				t.Fatalf("block %d (%s) produced invalid JSON arguments: %q", i, b.Name, clipFuzz(string(b.Arguments)))
			}
			// Arguments must be a JSON object: the dispatcher unmarshals into
			// a map, and an array or scalar would fail there instead of here.
			var into map[string]any
			if err := json.Unmarshal(b.Arguments, &into); err != nil {
				t.Fatalf("block %d (%s) arguments are not a JSON object: %q", i, b.Name, clipFuzz(string(b.Arguments)))
			}
		}
		// Recovery removes what it consumed; it must never grow the text it
		// was asked to clean.
		if len(cleaned) > len(raw) {
			t.Fatalf("cleaned text grew from %d to %d bytes", len(raw), len(cleaned))
		}
	})
}

func clipFuzz(s string) string {
	if len(s) <= 200 {
		return s
	}
	return s[:200] + "..."
}
